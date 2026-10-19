package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"jodjod/internal/note"
)

const (
	maxContentRunes = 2000
	maxListItems    = 50

	fuzzyMaxChoices   = 5
	fuzzyConfidentGap = 0.15

	// ยอมให้ remind_at ย้อนหลังได้นิดหน่อย เผื่อเวลาที่ใช้ระหว่างเรียก API
	reminderPastTolerance = time.Minute
)

// errToolInput คือ input ผิดที่โมเดลแก้เองได้ — ส่งกลับเป็น tool_result is_error ให้โมเดลลองใหม่/ถามผู้ใช้
type errToolInput struct{ msg string }

func (e errToolInput) Error() string { return e.msg }

func badInput(format string, args ...any) error {
	return errToolInput{msg: fmt.Sprintf(format, args...)}
}

// toolSpecs คืนนิยาม tool ทั้งหมด ลำดับคงที่เพื่อให้ prompt cache ใช้ซ้ำได้
func toolSpecs() []ToolSpec {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	strArray := func(desc string) map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
	}
	// pin ใส่ได้ตอนสร้าง: โมเดลเล็กสร้างแล้วเรียก pin_notes ต่อไม่ค่อยครบ จึงให้ปักในการเรียกเดียวกัน
	pin := map[string]any{"type": "boolean", "description": "true only when the user asks to pin it to the widget (ปักไว้/ปักหมุด/เอาขึ้น widget)."}
	tool := func(name, desc string, props map[string]any, required ...string) ToolSpec {
		if required == nil {
			required = []string{}
		}
		return ToolSpec{Name: name, Description: desc, Properties: props, Required: required}
	}

	return []ToolSpec{
		tool("create_note",
			"Save a plain note for the user. Use for anything that is not a list, a timed reminder, a completion, or a question.",
			map[string]any{
				"content":  str("The note text, without command words like 'จดว่า'."),
				"due_date": str("Optional due date as YYYY-MM-DD (Asia/Bangkok) when the user mentions a deadline but no reminder time."),
				"pin":      pin,
			},
			"content"),
		tool("add_to_list",
			"Add one or more items to a checklist. Reuses the user's open list with the same name if there is one, otherwise creates a new list.",
			map[string]any{
				"list_name": str("List name. Reuse an exact name from the open lists in <context> when the meaning matches."),
				"items":     strArray("Items to add, one per element, without bullets or numbering."),
				"pin":       pin,
			},
			"list_name", "items"),
		tool("create_reminder",
			"Create a reminder that the assistant will send to the user at a specific time.",
			map[string]any{
				"text":      str("What to remind the user about."),
				"remind_at": str("When to remind, RFC3339 with +07:00 offset, e.g. 2026-10-16T09:00:00+07:00."),
				"pin":       pin,
			},
			"text", "remind_at"),
		tool("complete_items",
			"Mark checklist items as done (or not done) by fuzzy-matching their text. Returns ambiguous candidates when unsure.",
			map[string]any{
				"queries": strArray("Item texts to match, e.g. [\"นม\", \"ไข่\"]."),
				"done":    map[string]any{"type": "boolean", "description": "true = mark done, false = mark not done."},
			},
			"queries", "done"),
		tool("pin_notes",
			"Pin notes or lists to the user's iPhone home-screen widget (pinned=true), or unpin them (pinned=false). "+
				"Matches notes by their title/text. Returns ambiguous candidates when unsure.",
			map[string]any{
				"queries": strArray("Titles or text of the notes/lists to pin, e.g. [\"ซื้อของ\", \"เลขห้องประชุม\"]."),
				"pinned":  map[string]any{"type": "boolean", "description": "true = pin to the widget, false = unpin."},
			},
			"queries", "pinned"),
		tool("list_pinned",
			"List the notes and lists currently pinned to the widget. Read-only.",
			map[string]any{
				// โมเดลเล็กเรียก tool ที่ไม่มี parameter เลยได้ไม่ค่อยดี จึงมีตัวกรองชนิดให้เลือก
				"kind": map[string]any{
					"type": "string", "enum": []string{"all", "checklist", "note", "reminder"},
					"description": "Which pinned items to list; use \"all\" unless the user asks for one kind.",
				},
			},
			"kind"),
		tool("search_notes",
			"Search the user's notes and lists by keyword. Read-only.",
			map[string]any{
				"query": str("Keyword to search for."),
			},
			"query"),
		tool("list_agenda",
			"List reminders and notes with due dates between two dates (inclusive). Read-only.",
			map[string]any{
				"from": str("Start date YYYY-MM-DD (Asia/Bangkok)."),
				"to":   str("End date YYYY-MM-DD (Asia/Bangkok), inclusive."),
			},
			"from", "to"),
	}
}

