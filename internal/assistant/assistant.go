// Package assistant แปลงข้อความภาษาธรรมชาติจาก LINE เป็นการกระทำกับโน้ต ผ่าน LLM tool calling
// (Claude หรือ local model ผ่าน Ollama — ดู provider.go)
package assistant

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"jodjod/internal/note"
)

const (
	// รอบสูงสุดของ request → tool → request (เช่น search แล้วค่อย complete)
	// เผื่อรอบที่ใช้เตือนโมเดลให้เรียก tool ไว้ด้วย
	maxRounds = 5

	// จำนวนครั้งสูงสุดที่เตือนโมเดลเมื่อมันตอบโดยไม่เรียก tool
	maxNudges = 2

	openListsInContext = 10

	fallbackReply = "ขอโทษครับ ตอนนี้ทำรายการไม่สำเร็จ ลองพิมพ์ใหม่อีกครั้งนะครับ"
	refusalReply  = "ขอโทษครับ เรื่องนี้ผมช่วยจัดการให้ไม่ได้ครับ"
)

var bangkok = mustLoadBangkok()

func mustLoadBangkok() *time.Location {
	loc, err := time.LoadLocation("Asia/Bangkok")
	if err != nil {
		return time.FixedZone("Asia/Bangkok", 7*60*60)
	}
	return loc
}

// NoteStore คือ operation ของ note.Repository ที่ tools ใช้
type NoteStore interface {
	SaveNote(ctx context.Context, n *note.Note) error
	SaveChecklist(ctx context.Context, userID string, content *string, items []string, source note.NoteSource) (*note.Note, error)
	FindOpenChecklistByTitle(ctx context.Context, userID, title string) (*note.Note, error)
	ListOpenChecklistTitles(ctx context.Context, userID string, limit int) ([]string, error)
	AppendItems(ctx context.Context, userID, noteID string, items []string) ([]*note.NoteItem, error)
	FindItemsBySimilarText(ctx context.Context, userID, query string, limit int) ([]*note.ItemMatch, error)
	SetItemDone(ctx context.Context, userID, itemID string, done bool) error
	SetPinned(ctx context.Context, userID, noteID string, pinned bool) error
	FindFeed(ctx context.Context, q note.FeedQuery) ([]*note.Note, error)
	SearchByUser(ctx context.Context, userID, query string) ([]*note.Note, error)
	FindAgenda(ctx context.Context, userID string, from, to time.Time) ([]*note.Note, error)
	FindItemsByNoteID(ctx context.Context, noteID string) ([]*note.NoteItem, error)
}

// ActionLogger บันทึกสิ่งที่ AI ทำ (ใช้ undo, ดูต้นทุน, และเป็น dataset สำหรับ eval)
type ActionLogger interface {
	Insert(ctx context.Context, a *Action) (string, error)
	Undo(ctx context.Context, userID, actionID string) (*UndoResult, error)
	History(ctx context.Context, userID string, limit int, before *time.Time) ([]HistoryItem, error)
}

// ตรวจตอน compile ว่าของจริง implement interface ครบ
var (
	_ NoteStore      = (*note.Repository)(nil)
	_ ActionLogger   = (*ActionRepository)(nil)
	_ Provider       = (*ClaudeProvider)(nil)
	_ Provider       = (*OllamaProvider)(nil)
	_ note.Assistant = (*Assistant)(nil)
)

type Assistant struct {
	provider Provider
	store    NoteStore
	actions  ActionLogger
	calendar Calendar // nil = ไม่ได้ตั้งค่า Google Calendar
	now      func() time.Time
}

func New(provider Provider, store NoteStore, actions ActionLogger) *Assistant {
	return &Assistant{
		provider: provider,
		store:    store,
		actions:  actions,
		now:      func() time.Time { return time.Now().In(bangkok) },
	}
}

// Request คือข้อความหนึ่งข้อความจากผู้ใช้ที่ resolve เป็น user ในระบบแล้ว
type Request struct {
	UserID         string
	WebhookEventID string
	Text           string
}

// Result คือสิ่งที่ handler ต้องใช้ตอบกลับ LINE
type Result struct {
	Reply string
	// ActionID ของแถวใน ai_actions (ว่างถ้าบันทึกไม่สำเร็จ)
	ActionID string
	// Wrote = มีการสร้าง/แก้ข้อมูล → ควรแสดงปุ่ม undo
	Wrote bool
}

