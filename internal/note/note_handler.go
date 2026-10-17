package note

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/line/line-bot-sdk-go/v8/linebot/messaging_api"
	"github.com/line/line-bot-sdk-go/v8/linebot/webhook"
)

const (
	// จำนวน webhook callback ที่ประมวลผลพร้อมกันได้สูงสุด
	maxConcurrentCallbacks = 16

	// เผื่อเวลาให้ AI (หลายรอบ) + fallback ไปทาง rule-based ถ้า AI ล้ม
	eventTimeout  = 30 * time.Second
	dedupeTimeout = 3 * time.Second

	// AI ต้องเสร็จภายในเวลานี้ เหลือเวลาใน eventTimeout ให้ fallback ตอบได้ทัน
	aiTimeout = 20 * time.Second
	// LINE รับค่า 5–60 วินาที (ทีละ 5); แสดงจุดสามจุดระหว่างรอ AI
	loadingSeconds = 20
)

// Assistant คือ AI ที่แปลงข้อความอิสระเป็นการกระทำ (implement โดย internal/assistant)
// แยกเป็น interface ที่นี่เพื่อไม่ให้ package note import assistant (assistant import note อยู่แล้ว)
type Assistant interface {
	// HandleLine คืนข้อความตอบกลับ; error = ไม่ได้เขียนข้อมูลใดเลย ผู้เรียก fallback ได้
	HandleLine(ctx context.Context, userID, webhookEventID, text string) (string, error)
}

type Handler struct {
	service       *Service
	bot           *messaging_api.MessagingApiAPI
	channelSecret string
	ai            Assistant // nil = ไม่ใช้ AI ทุกข้อความไปทาง rule-based

	wg  sync.WaitGroup
	sem chan struct{}
}

func NewHandler(service *Service, bot *messaging_api.MessagingApiAPI, channelSecret string, ai Assistant) *Handler {
	return &Handler{
		service:       service,
		bot:           bot,
		channelSecret: channelSecret,
		ai:            ai,
		sem:           make(chan struct{}, maxConcurrentCallbacks),
	}
}

// HandleWebhook ตรวจ signature แล้วตอบ 200 ทันที ส่วนการประมวลผล event ทำใน goroutine
// เพื่อไม่ให้ LINE timeout/redeliver เวลางานช้า (เช่นเรียก AI)
func (h *Handler) HandleWebhook(c *gin.Context) {
	cb, err := webhook.ParseRequest(h.channelSecret, c.Request)
	if err != nil {
		log.Println("parse/verify ล้มเหลว:", err)
		c.Status(http.StatusBadRequest)
		return
	}

	if len(cb.Events) > 0 {
		h.wg.Add(1)
		go h.processEvents(cb.Events)
	}

	c.Status(http.StatusOK)
}

// Wait รอให้ event ที่กำลังประมวลผลอยู่เสร็จ หรือจน ctx หมดเวลา
// ต้องเรียกหลัง http.Server.Shutdown แล้ว (ไม่มี request ใหม่เข้ามา Add อีก)
func (h *Handler) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// processEvents ประมวลผล event ใน callback เดียวกันตามลำดับ
// (เช่น "ซื้อนม" ตามด้วย "นม เสร็จ" ต้องไม่สลับกัน)
func (h *Handler) processEvents(events []webhook.EventInterface) {
	defer h.wg.Done()

	h.sem <- struct{}{}
	defer func() { <-h.sem }()

	for _, event := range events {
		h.safeHandleEvent(event)
	}
}

func (h *Handler) safeHandleEvent(event webhook.EventInterface) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic ระหว่างประมวลผล event: %v\n%s", r, debug.Stack())
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), eventTimeout)
	defer cancel()

	h.handleEvent(ctx, event)
}

func (h *Handler) handleEvent(ctx context.Context, event webhook.EventInterface) {
	if id := webhookEventID(event); id != "" && !h.markNew(ctx, id) {
		log.Println("ข้าม event ที่เคยประมวลผลแล้ว:", id)
		return
	}

	switch e := event.(type) {
	case webhook.MessageEvent:
		h.handleMessageEvent(ctx, e)
	case webhook.PostbackEvent:
		h.handlePostbackEvent(ctx, e)
	}
}

// markNew คืน true ถ้า event นี้ควรถูกประมวลผล
// ถ้า DB สำหรับ dedupe มีปัญหา ให้ประมวลผลต่อ (ยอมซ้ำดีกว่าหายเงียบ)
func (h *Handler) markNew(ctx context.Context, webhookEventID string) bool {
	ctx, cancel := context.WithTimeout(ctx, dedupeTimeout)
	defer cancel()

	isNew, err := h.service.MarkEventProcessed(ctx, webhookEventID)
	if err != nil {
		log.Println("dedupe webhook event ล้มเหลว:", err)
		return true
	}
	return isNew
}

func webhookEventID(event webhook.EventInterface) string {
	switch e := event.(type) {
	case webhook.MessageEvent:
		return e.WebhookEventId
	case webhook.PostbackEvent:
		return e.WebhookEventId
	}
	return ""
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

	// คำสั่งแบบเดิม (ดู/ลบ/เสร็จ 2/หา ...) ตอบทันทีไม่ต้องใช้ AI; ข้อความอื่นให้ AI ลองก่อน
	if h.ai != nil && !IsCommand(textMsg.Text) &&
		h.handleWithAI(ctx, source.UserId, msgEvent.WebhookEventId, msgEvent.ReplyToken, textMsg.Text) {
		return
	}

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

// handleWithAI ส่งข้อความให้ AI แล้วตอบกลับ คืน false ถ้า AI ใช้ไม่ได้และยังไม่ได้เขียนข้อมูล
// (เช่น Ollama ไม่ได้เปิด) ให้ผู้เรียก fallback ไปทาง rule-based เพื่อไม่ให้ข้อความหาย
func (h *Handler) handleWithAI(ctx context.Context, lineUserID, webhookEventID, replyToken, text string) bool {
	userID, err := h.service.ResolveLineUser(ctx, lineUserID)
	if err != nil {
		log.Println("resolve user สำหรับ AI ล้มเหลว:", err)
		return false
	}

	h.showLoading(lineUserID)

	aiCtx, cancel := context.WithTimeout(ctx, aiTimeout)
	defer cancel()
	reply, err := h.ai.HandleLine(aiCtx, userID, webhookEventID, text)
	if err != nil {
		log.Println("AI ล้มเหลว ใช้ rule-based แทน:", err)
		return false
	}

	h.reply(ctx, replyToken, reply)
	return true
}

// showLoading แสดงจุดสามจุดในแชทระหว่างรอ AI (ล้มเหลวก็ไม่เป็นไร)
func (h *Handler) showLoading(lineUserID string) {
	if h.bot == nil {
		return
	}
	_, err := h.bot.ShowLoadingAnimation(&messaging_api.ShowLoadingAnimationRequest{
		ChatId:         lineUserID,
		LoadingSeconds: loadingSeconds,
	})
	if err != nil {
		log.Println("show loading animation ล้มเหลว:", err)
	}
}

func (h *Handler) handlePostbackEvent(ctx context.Context, e webhook.PostbackEvent) {
	source, ok := e.Source.(webhook.UserSource)
	if !ok {
		log.Println("postback ไม่ได้มาจาก user เดี่ยว ข้าม")
		return
	}

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