// toolRun เก็บผลของ tool call หนึ่งครั้ง สำหรับตอบโมเดลและบันทึกลง ai_actions
type toolRun struct {
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	Result  any             `json:"result,omitempty"`
	Error   string          `json:"error,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
}

// itemChange บันทึกสถานะเดิมของ item เพื่อให้ undo ย้อนกลับได้
type itemChange struct {
	ItemID       string `json:"item_id"`
	Text         string `json:"text"`
	PreviousDone bool   `json:"previous_done"`
	Done         bool   `json:"done"`
}

// pinChange บันทึกสถานะปักหมุดเดิมของโน้ต เพื่อให้ undo ย้อนกลับได้ (คืนเวลาที่ปักเดิมด้วย)
type pinChange struct {
	NoteID           string     `json:"note_id"`
	Content          string     `json:"content"`
	PreviousPinnedAt *time.Time `json:"previous_pinned_at"`
	Pinned           bool       `json:"pinned"`
}

// executor รัน tool ของ user หนึ่งคน และสะสมสิ่งที่ถูกเขียนลง DB ไว้ทำ undo
type executor struct {
	store  NoteStore
	userID string
	now    time.Time
	// userText คือข้อความต้นฉบับของผู้ใช้ ใช้ตรวจว่าผู้ใช้บอกเวลาเองจริงก่อนตั้งเตือน
	userText string

	savedNote     bool // มีการ create_note สำเร็จ
	savedReminder bool // มีการ create_reminder สำเร็จ

	// calendar = ปฏิทินของผู้ใช้ (nil ได้); calendarOn = ผู้ใช้เชื่อมไว้แล้ว
	calendar   Calendar
	calendarOn bool

	createdNoteIDs []string
	addedItemIDs   []string
	itemChanges    []itemChange
	pinChanges     []pinChange
	// event ใน Google Calendar ที่สร้างในข้อความนี้ (ลบตอนกดยกเลิก)
	calendarEventIDs []string
	// นัดที่ถูกเลื่อนในข้อความนี้ (ย้ายกลับตอนกดยกเลิก)
	calendarMoves []eventMove
	// นัดที่ถูกลบในข้อความนี้ (สร้างกลับตอนกดยกเลิก)
	cancelledEvents []cancelledEvent
}

func (e *executor) wrote() bool {
	return len(e.createdNoteIDs) > 0 || len(e.addedItemIDs) > 0 || len(e.itemChanges) > 0 || len(e.pinChanges) > 0 || len(e.calendarEventIDs) > 0 || len(e.calendarMoves) > 0 || len(e.cancelledEvents) > 0
}

// run เรียก tool ตามชื่อ; error ชนิด errToolInput คือ input ผิด ส่วน error อื่นคือระบบผิดพลาด
func (e *executor) run(ctx context.Context, name string, input json.RawMessage) (any, error) {
	switch name {
	case "create_note":
		var in struct {
			Content string `json:"content"`
			DueDate string `json:"due_date"`
			Pin     bool   `json:"pin"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, badInput("invalid input: %v", err)
		}
		return e.withPin(ctx, in.Pin)(e.createNote(ctx, in.Content, in.DueDate))
	case "add_to_list":
		var in struct {
			ListName string   `json:"list_name"`
			Items    []string `json:"items"`
			Pin      bool     `json:"pin"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, badInput("invalid input: %v", err)
		}
		return e.withPin(ctx, in.Pin)(e.addToList(ctx, in.ListName, in.Items))
	case "create_reminder":
		var in struct {
			Text     string `json:"text"`
			RemindAt string `json:"remind_at"`
			Pin      bool   `json:"pin"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, badInput("invalid input: %v", err)
		}
		return e.withPin(ctx, in.Pin)(e.createReminder(ctx, in.Text, in.RemindAt))
	case "move_event":
		return e.moveEvent(ctx, input)
	case "cancel_event":
		return e.cancelEvent(ctx, input)
	case "complete_items":
		var in struct {
			Queries []string `json:"queries"`
			Done    bool     `json:"done"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, badInput("invalid input: %v", err)
		}
		return e.completeItems(ctx, in.Queries, in.Done)
	case "pin_notes":
		var in struct {
			Queries []string `json:"queries"`
			Pinned  bool     `json:"pinned"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, badInput("invalid input: %v", err)
		}
		return e.pinNotes(ctx, in.Queries, in.Pinned)
	case "list_pinned":
		var in struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, badInput("invalid input: %v", err)
		}
		return e.listPinned(ctx, in.Kind)
	case "search_notes":
		var in struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, badInput("invalid input: %v", err)
		}
		return e.searchNotes(ctx, in.Query)
	case "list_agenda":
		var in struct {
			From string `json:"from"`
			To   string `json:"to"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, badInput("invalid input: %v", err)
		}
		return e.listAgenda(ctx, in.From, in.To)
	case "create_event":
		return e.createEvent(ctx, input)
	default:
		return nil, badInput("unknown tool %q", name)
	}
}

func cleanText(s, field string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", badInput("%s is empty", field)
	}
	if utf8.RuneCountInString(s) > maxContentRunes {
		return "", badInput("%s is longer than %d characters", field, maxContentRunes)
	}
	return s, nil
}

func (e *executor) createNote(ctx context.Context, content, dueDate string) (any, error) {
	content, err := cleanText(content, "content")
	if err != nil {
		return nil, err
	}
	n := &note.Note{UserID: e.userID, Content: &content, Type: note.NoteTypeNote, Source: note.NoteSourceAI}
	if strings.TrimSpace(dueDate) != "" {
		d, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(dueDate), bangkok)
		if err != nil {
			return nil, badInput("due_date must be YYYY-MM-DD")
		}
		n.DueDate = &d
	}
	if err := e.store.SaveNote(ctx, n); err != nil {
		return nil, err
	}
	e.savedNote = true
	e.createdNoteIDs = append(e.createdNoteIDs, n.ID)
	return map[string]any{"note_id": n.ID, "saved": content}, nil
}

func (e *executor) addToList(ctx context.Context, listName string, items []string) (any, error) {
	listName, err := cleanText(listName, "list_name")
	if err != nil {
		return nil, err
	}
	clean := make([]string, 0, len(items))
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" {
			continue
		}
		if utf8.RuneCountInString(it) > maxContentRunes {
			return nil, badInput("item is longer than %d characters", maxContentRunes)
		}
		clean = append(clean, it)
	}
	if len(clean) == 0 {
		return nil, badInput("items is empty")
	}
	if len(clean) > maxListItems {
		return nil, badInput("too many items (max %d)", maxListItems)
	}

	existing, err := e.store.FindOpenChecklistByTitle(ctx, e.userID, listName)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		n, err := e.store.SaveChecklist(ctx, e.userID, &listName, clean, note.NoteSourceAI)
		if err != nil {
			return nil, err
		}
		e.createdNoteIDs = append(e.createdNoteIDs, n.ID)
		return map[string]any{"note_id": n.ID, "list": listName, "created_new_list": true, "added": clean}, nil
	}

	added, err := e.store.AppendItems(ctx, e.userID, existing.ID, clean)
	if err != nil {
		return nil, err
	}
	for _, it := range added {
		e.addedItemIDs = append(e.addedItemIDs, it.ID)
	}
	return map[string]any{"note_id": existing.ID, "list": listName, "created_new_list": false, "added": clean,
		"previous_pinned_at": existing.PinnedAt}, nil
}

func (e *executor) createReminder(ctx context.Context, text, remindAt string) (any, error) {
	text, err := cleanText(text, "text")
	if err != nil {
		return nil, err
	}
	if !mentionsTime(e.userText) {
		return nil, badInput("the user did not say any date or time, so do not create a reminder and do not invent a time. " +
			"Save it with create_note instead, then tell the user it was noted and that they can send a day or time (e.g. \"พรุ่งนี้ 9 โมง\") to get a reminder")
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(remindAt))
	if err != nil {
		return nil, badInput("remind_at must be RFC3339, e.g. 2026-10-16T09:00:00+07:00")
	}
	at = correctClock(correctDay(at, e.userText, e.now), e.userText)
	if at.Before(e.now.Add(-reminderPastTolerance)) {
		return nil, badInput("remind_at %s is in the past (now is %s); ask the user for a future time",
			at.In(bangkok).Format(time.RFC3339), e.now.Format(time.RFC3339))
	}
	n := &note.Note{UserID: e.userID, Content: &text, Type: note.NoteTypeReminder, RemindAt: &at, Source: note.NoteSourceAI}
	if err := e.store.SaveNote(ctx, n); err != nil {
		return nil, err
	}
	e.savedReminder = true
	e.createdNoteIDs = append(e.createdNoteIDs, n.ID)
	result := map[string]any{"note_id": n.ID, "text": text, "remind_at": at.In(bangkok).Format(time.RFC3339), "when": thaiDateTime(at)}
	if e.calendarOn {
		result["also_in_google_calendar"] = e.mirrorReminder(ctx, text, at)
	}
	return result, nil
}

type candidate struct {
	Text string `json:"text"`
	List string `json:"list"`
	Date string `json:"date"`
	Done bool   `json:"done"`
}

func (e *executor) completeItems(ctx context.Context, queries []string, done bool) (any, error) {
	var (
		changed   []string
		ambiguous = map[string][]candidate{}
		notFound  []string
	)
	for _, q := range queries {
		q = strings.TrimSpace(q)
		if q == "" {
			continue
		}
		matches, err := e.store.FindItemsBySimilarText(ctx, e.userID, q, fuzzyMaxChoices)
		if err != nil {
			return nil, err
		}
		best, ok := pickConfident(matches, done)
		if !ok {
			if len(matches) == 0 {
				notFound = append(notFound, q)
				continue
			}
			for _, m := range matches {
				ambiguous[q] = append(ambiguous[q], candidate{
					Text: m.Item.Text, List: m.NoteContent,
					Date: m.NoteCreated.In(bangkok).Format("2006-01-02"), Done: m.Item.IsDone,
				})
			}
			continue
		}
		if best.Item.IsDone == done {
			changed = append(changed, best.Item.Text)
			continue // สถานะตรงอยู่แล้ว ไม่ต้องเขียน
		}
		if err := e.store.SetItemDone(ctx, e.userID, best.Item.ID, done); err != nil {
			return nil, err
		}
		e.itemChanges = append(e.itemChanges, itemChange{
			ItemID: best.Item.ID, Text: best.Item.Text, PreviousDone: best.Item.IsDone, Done: done,
		})
		changed = append(changed, best.Item.Text)
	}

	result := map[string]any{"done": done, "updated": changed}
	if len(ambiguous) > 0 {
		result["ambiguous"] = ambiguous
	}
	if len(notFound) > 0 {
		result["not_found"] = notFound
	}
	return result, nil
}

// pickConfident เลือก item ที่แน่ใจ: มีตัวเดียว หรือคะแนนห่างจากอันดับสองพอ
// ถ้าหลายอันใกล้กัน แต่มีตัวเดียวที่สถานะยังไม่ตรงกับที่ต้องการ ให้เลือกตัวนั้น
func pickConfident(matches []*note.ItemMatch, done bool) (*note.ItemMatch, bool) {
	switch {
	case len(matches) == 0:
		return nil, false
	case len(matches) == 1:
		return matches[0], true
	case matches[0].Similarity-matches[1].Similarity >= fuzzyConfidentGap:
		return matches[0], true
	}
	var pending []*note.ItemMatch
	for _, m := range matches {
		if matches[0].Similarity-m.Similarity < fuzzyConfidentGap && m.Item.IsDone != done {
			pending = append(pending, m)
		}
	}
	if len(pending) == 1 {
		return pending[0], true
	}
	return nil, false
}

type noteView struct {
	Type     string   `json:"type"`
	Content  string   `json:"content"`
	Created  string   `json:"created"`
	RemindAt string   `json:"remind_at,omitempty"`
	DueDate  string   `json:"due_date,omitempty"`
	Pinned   bool     `json:"pinned,omitempty"`
	Items    []string `json:"items,omitempty"`
}

func (e *executor) viewNotes(ctx context.Context, notes []*note.Note) ([]noteView, error) {
	views := make([]noteView, 0, len(notes))
	for _, n := range notes {
		v := noteView{Type: string(n.Type), Created: n.CreatedAt.In(bangkok).Format("2006-01-02 15:04"), Pinned: n.PinnedAt != nil}
		if n.Content != nil {
			v.Content = *n.Content
		}
		if n.RemindAt != nil {
			v.RemindAt = n.RemindAt.In(bangkok).Format(time.RFC3339)
		}
		if n.DueDate != nil {
			v.DueDate = n.DueDate.In(bangkok).Format("2006-01-02")
		}
		if n.Type == note.NoteTypeChecklist {
			items, err := e.store.FindItemsByNoteID(ctx, n.ID)
			if err != nil {
				return nil, err
			}
			for _, it := range items {
				box := "☐ "
				if it.IsDone {
					box = "☑ "
				}
				v.Items = append(v.Items, box+it.Text)
			}
		}
		views = append(views, v)
	}
	return views, nil
}

func (e *executor) searchNotes(ctx context.Context, query string) (any, error) {
	query, err := cleanText(query, "query")
	if err != nil {
		return nil, err
	}
	notes, err := e.store.SearchByUser(ctx, e.userID, query)
	if err != nil {
		return nil, err
	}
	views, err := e.viewNotes(ctx, notes)
	if err != nil {
		return nil, err
	}
	return map[string]any{"query": query, "count": len(views), "notes": views}, nil
}

func (e *executor) listAgenda(ctx context.Context, fromStr, toStr string) (any, error) {
	from, err1 := time.ParseInLocation("2006-01-02", strings.TrimSpace(fromStr), bangkok)
	to, err2 := time.ParseInLocation("2006-01-02", strings.TrimSpace(toStr), bangkok)
	if err := errors.Join(err1, err2); err != nil {
		return nil, badInput("from/to must be YYYY-MM-DD")
	}
	if to.Before(from) {
		return nil, badInput("to is before from")
	}
	if to.Sub(from) > 366*24*time.Hour {
		return nil, badInput("range is longer than one year")
	}
	notes, err := e.store.FindAgenda(ctx, e.userID, from, to.AddDate(0, 0, 1))
	if err != nil {
		return nil, err
	}
	views, err := e.viewNotes(ctx, notes)
	if err != nil {
		return nil, err
	}
	views = append(views, e.calendarAgenda(ctx, from, to.AddDate(0, 0, 1))...)
	return map[string]any{"from": fromStr, "to": toStr, "count": len(views), "items": views}, nil
}

// withPin ปักโน้ตที่ tool เพิ่งสร้าง/เพิ่มของเข้าไป เมื่อผู้ใช้ขอให้ปัก (ช่อง pin)
// ใช้ห่อผลของ tool: e.withPin(ctx, pin)(e.createNote(...))
func (e *executor) withPin(ctx context.Context, pin bool) func(any, error) (any, error) {
	return func(result any, err error) (any, error) {
		if err != nil || !pin {
			return result, err
		}
		m, ok := result.(map[string]any)
		id, _ := m["note_id"].(string)
		if !ok || id == "" {
			return result, nil
		}
		prev, _ := m["previous_pinned_at"].(*time.Time)
		delete(m, "previous_pinned_at")
		if prev == nil {
			if err := e.store.SetPinned(ctx, e.userID, id, true); err != nil {
				return nil, err
			}
			content, _ := m["saved"].(string)
			if content == "" {
				content, _ = m["text"].(string)
			}
			if content == "" {
				content, _ = m["list"].(string)
			}
			e.pinChanges = append(e.pinChanges, pinChange{NoteID: id, Content: content, Pinned: true})
		}
		m["pinned"] = true
		return m, nil
	}
}

type pinCandidate struct {
	Content string `json:"content"`
	Type    string `json:"type"`
	Date    string `json:"date"`
	Pinned  bool   `json:"pinned"`
}

func (e *executor) pinNotes(ctx context.Context, queries []string, pinned bool) (any, error) {
	var (
		changed   []string
		ambiguous = map[string][]pinCandidate{}
		notFound  []string
	)
	for _, q := range queries {
		q = strings.TrimSpace(q)
		if q == "" {
			continue
		}
		matches, err := e.store.SearchByUser(ctx, e.userID, q)
		if err != nil {
			return nil, err
		}
		best, ok := pickPinTarget(matches, q, pinned)
		if !ok {
			if len(matches) == 0 {
				notFound = append(notFound, q)
				continue
			}
			for i, m := range matches {
				if i == fuzzyMaxChoices {
					break
				}
				ambiguous[q] = append(ambiguous[q], pinCandidate{
					Content: noteContent(m), Type: string(m.Type),
					Date: m.CreatedAt.In(bangkok).Format("2006-01-02"), Pinned: m.PinnedAt != nil,
				})
			}
			continue
		}
		if (best.PinnedAt != nil) == pinned {
			changed = append(changed, noteContent(best))
			continue // สถานะตรงอยู่แล้ว ไม่ต้องเขียน
		}
		if err := e.store.SetPinned(ctx, e.userID, best.ID, pinned); err != nil {
			return nil, err
		}
		e.pinChanges = append(e.pinChanges, pinChange{
			NoteID: best.ID, Content: noteContent(best), PreviousPinnedAt: best.PinnedAt, Pinned: pinned,
		})
		changed = append(changed, noteContent(best))
	}

	result := map[string]any{"pinned": pinned, "updated": changed}
	if len(ambiguous) > 0 {
		result["ambiguous"] = ambiguous
	}
	if len(notFound) > 0 {
		result["not_found"] = notFound
	}
	return result, nil
}

// pickPinTarget เลือกโน้ตที่แน่ใจจากผลค้นหา (เรียงใหม่ → เก่า):
// เจอตัวเดียว, ชื่อตรงเป๊ะตัวเดียว, หรือมีตัวเดียวที่ยังไม่อยู่ในสถานะที่ต้องการ
// (ตอนปักจะข้ามโน้ตที่ทำเสร็จแล้ว)
func pickPinTarget(matches []*note.Note, query string, pinned bool) (*note.Note, bool) {
	if len(matches) == 0 {
		return nil, false
	}
	if len(matches) == 1 {
		return matches[0], true
	}
	var exact []*note.Note
	for _, m := range matches {
		if strings.EqualFold(strings.TrimSpace(noteContent(m)), query) {
			exact = append(exact, m)
		}
	}
	if len(exact) == 1 {
		return exact[0], true
	}
	pool := matches
	if len(exact) > 1 {
		pool = exact
	}
	var pending []*note.Note
	for _, m := range pool {
		if (m.PinnedAt != nil) != pinned && !(pinned && m.IsDone) {
			pending = append(pending, m)
		}
	}
	if len(pending) == 1 {
		return pending[0], true
	}
	return nil, false
}

func noteContent(n *note.Note) string {
	if n.Content == nil {
		return ""
	}
	return *n.Content
}

func (e *executor) listPinned(ctx context.Context, kind string) (any, error) {
	q := note.FeedQuery{UserID: e.userID, Desc: true, Limit: 50, PinnedOnly: true}
	switch kind {
	case "", "all":
	case "checklist", "note", "reminder":
		t := note.NoteType(kind)
		q.TypeFilter = &t
	default:
		return nil, badInput("kind must be all, checklist, note or reminder")
	}
	notes, err := e.store.FindFeed(ctx, q)
	if err != nil {
		return nil, err
	}
	views, err := e.viewNotes(ctx, notes)
	if err != nil {
		return nil, err
	}
	return map[string]any{"count": len(views), "notes": views}, nil
}

// toolResultText แปลงผลของ tool เป็นข้อความที่ส่งกลับให้โมเดล
func toolResultText(run toolRun) string {
	if run.IsError {
		return run.Error
	}
	data, err := json.Marshal(run.Result)
	if err != nil {
		return `{"error":"could not encode result"}`
	}
	return string(data)
}