// Handle ส่งข้อความให้โมเดลเลือก tool, รัน tool, วนจนได้คำตอบสุดท้าย แล้วบันทึก ai_actions
// error ที่คืนคือ error ของระบบ; กรณีโมเดลปฏิเสธหรือถึงขีดจำกัดจะคืน Result พร้อมข้อความสุภาพแทน
func (a *Assistant) Handle(ctx context.Context, req Request) (*Result, error) {
	now := a.now()
	exec := &executor{store: a.store, userID: req.UserID, now: now, userText: req.Text, calendar: a.calendar}
	if a.calendar != nil {
		on, err := a.calendar.Connected(ctx, req.UserID)
		if err != nil {
			log.Println("เช็คการเชื่อม Google Calendar ล้มเหลว:", err)
		}
		exec.calendarOn = on
	}
	action := &Action{UserID: req.UserID, WebhookEventID: req.WebhookEventID, InputText: req.Text, Model: a.provider.Name()}

	reply, runErr := a.run(ctx, req, exec, action, now)
	if runErr == nil && needsReminderHint(req.Text, exec) && !strings.Contains(reply, reminderHint) {
		reply += "\n" + reminderHint
	}

	action.ReplyText = reply
	action.CreatedNoteIDs = exec.createdNoteIDs
	action.AddedItemIDs = exec.addedItemIDs
	action.ItemChanges = exec.itemChanges
	action.PinChanges = exec.pinChanges
	action.CalendarEventIDs = exec.calendarEventIDs
	action.CalendarMoves = exec.calendarMoves
	action.CancelledEvents = exec.cancelledEvents
	if runErr != nil {
		action.Error = runErr.Error()
	}

	res := &Result{Reply: reply, Wrote: exec.wrote()}
	if a.actions != nil {
		// บันทึกด้วย context ใหม่: ถ้า ctx หลักหมดเวลา ก็ยังอยากได้ log ไว้ทำ undo
		logStart := time.Now()
		logCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		id, err := a.actions.Insert(logCtx, action)
		cancel()
		action.timing.log = time.Since(logStart)
		if err != nil {
			log.Println("บันทึก ai_action ล้มเหลว:", err)
		}
		res.ActionID = id
	}
	log.Printf("AI timing (%s, %d rounds): %s", a.provider.Name(), len(action.timing.llm), action.timing)

	if runErr != nil && !exec.wrote() {
		return nil, runErr
	}
	if runErr != nil {
		// เขียนข้อมูลไปบางส่วนแล้ว: ตอบผู้ใช้ให้รู้ (มีปุ่ม undo) แทนการคืน error เงียบๆ
		log.Println("assistant ล้มเหลวหลังเขียนข้อมูลบางส่วน:", runErr)
		res.Reply = fallbackReply
	}
	return res, nil
}

