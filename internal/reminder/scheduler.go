// Package reminder ส่ง reminder ที่ถึงเวลาไปหาผู้ใช้ทาง LINE (push message)
package reminder

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"jodjod/internal/note"
)

const (
	defaultInterval = time.Minute
	batchSize       = 20
	// เลยเวลามานานกว่านี้ (เช่น server ปิดไปหลายวัน) ไม่ส่งแล้ว เตือนช้าขนาดนั้นไม่มีประโยชน์
	maxLate = 24 * time.Hour
	// ส่งช้ากว่านี้ให้บอกเวลาที่ตั้งไว้ด้วย ผู้ใช้จะได้รู้ว่าเตือนช้า
	lateNotice = 5 * time.Minute
)

// Store คือ operation ของ note.Repository ที่ scheduler ใช้
type Store interface {
	ClaimDueReminders(ctx context.Context, now time.Time, limit int) ([]note.DueReminder, error)
	ReleaseReminder(ctx context.Context, noteID string) error
}

// Pusher ส่งข้อความหาผู้ใช้ LINE; retryKey ส่งซ้ำด้วยค่าเดิมแล้ว LINE จะไม่ส่งซ้ำ
type Pusher interface {
	Push(ctx context.Context, lineUserID, retryKey, text string) error
}

// PushError คือ error ของการส่ง; Retry = ชั่วคราว (เน็ต, 5xx, 429) ลองใหม่รอบหน้าได้
type PushError struct {
	Retry bool
	Err   error
}

func (e *PushError) Error() string { return e.Err.Error() }
func (e *PushError) Unwrap() error { return e.Err }

type Scheduler struct {
	store    Store
	pusher   Pusher
	interval time.Duration
	now      func() time.Time
}

func New(store Store, pusher Pusher) *Scheduler {
	return &Scheduler{store: store, pusher: pusher, interval: defaultInterval, now: time.Now}
}

// Run ส่ง reminder ทุก interval จน ctx ถูกยกเลิก (เรียกใน goroutine)
func (s *Scheduler) Run(ctx context.Context) {
	log.Printf("reminder: เริ่มส่งเตือนทุก %s", s.interval)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		s.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Tick ส่ง reminder ที่ถึงเวลาหนึ่งรอบ (วนจนหมดคิว ถ้ามีมากกว่า batchSize)
func (s *Scheduler) Tick(ctx context.Context) {
	for {
		due, err := s.store.ClaimDueReminders(ctx, s.now(), batchSize)
		if err != nil {
			log.Println("reminder: ดึงรายการถึงเวลาไม่สำเร็จ:", err)
			return
		}
		for _, d := range due {
			s.send(ctx, d)
		}
		if len(due) < batchSize || ctx.Err() != nil {
			return
		}
	}
}

func (s *Scheduler) send(ctx context.Context, d note.DueReminder) {
	late := s.now().Sub(d.RemindAt)
	switch {
	case d.LineUserID == "":
		log.Printf("reminder: ข้าม %s เพราะผู้ใช้ไม่ได้เชื่อม LINE", d.NoteID)
		return
	case late > maxLate:
		log.Printf("reminder: ข้าม %s เพราะเลยเวลามา %s", d.NoteID, late.Round(time.Minute))
		return
	}

	err := s.pusher.Push(ctx, d.LineUserID, d.NoteID, Message(d, late))
	if err == nil {
		log.Printf("reminder: ส่ง %s แล้ว (ช้า %s)", d.NoteID, late.Round(time.Second))
		return
	}
	var pe *PushError
	if errors.As(err, &pe) && pe.Retry {
		log.Printf("reminder: ส่ง %s ไม่สำเร็จ จะลองใหม่รอบหน้า: %v", d.NoteID, err)
		if rerr := s.store.ReleaseReminder(context.WithoutCancel(ctx), d.NoteID); rerr != nil {
			log.Println("reminder: คืนสถานะไม่สำเร็จ:", rerr)
		}
		return
	}
	log.Printf("reminder: ส่ง %s ไม่สำเร็จ (ไม่ลองใหม่): %v", d.NoteID, err)
}

// Message คือข้อความเตือนที่ส่งหาผู้ใช้
func Message(d note.DueReminder, late time.Duration) string {
	text := fmt.Sprintf("⏰ เตือนครับ: %s", d.Content)
	if late > lateNotice {
		text += fmt.Sprintf("\n(ตั้งไว้ %s ส่งช้าไปหน่อยนะครับ)", note.ThaiDateTime(d.RemindAt))
	}
	return text
}
