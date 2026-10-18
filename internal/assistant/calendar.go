package assistant

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	"jodjod/internal/integrations"
)

// Calendar คือปฏิทินภายนอกของผู้ใช้ (implement โดย integrations.Google)
// ไม่ได้ตั้งค่าไว้ (nil) หรือผู้ใช้ยังไม่ได้เชื่อม = ทำงานแบบเดิมโดยไม่มีปฏิทิน
type Calendar interface {
	Connected(ctx context.Context, userID string) (bool, error)
	CreateEvent(ctx context.Context, userID string, e integrations.Event) (integrations.Event, error)
	ListEvents(ctx context.Context, userID string, from, to time.Time) ([]integrations.Event, error)
	DeleteEvent(ctx context.Context, userID, eventID string) error
	MoveEvent(ctx context.Context, userID, eventID string, start, end time.Time) (integrations.Event, error)
}

var _ Calendar = (*integrations.Google)(nil)

const (
	defaultEventDuration    = time.Hour
	reminderEventDuration   = 15 * time.Minute
	maxEventDuration        = 14 * 24 * time.Hour
	calendarCallTimeout     = 10 * time.Second
	calendarAgendaMaxEvents = 50
)

// WithCalendar เปิดให้ AI ลงนัดและอ่านนัดจากปฏิทินของผู้ใช้
func (a *Assistant) WithCalendar(c Calendar) *Assistant {
	a.calendar = c
	return a
}

// eventToolSpec มีให้โมเดลเห็นเฉพาะตอนผู้ใช้เชื่อมปฏิทินแล้ว
func eventToolSpec() ToolSpec {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	return ToolSpec{
		Name: "create_event",
		Description: "Add an appointment, meeting or activity with a specific date/time to the user's Google Calendar " +
			"(e.g. \"พรุ่งนี้บ่ายสองประชุมทีม\", \"เสาร์นี้ 10 โมงนัดหมอฟัน\"). Use create_reminder instead when the user asks to be reminded to do something.",
		Properties: map[string]any{
			"title":    str("Short event title, without the date/time words."),
			"start":    str("Start time, RFC3339 with +07:00, e.g. 2026-10-16T14:00:00+07:00."),
			"end":      str("Optional end time, RFC3339 with +07:00. Leave empty for a 1-hour event."),
			"location": str("Optional place, only if the user said one."),
		},
		Required: []string{"title", "start"},
	}
}

// moveEventToolSpec ใช้เลื่อนนัดที่มีอยู่แล้ว แทนการสร้างนัดใหม่ซ้ำ
func moveEventToolSpec() ToolSpec {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	return ToolSpec{
		Name: "move_event",
		Description: "Reschedule an EXISTING upcoming event in the user's Google Calendar " +
			"(e.g. \"เลื่อนประชุมทีมเป็นบ่ายสาม\", \"ย้ายนัดหมอฟันไปวันศุกร์\"). Never use create_event to move an event. " +
			"Returns ambiguous candidates when several events match.",
		Properties: map[string]any{
			"query":         str("Words from the event title, e.g. \"ประชุม\" or \"หมอฟัน\"."),
			"new_start":     str("New start time, RFC3339 with +07:00. The event keeps its length."),
			"original_date": str("Optional current date of the event (YYYY-MM-DD) if the user said it, to pick between events with the same name."),
		},
		Required: []string{"query", "new_start"},
	}
}

// cancelEventToolSpec ใช้ลบนัดที่มีอยู่ เมื่อผู้ใช้บอกว่ายกเลิก/ลบนัด
func cancelEventToolSpec() ToolSpec {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	return ToolSpec{
		Name: "cancel_event",
		Description: "Delete an EXISTING upcoming event from the user's Google Calendar when the user says it is cancelled " +
			"(e.g. \"ยกเลิกประชุมทีมพรุ่งนี้\", \"ลบนัดหมอฟัน\", \"ไม่ไปกินข้าวกับแม่แล้ว\"). Returns ambiguous candidates when several events match.",
		Properties: map[string]any{
			"query":         str("Words from the event title, e.g. \"ประชุม\" or \"หมอฟัน\"."),
			"original_date": str("Optional date of the event (YYYY-MM-DD) if the user said it, to pick between events with the same name."),
		},
		Required: []string{"query"},
	}
}

