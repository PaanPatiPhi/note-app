package assistant

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"jodjod/internal/integrations"
)

// ── fake Google Calendar ────────────────────────────────────────

type fakeCalendar struct {
	connected bool
	created   []integrations.Event
	listed    []integrations.Event
	deleted   []string
	moved     []integrations.Event
}

func (f *fakeCalendar) MoveEvent(_ context.Context, _ string, id string, start, end time.Time) (integrations.Event, error) {
	e := integrations.Event{ID: id, Start: start, End: end}
	f.moved = append(f.moved, e)
	return e, nil
}

func (f *fakeCalendar) Connected(context.Context, string) (bool, error) { return f.connected, nil }

func (f *fakeCalendar) CreateEvent(_ context.Context, _ string, e integrations.Event) (integrations.Event, error) {
	e.ID = fmt.Sprintf("gcal-%d", len(f.created)+1)
	f.created = append(f.created, e)
	return e, nil
}

func (f *fakeCalendar) ListEvents(context.Context, string, time.Time, time.Time) ([]integrations.Event, error) {
	return f.listed, nil
}

func (f *fakeCalendar) DeleteEvent(_ context.Context, _ string, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

func TestCreateEventGoesToCalendar(t *testing.T) {
	cal := &fakeCalendar{connected: true}
	llm := &fakeLLM{responses: []string{
		// โมเดลให้ชั่วโมงผิด (02:00) — โค้ดต้องแก้เป็นบ่ายสองตามที่ผู้ใช้พิมพ์ และเลื่อนเวลาจบตาม
		toolUseMsg("t1", "create_event", map[string]any{"title": "ประชุมทีม", "start": "2026-10-16T02:00:00+07:00", "end": "2026-10-16T03:30:00+07:00"}),
		textMsg("📅 ลงนัดแล้วครับ"),
	}}
	a, actions := newTestAssistant(llm, newFakeStore())
	a.WithCalendar(cal)

	res := handle(t, a, "พรุ่งนี้บ่ายสองประชุมทีม")

	if !strings.Contains(llm.requests[0], `"create_event"`) {
		t.Error("create_event should be offered when the calendar is connected")
	}
	if len(cal.created) != 1 {
		t.Fatalf("created events = %d, want 1", len(cal.created))
	}
	ev := cal.created[0]
	wantStart := time.Date(2026, 10, 16, 14, 0, 0, 0, bangkok)
	if ev.Title != "ประชุมทีม" || !ev.Start.Equal(wantStart) || !ev.End.Equal(wantStart.Add(90*time.Minute)) || ev.PopupAtStart {
		t.Errorf("event = %+v", ev)
	}
	if !res.Wrote || len(actions.saved[0].CalendarEventIDs) != 1 || actions.saved[0].CalendarEventIDs[0] != "gcal-1" {
		t.Errorf("result wrote=%v action ids=%v", res.Wrote, actions.saved[0].CalendarEventIDs)
	}
}

func TestCreateEventWithoutCalendar(t *testing.T) {
	cal := &fakeCalendar{connected: false}
	llm := &fakeLLM{responses: []string{
		toolUseMsg("t1", "create_event", map[string]any{"title": "ประชุมทีม", "start": "2026-10-16T14:00:00+07:00"}),
		textMsg("ยังไม่ได้เชื่อม Google Calendar ครับ"),
	}}
	a, _ := newTestAssistant(llm, newFakeStore())
	a.WithCalendar(cal)

	handle(t, a, "พรุ่งนี้บ่ายสองประชุมทีม")

	if strings.Contains(llm.requests[0], `"create_event"`) {
		t.Error("create_event must not be offered before the user connects a calendar")
	}
	if len(cal.created) != 0 || !strings.Contains(llm.requests[1], `"is_error":true`) {
		t.Errorf("create_event without a calendar should be a tool error (created=%d)", len(cal.created))
	}
}

func TestCreateEventNeedsUserTime(t *testing.T) {
	cal := &fakeCalendar{connected: true}
	llm := &fakeLLM{responses: []string{
		toolUseMsg("t1", "create_event", map[string]any{"title": "ประชุมทีม", "start": "2026-10-16T14:00:00+07:00"}),
		textMsg("ประชุมกี่โมงครับ"),
	}}
	a, _ := newTestAssistant(llm, newFakeStore())
	a.WithCalendar(cal)

	handle(t, a, "มีประชุมทีม")

	if len(cal.created) != 0 {
		t.Error("an event must not be created with a time the user never gave")
	}
}

func TestReminderIsMirroredToCalendar(t *testing.T) {
	cal := &fakeCalendar{connected: true}
	llm := &fakeLLM{responses: []string{
		toolUseMsg("t1", "create_reminder", map[string]any{"text": "โทรหาหมอ", "remind_at": "2026-10-16T09:00:00+07:00"}),
		textMsg("⏰ ตั้งเตือนแล้วครับ"),
	}}
	store := newFakeStore()
	a, actions := newTestAssistant(llm, store)
	a.WithCalendar(cal)

	handle(t, a, "พรุ่งนี้ 9 โมงเตือนโทรหาหมอ")

	if len(store.notes) != 1 || len(cal.created) != 1 {
		t.Fatalf("notes=%d events=%d, want 1 and 1", len(store.notes), len(cal.created))
	}
	ev := cal.created[0]
	at := time.Date(2026, 10, 16, 9, 0, 0, 0, bangkok)
	if ev.Title != "⏰ โทรหาหมอ" || !ev.PopupAtStart || !ev.Start.Equal(at) || !ev.End.Equal(at.Add(reminderEventDuration)) {
		t.Errorf("mirrored event = %+v", ev)
	}
	if !strings.Contains(llm.requests[1], `also_in_google_calendar`) {
		t.Error("tool result should tell the model the reminder is also in Google Calendar")
	}
	if got := actions.saved[0].CalendarEventIDs; len(got) != 1 {
		t.Errorf("calendar event ids = %v", got)
	}
}

func TestAgendaIncludesCalendarEvents(t *testing.T) {
	start := time.Date(2026, 10, 16, 14, 0, 0, 0, bangkok)
	cal := &fakeCalendar{connected: true, listed: []integrations.Event{
		{ID: "e1", Title: "ประชุมทีม", Start: start, End: start.Add(time.Hour)},
		{ID: "e2", Title: "วันหยุด", Start: time.Date(2026, 10, 23, 0, 0, 0, 0, bangkok), AllDay: true},
	}}
	llm := &fakeLLM{responses: []string{
		toolUseMsg("t1", "list_agenda", map[string]any{"from": "2026-10-15", "to": "2026-10-25"}),
		textMsg("มีประชุมทีมพรุ่งนี้บ่ายสองครับ"),
	}}
	a, _ := newTestAssistant(llm, newFakeStore())
	a.WithCalendar(cal)

	res := handle(t, a, "อาทิตย์นี้มีนัดอะไรบ้าง")

	for _, want := range []string{`calendar_event`, `ประชุมทีม`, `2026-10-16T14:00:00+07:00`, `2026-10-23`} {
		if !strings.Contains(llm.requests[1], want) {
			t.Errorf("agenda tool result missing %s", want)
		}
	}
	if res.Wrote {
		t.Error("reading the agenda should not count as a write")
	}
}

type undoWithEvents struct {
	*fakeActions
	ids []string
}

func (u undoWithEvents) Undo(context.Context, string, string) (*UndoResult, error) {
	return &UndoResult{DeletedNotes: 1, CalendarEventIDs: u.ids}, nil
}

func TestUndoDeletesCalendarEvents(t *testing.T) {
	cal := &fakeCalendar{connected: true}
	a := New(NewClaudeProvider(&fakeLLM{}, ""), newFakeStore(), undoWithEvents{&fakeActions{}, []string{"gcal-1", "gcal-2"}})
	a.WithCalendar(cal)

	res, err := a.UndoAction(context.Background(), "user-1", "action-1")
	if err != nil {
		t.Fatal(err)
	}
	if res.DeletedEvents != 2 || strings.Join(cal.deleted, ",") != "gcal-1,gcal-2" {
		t.Errorf("deleted = %v (count %d)", cal.deleted, res.DeletedEvents)
	}
	if got := undoReplyText(res); !strings.Contains(got, "ลบนัดใน Google Calendar 2 รายการ") {
		t.Errorf("undo reply = %q", got)
	}
}

// testNow = พฤ. 15 ต.ค. 14:30 → "พรุ่งนี้" = 16 ต.ค.
func meetingCalendar() *fakeCalendar {
	start := time.Date(2026, 10, 16, 14, 0, 0, 0, bangkok)
	return &fakeCalendar{connected: true, listed: []integrations.Event{
		{ID: "m1", Title: "ประชุมทีม", Start: start, End: start.Add(90 * time.Minute)},
		{ID: "r1", Title: "⏰ ประชุมทีม", Start: start, End: start.Add(15 * time.Minute)},
		{ID: "d1", Title: "หมอฟัน", Start: start.Add(48 * time.Hour), End: start.Add(49 * time.Hour)},
	}}
}

func TestMoveEventKeepsDayAndLength(t *testing.T) {
	cal := meetingCalendar()
	llm := &fakeLLM{responses: []string{
		// โมเดลให้เวลาผิดวันผิดชั่วโมง → ต้องได้ 16 ต.ค. 15:00 (วันเดิม, บ่ายสาม) ยาวเท่าเดิม 90 นาที
		toolUseMsg("t1", "move_event", map[string]any{"query": "ประชุม", "new_start": "2026-10-15T03:00:00+07:00"}),
		textMsg("📅 เลื่อนแล้วครับ"),
	}}
	a, actions := newTestAssistant(llm, newFakeStore())
	a.WithCalendar(cal)

	res := handle(t, a, "เลื่อนประชุมทีมเป็นบ่ายสาม")

	if !strings.Contains(llm.requests[0], `"move_event"`) {
		t.Error("move_event should be offered when the calendar is connected")
	}
	if len(cal.created) != 0 || len(cal.moved) != 1 {
		t.Fatalf("created=%d moved=%d, want 0 and 1", len(cal.created), len(cal.moved))
	}
	want := time.Date(2026, 10, 16, 15, 0, 0, 0, bangkok)
	if m := cal.moved[0]; m.ID != "m1" || !m.Start.Equal(want) || !m.End.Equal(want.Add(90*time.Minute)) {
		t.Errorf("moved = %+v (reminder shadows must be skipped)", m)
	}
	mv := actions.saved[0].CalendarMoves
	if !res.Wrote || len(mv) != 1 || !mv[0].PrevStart.Equal(time.Date(2026, 10, 16, 14, 0, 0, 0, bangkok)) {
		t.Errorf("wrote=%v moves=%+v", res.Wrote, mv)
	}
}

func TestMoveEventToAnotherDay(t *testing.T) {
	cal := meetingCalendar()
	llm := &fakeLLM{responses: []string{
		toolUseMsg("t1", "move_event", map[string]any{"query": "ประชุมทีม", "new_start": "2026-10-16T14:00:00+07:00"}),
		textMsg("📅 เลื่อนแล้วครับ"),
	}}
	a, _ := newTestAssistant(llm, newFakeStore())
	a.WithCalendar(cal)

	// "พรุ่งนี้บ่ายสอง" คือเวลาเดิม ส่วนเวลาใหม่คือ "มะรืน 10 โมง"
	handle(t, a, "เลื่อนประชุมทีมพรุ่งนี้บ่ายสองไปมะรืน 10 โมง")

	want := time.Date(2026, 10, 17, 10, 0, 0, 0, bangkok)
	if len(cal.moved) != 1 || !cal.moved[0].Start.Equal(want) {
		t.Fatalf("moved = %+v, want start %s", cal.moved, want)
	}
}

func TestCreateEventRefusedWhenRescheduling(t *testing.T) {
	cal := meetingCalendar()
	llm := &fakeLLM{responses: []string{
		toolUseMsg("t1", "create_event", map[string]any{"title": "ประชุมทีม", "start": "2026-10-16T15:00:00+07:00"}),
		toolUseMsg("t2", "move_event", map[string]any{"query": "ประชุม", "new_start": "2026-10-16T15:00:00+07:00"}),
		textMsg("📅 เลื่อนแล้วครับ"),
	}}
	a, _ := newTestAssistant(llm, newFakeStore())
	a.WithCalendar(cal)

	handle(t, a, "เลื่อนประชุมเป็นบ่ายสาม")

	if len(cal.created) != 0 {
		t.Error("asking to move a meeting must not create a duplicate event")
	}
	if !strings.Contains(llm.requests[1], "call move_event instead") || len(cal.moved) != 1 {
		t.Errorf("model should be told to use move_event (moved=%d)", len(cal.moved))
	}
}

func TestMoveEventAmbiguousOrMissing(t *testing.T) {
	cal := meetingCalendar()
	start := time.Date(2026, 10, 20, 9, 0, 0, 0, bangkok)
	cal.listed = append(cal.listed, integrations.Event{ID: "m2", Title: "ประชุมลูกค้า", Start: start, End: start.Add(time.Hour)})
	llm := &fakeLLM{responses: []string{
		toolUseMsg("t1", "move_event", map[string]any{"query": "ประชุม", "new_start": "2026-10-16T15:00:00+07:00"}),
		textMsg("หมายถึงประชุมทีมหรือประชุมลูกค้าครับ"),
	}}
	a, _ := newTestAssistant(llm, newFakeStore())
	a.WithCalendar(cal)
	handle(t, a, "เลื่อนประชุมเป็นบ่ายสาม")
	if len(cal.moved) != 0 || !strings.Contains(llm.requests[1], "ambiguous") || !strings.Contains(llm.requests[1], "ประชุมลูกค้า") {
		t.Errorf("two matching meetings should be ambiguous (moved=%d)", len(cal.moved))
	}

	missing := &fakeLLM{responses: []string{
		toolUseMsg("t1", "move_event", map[string]any{"query": "ตัดผม", "new_start": "2026-10-16T15:00:00+07:00"}),
		textMsg("หานัดตัดผมไม่เจอครับ"),
	}}
	a2, _ := newTestAssistant(missing, newFakeStore())
	a2.WithCalendar(cal)
	handle(t, a2, "เลื่อนตัดผมเป็นบ่ายสาม")
	if !strings.Contains(missing.requests[1], `"is_error":true`) || !strings.Contains(missing.requests[1], "do not create a new event") {
		t.Error("an unknown event should be a tool error telling the model not to create one")
	}
}

func TestMoveEventNeedsNewTime(t *testing.T) {
	cal := meetingCalendar()
	llm := &fakeLLM{responses: []string{
		toolUseMsg("t1", "move_event", map[string]any{"query": "ประชุม", "new_start": "2026-10-16T15:00:00+07:00"}),
		textMsg("เลื่อนไปเมื่อไหร่ครับ"),
	}}
	a, _ := newTestAssistant(llm, newFakeStore())
	a.WithCalendar(cal)
	handle(t, a, "เลื่อนประชุมทีมออกไปก่อน")
	if len(cal.moved) != 0 {
		t.Error("must not move an event to a time the user never gave")
	}
}

type undoWithMoves struct{ *fakeActions }

func (undoWithMoves) Undo(context.Context, string, string) (*UndoResult, error) {
	prev := time.Date(2026, 10, 16, 14, 0, 0, 0, bangkok)
	return &UndoResult{CalendarMoves: []eventMove{{EventID: "m1", PrevStart: prev, PrevEnd: prev.Add(time.Hour)}}}, nil
}

func TestUndoMovesEventBack(t *testing.T) {
	cal := &fakeCalendar{connected: true}
	a := New(NewClaudeProvider(&fakeLLM{}, ""), newFakeStore(), undoWithMoves{&fakeActions{}})
	a.WithCalendar(cal)

	res, err := a.UndoAction(context.Background(), "user-1", "action-1")
	if err != nil {
		t.Fatal(err)
	}
	prev := time.Date(2026, 10, 16, 14, 0, 0, 0, bangkok)
	if res.MovedBack != 1 || len(cal.moved) != 1 || !cal.moved[0].Start.Equal(prev) {
		t.Errorf("moved back = %+v (count %d)", cal.moved, res.MovedBack)
	}
	if got := undoReplyText(res); !strings.Contains(got, "ย้ายนัดกลับเวลาเดิม 1 รายการ") {
		t.Errorf("undo reply = %q", got)
	}
}

func TestMoveTargetText(t *testing.T) {
	for in, want := range map[string]string{
		"เลื่อนประชุมเป็นบ่ายสาม":            "บ่ายสาม",
		"เลื่อนประชุมพรุ่งนี้ไปมะรืน 10 โมง": "มะรืน 10 โมง",
		"ประชุมบ่ายสาม":                      "ประชุมบ่ายสาม",
	} {
		if got := moveTargetText(in); got != want {
			t.Errorf("moveTargetText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCancelEventDeletesMatchingEvent(t *testing.T) {
	cal := meetingCalendar()
	llm := &fakeLLM{responses: []string{
		toolUseMsg("t1", "cancel_event", map[string]any{"query": "ประชุม"}),
		textMsg("📅 ยกเลิกประชุมทีมแล้วครับ"),
	}}
	a, actions := newTestAssistant(llm, newFakeStore())
	a.WithCalendar(cal)

	res := handle(t, a, "ยกเลิกประชุมทีมพรุ่งนี้")

	if !strings.Contains(llm.requests[0], `"cancel_event"`) {
		t.Error("cancel_event should be offered when the calendar is connected")
	}
	if strings.Join(cal.deleted, ",") != "m1" {
		t.Fatalf("deleted = %v, want only m1 (reminder shadows are skipped)", cal.deleted)
	}
	got := actions.saved[0].CancelledEvents
	if !res.Wrote || len(got) != 1 || got[0].Title != "ประชุมทีม" || got[0].End.Sub(got[0].Start) != 90*time.Minute {
		t.Errorf("wrote=%v cancelled=%+v", res.Wrote, got)
	}
}

func TestCancelEventNeedsUserToAsk(t *testing.T) {
	for _, text := range []string{"พรุ่งนี้ประชุมทีมกี่โมง", "ยกเลิกติ๊กประชุมทีม"} {
		cal := meetingCalendar()
		llm := &fakeLLM{responses: []string{
			toolUseMsg("t1", "cancel_event", map[string]any{"query": "ประชุม"}),
			textMsg("ไม่ได้ลบครับ"),
		}}
		a, _ := newTestAssistant(llm, newFakeStore())
		a.WithCalendar(cal)
		handle(t, a, text)
		if len(cal.deleted) != 0 || !strings.Contains(llm.requests[1], "do not delete anything") {
			t.Errorf("%q must not delete an event (deleted=%v)", text, cal.deleted)
		}
	}
}

func TestCancelAllDayEvent(t *testing.T) {
	cal := meetingCalendar()
	day := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	cal.listed = append(cal.listed, integrations.Event{ID: "h1", Title: "ไปเที่ยวเชียงใหม่", Start: day, End: day.AddDate(0, 0, 1), AllDay: true})
	llm := &fakeLLM{responses: []string{
		toolUseMsg("t1", "cancel_event", map[string]any{"query": "เชียงใหม่"}),
		textMsg("📅 ยกเลิกแล้วครับ"),
	}}
	a, _ := newTestAssistant(llm, newFakeStore())
	a.WithCalendar(cal)
	handle(t, a, "ไม่ไปเชียงใหม่แล้ว ลบนัดออกด้วย")
	if strings.Join(cal.deleted, ",") != "h1" || !strings.Contains(llm.requests[1], "ทั้งวัน") {
		t.Errorf("deleted = %v; all-day events can be cancelled and are described as ทั้งวัน", cal.deleted)
	}
}

type undoWithCancelled struct{ *fakeActions }

func (undoWithCancelled) Undo(context.Context, string, string) (*UndoResult, error) {
	start := time.Date(2026, 10, 16, 14, 0, 0, 0, bangkok)
	return &UndoResult{CancelledEvents: []cancelledEvent{{Title: "ประชุมทีม", Location: "ห้อง 3", Start: start, End: start.Add(time.Hour)}}}, nil
}

func TestUndoRecreatesCancelledEvent(t *testing.T) {
	cal := &fakeCalendar{connected: true}
	a := New(NewClaudeProvider(&fakeLLM{}, ""), newFakeStore(), undoWithCancelled{&fakeActions{}})
	a.WithCalendar(cal)

	res, err := a.UndoAction(context.Background(), "user-1", "action-1")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 16, 14, 0, 0, 0, bangkok)
	if res.RecreatedEvents != 1 || len(cal.created) != 1 {
		t.Fatalf("recreated = %d, created = %+v", res.RecreatedEvents, cal.created)
	}
	if ev := cal.created[0]; ev.Title != "ประชุมทีม" || ev.Location != "ห้อง 3" || !ev.Start.Equal(start) || !ev.End.Equal(start.Add(time.Hour)) {
		t.Errorf("recreated event = %+v", ev)
	}
	if got := undoReplyText(res); !strings.Contains(got, "คืนนัดที่ลบไปใน Google Calendar 1 รายการ") {
		t.Errorf("undo reply = %q", got)
	}
}