func (a *Assistant) run(ctx context.Context, req Request, exec *executor, action *Action, now time.Time) (string, error) {
	if note.IsPinnedQuestion(req.Text) {
		return a.answerPinned(ctx, exec, action)
	}

	listsStart := time.Now()
	openLists, err := a.store.ListOpenChecklistTitles(ctx, req.UserID, openListsInContext)
	action.timing.lists = time.Since(listsStart)
	if err != nil {
		// ไม่ร้ายแรง: แค่โมเดลจะไม่รู้ชื่อลิสต์เดิม
		log.Println("โหลดชื่อลิสต์ล้มเหลว:", err)
	}

	conv := a.provider.Start(systemPrompt, toolsFor(exec.calendarOn), buildUserMessage(now, openLists, req.Text))

	var (
		results  []ToolResult
		followUp string
		nudges   int
	)
	for round := 0; round < maxRounds; round++ {
		llmStart := time.Now()
		turn, err := conv.Next(ctx, results, followUp)
		action.timing.llm = append(action.timing.llm, time.Since(llmStart))
		followUp = ""
		if err != nil {
			return fallbackReply, fmt.Errorf("%s request (round %d): %w", a.provider.Name(), round+1, err)
		}
		action.addUsage(turn.Usage)

		switch turn.Stop {
		case StopRefusal:
			return refusalReply, nil
		case StopMaxTokens:
			return fallbackReply, errors.New("model response hit the output token limit")
		}

		if len(turn.ToolCalls) == 0 {
			reply := strings.TrimSpace(turn.Text)
			if reply == "" {
				return fallbackReply, errors.New("model returned an empty reply")
			}
			// โมเดลเล็กบางทีตอบโดยไม่เรียก tool เลย ("✅ ทำแล้ว", "ครับ", "กำลังค้นหา...") → ไม่มีอะไรถูกบันทึก
			// ทุกข้อความของผู้ใช้ต้องจบด้วย tool อย่างน้อยหนึ่งตัว ยกเว้นโมเดลถามกลับ
			if len(action.ToolCalls) == 0 && !isQuestion(reply) {
				if nudges < maxNudges {
					nudges++
					results, followUp = nil, nudgeNoToolCall
					continue
				}
				// เตือนครบแล้วยังไม่เรียก tool: ถ้ายังอ้างว่าทำแล้ว อย่าปล่อยให้ผู้ใช้เข้าใจผิด
				if claimsAction(reply) {
					return fallbackReply, errors.New("model claimed an action without calling any tool")
				}
			}
			return reply, nil
		}

		results = results[:0]
		for _, call := range turn.ToolCalls {
			toolStart := time.Now()
			run := a.runTool(ctx, exec, call)
			action.timing.tools += time.Since(toolStart)
			action.ToolCalls = append(action.ToolCalls, run)
			results = append(results, ToolResult{
				CallID: call.ID, Name: call.Name, Content: toolResultText(run), IsError: run.IsError,
			})
		}
	}
	return fallbackReply, fmt.Errorf("no final reply after %d rounds", maxRounds)
}

func (a *Assistant) runTool(ctx context.Context, exec *executor, call ToolCall) toolRun {
	run := toolRun{Name: call.Name, Input: call.Input}

	result, err := exec.run(ctx, call.Name, call.Input)
	var inputErr errToolInput
	switch {
	case errors.As(err, &inputErr):
		run.IsError, run.Error = true, inputErr.msg
	case err != nil:
		// error ของระบบ (DB ฯลฯ): ไม่ส่งรายละเอียดภายในให้โมเดล
		log.Printf("tool %s ล้มเหลว: %v", call.Name, err)
		run.IsError, run.Error = true, "internal error, the action was not completed"
	default:
		run.Result = result
	}
	return run
}

// claimsAction บอกว่าคำตอบมี emoji ที่ใช้ยืนยันการเขียนข้อมูลหรือไม่
func claimsAction(reply string) bool {
	return strings.ContainsAny(reply, confirmationEmojis)
}

// questionMarkers คือคำ/เครื่องหมายที่บอกว่าโมเดลกำลังถามผู้ใช้กลับ
// ไม่ใส่ "อะไร" เพราะเจอในประโยคบอกเล่าบ่อย เช่น "ไม่มีนัดอะไร"
var questionMarkers = []string{
	"?", "？", "ไหม", "มั้ย", "หรือเปล่า", "รึเปล่า", "หรือไม่",
	"เมื่อไหร่", "เมื่อไร", "อันไหน", "ตัวไหน", "วันไหน", "กี่โมง",
}

// isQuestion บอกว่าคำตอบเป็นคำถามกลับไปหาผู้ใช้ (ไม่ต้องเรียก tool ก็ได้)
func isQuestion(reply string) bool {
	for _, m := range questionMarkers {
		if strings.Contains(reply, m) {
			return true
		}
	}
	return false
}

// HandleLine คือ Handle ในรูปที่ LINE webhook ใช้ (implement note.Assistant)
func (a *Assistant) HandleLine(ctx context.Context, userID, webhookEventID, text string) (note.AIReply, error) {
	res, err := a.Handle(ctx, Request{UserID: userID, WebhookEventID: webhookEventID, Text: text})
	if err != nil {
		return note.AIReply{}, err
	}
	return note.AIReply{Text: res.Reply, ActionID: res.ActionID, Undoable: res.Wrote && res.ActionID != ""}, nil
}

// Undo ย้อนสิ่งที่ AI ทำใน action นั้น (จากปุ่มยกเลิกใน LINE) แล้วคืนข้อความตอบผู้ใช้
func (a *Assistant) Undo(ctx context.Context, userID, actionID string) (string, error) {
	res, err := a.UndoAction(ctx, userID, actionID)
	if err != nil {
		return "", err
	}
	return undoReplyText(res), nil
}

