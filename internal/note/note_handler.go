package note

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/line/line-bot-sdk-go/v8/linebot/messaging_api"
	"github.com/line/line-bot-sdk-go/v8/linebot/webhook"
)

type Handler struct {
	service       *Service
	bot           *messaging_api.MessagingApiAPI
	channelSecret string
}

func NewHandler(service *Service, bot *messaging_api.MessagingApiAPI, channelSecret string) *Handler {
	return &Handler{
		service:       service,
		bot:           bot,
		channelSecret: channelSecret,
	}
}

func (h *Handler) HandleWebhook(c *gin.Context) {
	cb, err := webhook.ParseRequest(h.channelSecret, c.Request)
	if err != nil {
		log.Println("parse/verify ล้มเหลว:", err)
		c.Status(http.StatusBadRequest)
		return
	}

	for _, event := range cb.Events {
		h.handleEvent(c.Request.Context(), event)
	}

	c.Status(http.StatusOK)
}

func (h *Handler) handleEvent(ctx context.Context, event webhook.EventInterface) {
	switch e := event.(type) {
	case webhook.MessageEvent:
		h.handleMessageEvent(ctx, e)
	case webhook.PostbackEvent:
		h.handlePostbackEvent(ctx, e)
	}
}

func (h *Handler) handleMessageEvent(ctx context.Context, msgEvent webhook.MessageEvent) {
	textMsg, ok := msgEvent.Message.(webhook.TextMessageContent)
	if !ok {
		return
	}

	source, ok := msgEvent.Source.(webhook.UserSource)
	if !ok {
		log.Println("event ไม่ได้มาจาก user เดี่ยว ข้าม")
		return
	}

	if msgEvent.DeliveryContext != nil && msgEvent.DeliveryContext.IsRedelivery {
		log.Println("ข้าม redelivered event:", msgEvent.WebhookEventId)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := h.service.CaptureFromLine(ctx, source.UserId, "", textMsg.Text)
	if err != nil {
		log.Println("capture note ล้มเหลว:", err)
		h.reply(ctx, msgEvent.ReplyToken, "ขอโทษครับ ทำรายการไม่สำเร็จ ลองอีกครั้งนะครับ")
		return
	}

	if result.Action == "item_choice" {
		h.replyWithChoices(ctx, msgEvent.ReplyToken, result)
		return
	}

	h.reply(ctx, msgEvent.ReplyToken, replyTextFor(result))
}

func (h *Handler) handlePostbackEvent(ctx context.Context, e webhook.PostbackEvent) {
	source, ok := e.Source.(webhook.UserSource)
	if !ok {
		log.Println("postback ไม่ได้มาจาก user เดี่ยว ข้าม")
		return
	}

	if e.DeliveryContext != nil && e.DeliveryContext.IsRedelivery {
		log.Println("ข้าม redelivered postback:", e.WebhookEventId)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	values, err := url.ParseQuery(e.Postback.Data)
	if err != nil {
		log.Println("parse postback data ล้มเหลว:", err)
		return
	}

	itemID := values.Get("item_id")
	done := values.Get("done") == "true"
	if itemID == "" {
		log.Println("postback data ไม่มี item_id")
		return
	}

	_, err = h.service.ToggleItemFromPostback(ctx, source.UserId, itemID, done)
	if err != nil {
		log.Println("toggle from postback ล้มเหลว:", err)
		h.reply(ctx, e.ReplyToken, "ขอโทษครับ ทำรายการไม่สำเร็จ ลองอีกครั้งนะครับ")
		return
	}

	confirmText := "เสร็จแล้ว ✅"
	if !done {
		confirmText = "ยกเลิกติ๊กแล้วครับ"
	}
	h.reply(ctx, e.ReplyToken, confirmText)
}

func (h *Handler) replyWithChoices(ctx context.Context, replyToken string, result *CommandResult) {
	items := make([]messaging_api.QuickReplyItem, 0, len(result.ItemMatches))
	for _, m := range result.ItemMatches {
		label := quickReplyLabel(m)
		data := url.Values{
			"item_id": {m.Item.ID},
			"done":    {strconv.FormatBool(result.ChoiceDone)},
		}.Encode()

		items = append(items, messaging_api.QuickReplyItem{
			Type: "action",
			Action: &messaging_api.PostbackAction{
				Label:       label,
				Data:        data,
				DisplayText: label,
			},
		})
	}

	verb := "เสร็จ"
	if !result.ChoiceDone {
		verb = "ยกเลิก"
	}

	_, err := h.bot.ReplyMessage(&messaging_api.ReplyMessageRequest{
		ReplyToken: replyToken,
		Messages: []messaging_api.MessageInterface{
			&messaging_api.TextMessage{
				Text:       fmt.Sprintf("เจอหลายรายการที่คล้ายกันครับ เลือกอันที่จะทำ %s เลย 👇", verb),
				QuickReply: &messaging_api.QuickReply{Items: items},
			},
		},
	})
	if err != nil {
		log.Println("reply quick reply ล้มเหลว:", err)
	}
}

// quickReplyLabel สร้าง label ปุ่ม ไม่เกิน 20 ตัวอักษรตามข้อจำกัดของ LINE
// รูปแบบ "<ชื่อรายการ>·<วันที่ย่อ>" เช่น "น้ำหกแพ็ค·25/8"
func quickReplyLabel(m *ItemMatch) string {
	date := m.NoteCreated.In(bangkok).Format("2/1")
	dateSuffix := "·" + date

	textRunes := []rune(m.Item.Text)
	maxTextLen := 20 - len([]rune(dateSuffix))
	if maxTextLen < 1 {
		maxTextLen = 1
	}
	if len(textRunes) > maxTextLen {
		textRunes = textRunes[:maxTextLen]
	}
	return string(textRunes) + dateSuffix
}

func replyTextFor(result *CommandResult) string {
	switch result.Action {
	case "deleted":
		return "ลบแล้ว 🗑️"
	case "updated":
		return "แก้ไขแล้ว ✏️"
	case "item_toggled":
		if result.Message == "" {
			return "เรียบร้อยแล้ว ✅"
		}
		return fmt.Sprintf("เสร็จแล้ว ✅ %s", result.Message)
	case "invalid_date":
		return "รูปแบบวันที่ไม่ถูกต้องครับ ลองแบบ 25/08 หรือ 25/08/2026"
	case "invalid_content":
		return result.Message
	case "listed":
		return formatNoteList(result.Notes, result.NoteItems, formatDateHeader(result.Date))
	case "searched":
		return formatNoteList(result.Notes, result.NoteItems, fmt.Sprintf(`ผลค้นหา "%s"`, result.Query))
	case "created":
		if result.Note != nil && result.Note.Type == NoteTypeChecklist {
			return "บันทึกลิสต์แล้ว ✅"
		}
		return "จดแล้ว ✅"
	default:
		return "เรียบร้อยครับ"
	}
}

func formatDateHeader(date time.Time) string {
	return fmt.Sprintf("วันที่ %s", date.Format("02/01/2006"))
}

func formatNoteList(notes []*Note, itemsByNote map[string][]*NoteItem, header string) string {
	if len(notes) == 0 {
		return fmt.Sprintf("%s ไม่พบโน้ตเลยครับ", header)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s (%d รายการ)\n\n", header, len(notes))

	for i, n := range notes {
		content := ""
		if n.Content != nil {
			content = *n.Content
		}

		marker := "📝"
		switch n.Type {
		case NoteTypeChecklist:
			marker = "📋"
		case NoteTypeTodo:
			marker = "✅"
			if n.IsDone {
				marker = "✔️"
			}
		}

		fmt.Fprintf(&b, "%d. %s %s\n", i+1, marker, content)

		if n.Type == NoteTypeChecklist {
			for j, item := range itemsByNote[n.ID] {
				box := "☐"
				if item.IsDone {
					box = "☑"
				}
				fmt.Fprintf(&b, "    %d) %s %s\n", j+1, box, item.Text)
			}
		}
	}
	return b.String()
}

func (h *Handler) reply(ctx context.Context, replyToken, text string) {
	_, err := h.bot.ReplyMessage(
		&messaging_api.ReplyMessageRequest{
			ReplyToken: replyToken,
			Messages: []messaging_api.MessageInterface{
				&messaging_api.TextMessage{Text: text},
			},
		},
	)
	if err != nil {
		log.Println("reply ล้มเหลว:", err)
	}
}