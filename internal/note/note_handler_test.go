package note

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/line/line-bot-sdk-go/v8/linebot/webhook"
)

func TestWebhookEventID(t *testing.T) {
	cases := []struct {
		name  string
		event webhook.EventInterface
		want  string
	}{
		{"message", webhook.MessageEvent{WebhookEventId: "msg-1"}, "msg-1"},
		{"postback", webhook.PostbackEvent{WebhookEventId: "pb-1"}, "pb-1"},
		{"other", webhook.FollowEvent{WebhookEventId: "follow-1"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := webhookEventID(tc.event); got != tc.want {
				t.Fatalf("webhookEventID() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWaitReturnsWhenWorkFinishes(t *testing.T) {
	h := NewHandler(nil, nil, "", nil)
	h.wg.Add(1)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := h.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait() with pending work = %v, want DeadlineExceeded", err)
	}

	h.wg.Done()
	if err := h.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() after work finished = %v, want nil", err)
	}
}

// service เป็น nil ทำให้ handleEvent panic ตอน dedupe — processEvents ต้อง recover,
// ปล่อย semaphore และเรียก wg.Done ให้ Wait กลับมาได้
func TestProcessEventsRecoversFromPanic(t *testing.T) {
	h := NewHandler(nil, nil, "", nil)
	h.wg.Add(1)
	h.processEvents([]webhook.EventInterface{
		webhook.MessageEvent{WebhookEventId: "boom-1"},
		webhook.MessageEvent{WebhookEventId: "boom-2"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.Wait(ctx); err != nil {
		t.Fatalf("Wait() after panic = %v, want nil", err)
	}
	if n := len(h.sem); n != 0 {
		t.Fatalf("semaphore still holds %d slots", n)
	}
}

func TestUndoPostbackData(t *testing.T) {
	const id = "6f1c2b9e-1d3a-4c5b-9e8f-0a1b2c3d4e5f"
	data := undoPostbackData(id)
	if len(data) > 300 {
		t.Fatalf("postback data is %d chars, LINE allows 300", len(data))
	}
	v, err := url.ParseQuery(data)
	if err != nil || v.Get("action") != postbackUndo || v.Get("id") != id {
		t.Fatalf("round trip = %v, %v", v, err)
	}
	if n := len([]rune(undoLabel)); n > 20 {
		t.Errorf("quick reply label is %d chars, LINE allows 20", n)
	}
}