func toolsFor(calendarOn bool) []ToolSpec {
	specs := toolSpecs()
	if calendarOn {
		specs = append(specs, eventToolSpec(), moveEventToolSpec(), cancelEventToolSpec())
	}
	return specs
}

func (e *executor) createEvent(ctx context.Context, input json.RawMessage) (any, error) {
	if !e.calendarOn {
		return nil, badInput("google calendar is not connected; use create_reminder or create_note instead")
	}
	var in struct {
		Title    string `json:"title"`
		Start    string `json:"start"`
		End      string `json:"end"`
		Location string `json:"location"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, badInput("invalid input: %v", err)
	}
	title, err := cleanText(in.Title, "title")
	if err != nil {
		return nil, err
	}
	if asksToReschedule(e.userText) {
		return nil, badInput("the user wants to move an existing event, not add a new one: call move_event instead")
	}
	if !mentionsTime(e.userText) {
		return nil, badInput("the user did not say any date or time, so do not create an event and do not invent a time. " +
			"Save it with create_note instead and tell the user they can send a day or time to add it to the calendar")
	}
	start, err := time.Parse(time.RFC3339, strings.TrimSpace(in.Start))
	if err != nil {
		return nil, badInput("start must be RFC3339, e.g. 2026-10-16T14:00:00+07:00")
	}
	fixed := correctClock(correctDay(start, e.userText, e.now), e.userText)
	shift := fixed.Sub(start)
	start = fixed
	if start.Before(e.now.Add(-reminderPastTolerance)) {
		return nil, badInput("start %s is in the past (now is %s); ask the user for a future time",
			start.In(bangkok).Format(time.RFC3339), e.now.Format(time.RFC3339))
	}

	end := start.Add(defaultEventDuration)
	if s := strings.TrimSpace(in.End); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			t = t.Add(shift) // เลื่อนตามเวลาเริ่มที่ถูกแก้ ความยาวนัดคงเดิม
			if t.After(start) && t.Sub(start) <= maxEventDuration {
				end = t
			}
		}
	}

	callCtx, cancel := context.WithTimeout(ctx, calendarCallTimeout)
	defer cancel()
	created, err := e.calendar.CreateEvent(callCtx, e.userID, integrations.Event{
		Title:    title,
		Location: strings.TrimSpace(in.Location),
		Start:    start.In(bangkok),
		End:      end.In(bangkok),
	})
	if err != nil {
		return nil, err
	}
	e.calendarEventIDs = append(e.calendarEventIDs, created.ID)
	return map[string]any{
		"added_to": "Google Calendar",
		"title":    title,
		"when":     thaiDateTime(start),
		"until":    end.In(bangkok).Format("15:04"),
	}, nil
}

// mirrorReminder ลง reminder ใน Google Calendar ด้วย ให้ Google เตือนในมือถือ (แทน LINE push ที่เสียเงิน)
// ถ้าล้มเหลวไม่ทำให้ reminder ล้มเหลว: reminder ยังอยู่ในระบบเรา
func (e *executor) mirrorReminder(ctx context.Context, text string, at time.Time) bool {
	if !e.calendarOn {
		return false
	}
	callCtx, cancel := context.WithTimeout(ctx, calendarCallTimeout)
	defer cancel()
	created, err := e.calendar.CreateEvent(callCtx, e.userID, integrations.Event{
		Title:        "⏰ " + text,
		Start:        at.In(bangkok),
		End:          at.In(bangkok).Add(reminderEventDuration),
		PopupAtStart: true,
	})
	if err != nil {
		log.Println("ลง reminder ใน Google Calendar ล้มเหลว:", err)
		return false
	}
	e.calendarEventIDs = append(e.calendarEventIDs, created.ID)
	return true
}

// calendarAgenda คืนนัดจากปฏิทินในช่วงเวลา สำหรับ list_agenda (ล้มเหลว = ไม่มีนัดจากปฏิทิน แต่ยังตอบจากโน้ตได้)
func (e *executor) calendarAgenda(ctx context.Context, from, to time.Time) []noteView {
	if !e.calendarOn {
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, calendarCallTimeout)
	defer cancel()
	events, err := e.calendar.ListEvents(callCtx, e.userID, from, to)
	if err != nil {
		log.Println("อ่านนัดจาก Google Calendar ล้มเหลว:", err)
		return nil
	}
	views := make([]noteView, 0, len(events))
	for i, ev := range events {
		if i >= calendarAgendaMaxEvents {
			break
		}
		v := noteView{Type: "calendar_event", Content: ev.Title}
		if ev.AllDay {
			v.DueDate = ev.Start.Format("2006-01-02")
		} else {
			v.RemindAt = ev.Start.In(bangkok).Format(time.RFC3339)
			v.Created = thaiDateTime(ev.Start)
		}
		views = append(views, v)
	}
	return views
}

// deleteCalendarEvents ใช้ตอนกดยกเลิก; คืนจำนวนที่ลบสำเร็จ
func (a *Assistant) deleteCalendarEvents(ctx context.Context, userID string, ids []string) int {
	if a.calendar == nil || len(ids) == 0 {
		return 0
	}
	deleted := 0
	for _, id := range ids {
		callCtx, cancel := context.WithTimeout(ctx, calendarCallTimeout)
		err := a.calendar.DeleteEvent(callCtx, userID, id)
		cancel()
		if err != nil {
			log.Println("ลบนัดใน Google Calendar ล้มเหลว:", err)
			continue
		}
		deleted++
	}
	return deleted
}

// rescheduleWords บอกว่าผู้ใช้ขอเลื่อนนัดเดิม (ไม่ใช้ "ย้าย" เดี่ยวๆ เพราะ "ย้ายบ้าน" เป็นนัดใหม่ได้)
var rescheduleWords = []string{"เลื่อน", "เปลี่ยนเวลา", "เปลี่ยนวัน", "ย้ายนัด", "ย้ายเวลา", "ย้ายประชุม", "reschedule", "postpone"}

func asksToReschedule(text string) bool {
	lower := strings.ToLower(text)
	for _, w := range rescheduleWords {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

// cancelWords บอกว่าผู้ใช้ขอยกเลิก/ลบนัดเอง ถ้าไม่มีคำเหล่านี้ cancel_event จะไม่ลบอะไร (กันโมเดลลบนัดเอง)
var cancelWords = []string{"ยกเลิก", "ลบนัด", "ลบประชุม", "ลบออก", "ไม่ไปแล้ว", "ไม่ได้ไปแล้ว", "ไม่มีแล้ว", "ไม่มีประชุม", "งดประชุม", "cancel", "delete", "remove"}

func asksToCancel(text string) bool {
	lower := strings.ToLower(strings.ReplaceAll(text, "ยกเลิกติ๊ก", "")) // "ยกเลิกติ๊ก" = เอาติ๊กออก ไม่ใช่ลบนัด
	for _, w := range cancelWords {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

// cancelledEvent เก็บรายละเอียดนัดที่ลบไว้ สร้างกลับตอนกดยกเลิก
type cancelledEvent struct {
	Title       string    `json:"title"`
	Location    string    `json:"location,omitempty"`
	Description string    `json:"description,omitempty"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	AllDay      bool      `json:"all_day,omitempty"`
}

func (e *executor) cancelEvent(ctx context.Context, input json.RawMessage) (any, error) {
	if !e.calendarOn {
		return nil, badInput("google calendar is not connected")
	}
	var in struct {
		Query        string `json:"query"`
		OriginalDate string `json:"original_date"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, badInput("invalid input: %v", err)
	}
	if !asksToCancel(e.userText) {
		return nil, badInput("the user did not ask to cancel or delete an event; do not delete anything")
	}
	callCtx, cancel := context.WithTimeout(ctx, calendarCallTimeout)
	defer cancel()
	ev, options, err := e.findEvent(callCtx, in.Query, in.OriginalDate, true)
	if err != nil {
		return nil, err
	}
	if options != nil {
		return map[string]any{"cancelled": false, "ambiguous": options}, nil
	}
	if err := e.calendar.DeleteEvent(callCtx, e.userID, ev.ID); err != nil {
		return nil, err
	}
	e.cancelledEvents = append(e.cancelledEvents, cancelledEvent{
		Title: ev.Title, Location: ev.Location, Description: ev.Description, Start: ev.Start, End: ev.End, AllDay: ev.AllDay,
	})
	return map[string]any{"cancelled": true, "title": ev.Title, "when": eventWhen(ev)}, nil
}

// restoreCancelledEvents สร้างนัดที่ลบไปกลับมาตอนกดยกเลิก (เป็น event ใหม่ใน Google); คืนจำนวนที่สำเร็จ
func (a *Assistant) restoreCancelledEvents(ctx context.Context, userID string, events []cancelledEvent) int {
	if a.calendar == nil {
		return 0
	}
	restored := 0
	for _, c := range events {
		ev := integrations.Event{Title: c.Title, Location: c.Location, Description: c.Description, Start: c.Start, End: c.End, AllDay: c.AllDay}
		if !c.AllDay {
			ev.Start, ev.End = c.Start.In(bangkok), c.End.In(bangkok)
		}
		callCtx, cancel := context.WithTimeout(ctx, calendarCallTimeout)
		_, err := a.calendar.CreateEvent(callCtx, userID, ev)
		cancel()
		if err != nil {
			log.Println("สร้างนัดที่ถูกลบกลับคืนใน Google Calendar ล้มเหลว:", err)
			continue
		}
		restored++
	}
	return restored
}

// eventMove คือการเลื่อนนัดหนึ่งครั้ง เก็บเวลาเดิมไว้ย้ายกลับตอนกดยกเลิก
type eventMove struct {
	EventID   string    `json:"event_id"`
	PrevStart time.Time `json:"prev_start"`
	PrevEnd   time.Time `json:"prev_end"`
}

const eventSearchWindow = 60 * 24 * time.Hour

// moveTargetText คือส่วนของข้อความที่บอกเวลาใหม่ เช่น "เลื่อนประชุมพรุ่งนี้บ่ายสองเป็นบ่ายสาม" → "บ่ายสาม"
// ใช้แก้เวลาที่โมเดลให้มา โดยไม่เอาเวลาเดิมของนัดมาปน
func moveTargetText(text string) string {
	best := -1
	marker := ""
	for _, m := range []string{"เป็น", "ไป", "to "} {
		if i := strings.LastIndex(text, m); i > best {
			best, marker = i, m
		}
	}
	if best < 0 {
		return text
	}
	return text[best+len(marker):]
}

func normalizeTitle(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), ""))
}

// findEvent หานัดที่กำลังจะมาถึงจากคำในชื่อนัด
// เจออันเดียว → คืน event; หลายอัน → คืนตัวเลือก (ambiguous) ให้โมเดลถามผู้ใช้; ไม่เจอ → badInput
func (e *executor) findEvent(ctx context.Context, rawQuery, originalDate string, allowAllDay bool) (integrations.Event, []map[string]string, error) {
	query := normalizeTitle(rawQuery)
	if query == "" {
		return integrations.Event{}, nil, badInput("query is empty")
	}
	events, err := e.calendar.ListEvents(ctx, e.userID, e.now.Add(-2*time.Hour), e.now.Add(eventSearchWindow))
	if err != nil {
		return integrations.Event{}, nil, err
	}
	var matches []integrations.Event
	for _, ev := range events {
		title := normalizeTitle(ev.Title)
		// นัดที่เป็น "⏰" คือเงาของ reminder ในระบบเรา แก้ตรงนี้แล้วจะไม่ตรงกับ reminder
		if (ev.AllDay && !allowAllDay) || strings.HasPrefix(ev.Title, "⏰") || title == "" {
			continue
		}
		if strings.Contains(title, query) || strings.Contains(query, title) {
			matches = append(matches, ev)
		}
	}
	if d := strings.TrimSpace(originalDate); d != "" && len(matches) > 1 {
		var sameDay []integrations.Event
		for _, ev := range matches {
			if eventDate(ev) == d {
				sameDay = append(sameDay, ev)
			}
		}
		if len(sameDay) > 0 {
			matches = sameDay
		}
	}
	switch {
	case len(matches) == 0:
		return integrations.Event{}, nil, badInput("no upcoming event in Google Calendar matches %q; tell the user you could not find it and do not create a new event", rawQuery)
	case len(matches) > 1:
		options := make([]map[string]string, 0, len(matches))
		for i, ev := range matches {
			if i >= 5 {
				break
			}
			options = append(options, map[string]string{"title": ev.Title, "when": eventWhen(ev)})
		}
		return integrations.Event{}, options, nil
	}
	return matches[0], nil, nil
}

// eventDate คือวันที่ของนัดตามเวลาไทย (นัดทั้งวันใช้วันที่ตามที่ Google ส่งมาตรงๆ)
func eventDate(ev integrations.Event) string {
	if ev.AllDay {
		return ev.Start.Format("2006-01-02")
	}
	return ev.Start.In(bangkok).Format("2006-01-02")
}

func eventWhen(ev integrations.Event) string {
	if ev.AllDay {
		return thaiDate(ev.Start) + " (ทั้งวัน)"
	}
	return thaiDateTime(ev.Start)
}

func (e *executor) moveEvent(ctx context.Context, input json.RawMessage) (any, error) {
	if !e.calendarOn {
		return nil, badInput("google calendar is not connected")
	}
	var in struct {
		Query        string `json:"query"`
		NewStart     string `json:"new_start"`
		OriginalDate string `json:"original_date"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, badInput("invalid input: %v", err)
	}
	target := moveTargetText(e.userText)
	if !mentionsTime(target) {
		return nil, badInput("the user did not say the new date or time; ask them when to move it to")
	}
	newStart, err := time.Parse(time.RFC3339, strings.TrimSpace(in.NewStart))
	if err != nil {
		return nil, badInput("new_start must be RFC3339, e.g. 2026-10-16T15:00:00+07:00")
	}

	callCtx, cancel := context.WithTimeout(ctx, calendarCallTimeout)
	defer cancel()
	ev, options, err := e.findEvent(callCtx, in.Query, in.OriginalDate, false)
	if err != nil {
		return nil, err
	}
	if options != nil {
		return map[string]any{"moved": false, "ambiguous": options}, nil
	}

	// แก้วัน/เวลาจากส่วนที่บอกเวลาใหม่เท่านั้น ถ้าไม่ได้บอกวันใหม่ ให้อยู่วันเดิมของนัด
	if _, ok := parseRelativeDay(target); ok {
		newStart = correctDay(newStart, target, e.now)
	} else if !mentionsDay(target) {
		old := ev.Start.In(bangkok)
		local := newStart.In(bangkok)
		newStart = time.Date(old.Year(), old.Month(), old.Day(), local.Hour(), local.Minute(), 0, 0, bangkok)
	}
	newStart = correctClock(newStart, target)
	if newStart.Before(e.now.Add(-reminderPastTolerance)) {
		return nil, badInput("new start %s is in the past; ask the user for a future time", newStart.In(bangkok).Format(time.RFC3339))
	}
	length := ev.End.Sub(ev.Start)
	if length <= 0 || length > maxEventDuration {
		length = defaultEventDuration
	}

	moved, err := e.calendar.MoveEvent(callCtx, e.userID, ev.ID, newStart.In(bangkok), newStart.In(bangkok).Add(length))
	if err != nil {
		return nil, err
	}
	e.calendarMoves = append(e.calendarMoves, eventMove{EventID: ev.ID, PrevStart: ev.Start, PrevEnd: ev.End})
	return map[string]any{
		"moved": true,
		"title": ev.Title,
		"from":  thaiDateTime(ev.Start),
		"to":    thaiDateTime(moved.Start),
		"until": moved.End.In(bangkok).Format("15:04"),
	}, nil
}

// restoreCalendarMoves ย้ายนัดกลับเวลาเดิมตอนกดยกเลิก (ย้อนจากหลังไปหน้า); คืนจำนวนที่สำเร็จ
func (a *Assistant) restoreCalendarMoves(ctx context.Context, userID string, moves []eventMove) int {
	if a.calendar == nil {
		return 0
	}
	restored := 0
	for i := len(moves) - 1; i >= 0; i-- {
		m := moves[i]
		callCtx, cancel := context.WithTimeout(ctx, calendarCallTimeout)
		_, err := a.calendar.MoveEvent(callCtx, userID, m.EventID, m.PrevStart.In(bangkok), m.PrevEnd.In(bangkok))
		cancel()
		if err != nil {
			log.Println("ย้ายนัดกลับเวลาเดิมใน Google Calendar ล้มเหลว:", err)
			continue
		}
		restored++
	}
	return restored
}