// UndoAction ย้อนข้อมูลในระบบ แล้วลบนัดใน Google Calendar ที่ action นั้นสร้างไว้
func (a *Assistant) UndoAction(ctx context.Context, userID, actionID string) (*UndoResult, error) {
	if a.actions == nil {
		return nil, errors.New("action log is not configured")
	}
	res, err := a.actions.Undo(ctx, userID, actionID)
	if err != nil {
		return nil, err
	}
	res.DeletedEvents = a.deleteCalendarEvents(ctx, userID, res.CalendarEventIDs)
	res.MovedBack = a.restoreCalendarMoves(ctx, userID, res.CalendarMoves)
	res.RecreatedEvents = a.restoreCancelledEvents(ctx, userID, res.CancelledEvents)
	return res, nil
}

func undoReplyText(r *UndoResult) string {
	switch {
	case r.NotFound:
		return "ไม่พบรายการที่จะยกเลิกครับ"
	case r.AlreadyUndone:
		return "รายการนี้ยกเลิกไปแล้วครับ"
	case r.Expired:
		return "ยกเลิกได้ภายใน 24 ชั่วโมงครับ รายการนี้เก่าเกินไปแล้ว"
	}
	var parts []string
	if r.DeletedNotes > 0 {
		parts = append(parts, fmt.Sprintf("ลบที่จดไว้ %d รายการ", r.DeletedNotes))
	}
	if r.RemovedItems > 0 {
		parts = append(parts, fmt.Sprintf("เอาออกจากลิสต์ %d รายการ", r.RemovedItems))
	}
	if r.RestoredItems > 0 {
		parts = append(parts, fmt.Sprintf("คืนสถานะติ๊ก %d รายการ", r.RestoredItems))
	}
	if r.RestoredPins > 0 {
		parts = append(parts, fmt.Sprintf("คืนสถานะปักหมุด %d รายการ", r.RestoredPins))
	}
	if r.DeletedEvents > 0 {
		parts = append(parts, fmt.Sprintf("ลบนัดใน Google Calendar %d รายการ", r.DeletedEvents))
	}
	if r.MovedBack > 0 {
		parts = append(parts, fmt.Sprintf("ย้ายนัดกลับเวลาเดิม %d รายการ", r.MovedBack))
	}
	if r.RecreatedEvents > 0 {
		parts = append(parts, fmt.Sprintf("คืนนัดที่ลบไปใน Google Calendar %d รายการ", r.RecreatedEvents))
	}
	if len(parts) == 0 {
		return "↩️ ยกเลิกแล้วครับ (ข้อมูลถูกลบหรือแก้ไปก่อนหน้านี้แล้ว)"
	}
	return "↩️ ยกเลิกแล้วครับ: " + strings.Join(parts, ", ")
}

// Warmup โหลดโมเดลล่วงหน้าถ้า provider รองรับ (เรียกตอน start server) ไม่งั้นไม่ทำอะไร
func (a *Assistant) Warmup(ctx context.Context) error {
	if w, ok := a.provider.(warmer); ok {
		return w.Warmup(ctx, systemPrompt, toolSpecs(), buildUserMessage(a.now(), nil, "สวัสดี"))
	}
	return nil
}

// reminderHint บอกผู้ใช้ว่าต้องพิมพ์เวลาถ้าอยากให้เตือน (ใช้เมื่อขอให้เตือนแต่ไม่ได้บอกเวลา)
const reminderHint = `ถ้าอยากให้เตือน พิมพ์บอกวันหรือเวลามาได้เลยครับ เช่น "เตือนซื้อพัดลม พรุ่งนี้ 9 โมง"`

// needsReminderHint: ผู้ใช้ขอให้ "เตือน" แต่ไม่ได้บอกวัน/เวลา ระบบจึงจดเป็นโน้ตแทน
// เติมคำแนะนำในโค้ดแทนการหวังให้โมเดลเล็กจำไปบอกเอง
func needsReminderHint(text string, exec *executor) bool {
	return strings.Contains(text, "เตือน") && !mentionsTime(text) && exec.savedNote && !exec.savedReminder
}
