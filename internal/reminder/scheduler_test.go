package reminder

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"jodjod/internal/note"
)

type fakeStore struct {
	queue    []note.DueReminder
	released []string
	claims   int
}

func (s *fakeStore) ClaimDueReminders(_ context.Context, _ time.Time, limit int) ([]note.DueReminder, error) {
	s.claims++
	n := min(limit, len(s.queue))
	out := s.queue[:n]
	s.queue = s.queue[n:]
	return out, nil
}

func (s *fakeStore) ReleaseReminder(_ context.Context, noteID string) error {
	s.released = append(s.released, noteID)
	return nil
}

type push struct{ to, key, text string }

type fakePusher struct {
	sent []push
	errs map[string]error // note id -> error
}

func (p *fakePusher) Push(_ context.Context, to, key, text string) error {
	if err := p.errs[key]; err != nil {
		return err
	}
	p.sent = append(p.sent, push{to, key, text})
	return nil
}

var testNow = time.Date(2026, 10, 24, 9, 0, 30, 0, time.UTC)

func newTestScheduler(store *fakeStore, pusher *fakePusher) *Scheduler {
	s := New(store, pusher)
	s.now = func() time.Time { return testNow }
	return s
}

func TestTickSendsDueReminders(t *testing.T) {
	store := &fakeStore{queue: []note.DueReminder{
		{NoteID: "on-time", LineUserID: "U1", Content: "โทรหาหมอ", RemindAt: testNow.Add(-30 * time.Second)},
		{NoteID: "late", LineUserID: "U1", Content: "จ่ายค่าเน็ต", RemindAt: testNow.Add(-2 * time.Hour)},
		{NoteID: "no-line", LineUserID: "", Content: "x", RemindAt: testNow},
		{NoteID: "stale", LineUserID: "U1", Content: "y", RemindAt: testNow.Add(-maxLate - time.Hour)},
	}}
	pusher := &fakePusher{}
	newTestScheduler(store, pusher).Tick(context.Background())

	if len(pusher.sent) != 2 {
		t.Fatalf("sent %d pushes, want on-time and late only: %+v", len(pusher.sent), pusher.sent)
	}
	if p := pusher.sent[0]; p.to != "U1" || p.key != "on-time" || p.text != "⏰ เตือนครับ: โทรหาหมอ" {
		t.Errorf("on-time push = %+v", p)
	}
	if p := pusher.sent[1]; !strings.Contains(p.text, "จ่ายค่าเน็ต") || !strings.Contains(p.text, "ส่งช้าไปหน่อย") {
		t.Errorf("late push should say it is late: %q", p.text)
	}
	if len(store.released) != 0 {
		t.Errorf("released %v, want none", store.released)
	}
}

func TestTickReleasesOnlyRetryableFailures(t *testing.T) {
	store := &fakeStore{queue: []note.DueReminder{
		{NoteID: "flaky", LineUserID: "U1", Content: "a", RemindAt: testNow},
		{NoteID: "blocked", LineUserID: "U2", Content: "b", RemindAt: testNow},
	}}
	pusher := &fakePusher{errs: map[string]error{
		"flaky":   &PushError{Retry: true, Err: errors.New("503")},
		"blocked": &PushError{Retry: false, Err: errors.New("403")},
	}}
	newTestScheduler(store, pusher).Tick(context.Background())

	if len(store.released) != 1 || store.released[0] != "flaky" {
		t.Errorf("released %v, want only the retryable one", store.released)
	}
}

func TestTickDrainsMoreThanOneBatch(t *testing.T) {
	store := &fakeStore{}
	for i := 0; i < batchSize+3; i++ {
		store.queue = append(store.queue, note.DueReminder{NoteID: string(rune('a' + i)), LineUserID: "U1", Content: "x", RemindAt: testNow})
	}
	pusher := &fakePusher{}
	newTestScheduler(store, pusher).Tick(context.Background())
	if len(pusher.sent) != batchSize+3 || store.claims != 2 {
		t.Errorf("sent %d in %d claims, want %d in 2", len(pusher.sent), store.claims, batchSize+3)
	}
}

func TestClassifyPush(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		status    int
		err       error
		wantNil   bool
		wantRetry bool
	}{
		{http.StatusOK, nil, true, false},
		{http.StatusConflict, boom, true, false}, // retry key เคยส่งแล้ว
		{0, boom, false, true},                   // เน็ตล่ม
		{http.StatusTooManyRequests, boom, false, true},
		{http.StatusInternalServerError, boom, false, true},
		{http.StatusForbidden, boom, false, false},
		{http.StatusBadRequest, boom, false, false},
	}
	for _, tc := range cases {
		err := classifyPush(tc.status, tc.err)
		if (err == nil) != tc.wantNil {
			t.Errorf("status %d: err = %v, want nil=%v", tc.status, err, tc.wantNil)
			continue
		}
		var pe *PushError
		if err != nil && (!errors.As(err, &pe) || pe.Retry != tc.wantRetry) {
			t.Errorf("status %d: err = %#v, want retry=%v", tc.status, err, tc.wantRetry)
		}
	}
}
