package reminder

import (
	"context"
	"fmt"
	"net/http"

	"github.com/line/line-bot-sdk-go/v8/linebot/messaging_api"
)

// LinePusher ส่ง reminder ด้วย LINE push message (นับโควตาของ LINE OA ต่างจาก reply ที่ฟรี)
type LinePusher struct {
	bot *messaging_api.MessagingApiAPI
}

func NewLinePusher(bot *messaging_api.MessagingApiAPI) *LinePusher {
	return &LinePusher{bot: bot}
}

// Push ส่งข้อความ; retryKey (UUID เช่น note id) ทำให้ LINE ไม่ส่งซ้ำถ้าเราลองส่งใหม่
func (p *LinePusher) Push(_ context.Context, lineUserID, retryKey, text string) error {
	res, _, err := p.bot.PushMessageWithHttpInfo(&messaging_api.PushMessageRequest{
		To:       lineUserID,
		Messages: []messaging_api.MessageInterface{&messaging_api.TextMessage{Text: text}},
	}, retryKey)
	status := 0
	if res != nil {
		status = res.StatusCode
	}
	return classifyPush(status, err)
}

// classifyPush แปลงผลการส่งเป็น error ที่ scheduler ใช้ตัดสินใจลองใหม่
func classifyPush(status int, err error) error {
	switch {
	case err == nil:
		return nil
	case status == http.StatusConflict:
		return nil // retry key เดิมถูกส่งไปแล้ว = ผู้ใช้ได้ข้อความแล้ว
	case status == 0, status == http.StatusTooManyRequests, status >= 500:
		return &PushError{Retry: true, Err: err} // เน็ต/LINE ล่ม/เกิน rate หรือโควตา
	default:
		return &PushError{Retry: false, Err: fmt.Errorf("line push %d: %w", status, err)} // เช่น ผู้ใช้บล็อกบอท
	}
}
