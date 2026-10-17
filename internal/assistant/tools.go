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
	tool := func(name, desc string, props map[string]any, required ...string) ToolSpec {
		return ToolSpec{Name: name, Description: desc, Properties: props, Required: required}
	}

	return []ToolSpec{
		tool("create_note",
			"Save a plain note for the user. Use for anything that is not a list, a timed reminder, a completion, or a question.",
			map[string]any{
				"content":  str("The note text, without command words like 'จดว่า'."),
				"due_date": str("Optional due date as YYYY-MM-DD (Asia/Bangkok) when the user mentions a deadline but no reminder time."),
			},
			"content"),
		tool("add_to_list",
			"Add one or more items to a checklist. Reuses the user's open list with the same name if there is one, otherwise creates a new list.",
			map[string]any{
				"list_name": str("List name. Reuse an exact name from the open lists in <context> when the meaning matches."),
				"items":     strArray("Items to add, one per element, without bullets or numbering."),
			},
			"list_name", "items"),
		tool("create_reminder",
			"Create a reminder that JodJod will send to the user at a specific time.",
			map[string]any{
				"text":      str("What to remind the user about."),
				"remind_at": str("When to remind, RFC3339 with +07:00 offset, e.g. 2026-10-16T09:00:00+07:00."),
			},
			"text", "remind_at"),
		tool("complete_items",
			"Mark checklist items as done (or not done) by fuzzy-matching their text. Returns ambiguous candidates when unsure.",
			map[string]any{
				"queries": strArray("Item texts to match, e.g. [\"นม\", \"ไข่\"]."),
				"done":    map[string]any{"type": "boolean", "description": "true = mark done, false = mark not done."},
			},
			"queries", "done"),
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

// executor รัน tool ของ user หนึ่งคน และสะสมสิ่งที่ถูกเขียนลง DB ไว้ทำ undo
type executor struct {
	store  NoteStore
	userID string
	now    time.Time

	createdNoteIDs []string
	addedItemIDs   []string
	itemChanges    []itemChange
}

func (e *executor) wrote() bool {
	return len(e.createdNoteIDs) > 0 || len(e.addedItemIDs) > 0 || len(e.itemChanges) > 0
}

// run เรียก tool ตามชื่อ; error ชนิด errToolInput คือ input ผิด ส่วน error อื่นคือระบบผิดพลาด
func (e *executor) run(ctx context.Context, name string, input json.RawMessage) (any, error) {
	switch name {
	case "create_note":
		var in struct {
			Content string `json:"content"`
			DueDate string `json:"due_date"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, badInput("invalid input: %v", err)
		}
		return e.createNote(ctx, in.Content, in.DueDate)
	case "add_to_list":
		var in struct {
			ListName string   `json:"list_name"`
			Items    []string `json:"items"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, badInput("invalid input: %v", err)
		}
		return e.addToList(ctx, in.ListName, in.Items)
	case "create_reminder":
		var in struct {
			Text     string `json:"text"`
			RemindAt string `json:"remind_at"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, badInput("invalid input: %v", err)
		}
		return e.createReminder(ctx, in.Text, in.RemindAt)
	case "complete_items":
		var in struct {
			Queries []string `json:"queries"`
			Done    bool     `json:"done"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, badInput("invalid input: %v", err)
		}
		return e.completeItems(ctx, in.Queries, in.Done)
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
		return map[string]any{"list": listName, "created_new_list": true, "added": clean}, nil
	}

	added, err := e.store.AppendItems(ctx, e.userID, existing.ID, clean)
	if err != nil {
		return nil, err
	}
	for _, it := range added {
		e.addedItemIDs = append(e.addedItemIDs, it.ID)
	}
	return map[string]any{"list": listName, "created_new_list": false, "added": clean}, nil
}

func (e *executor) createReminder(ctx context.Context, text, remindAt string) (any, error) {
	text, err := cleanText(text, "text")
	if err != nil {
		return nil, err
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(remindAt))
	if err != nil {
		return nil, badInput("remind_at must be RFC3339, e.g. 2026-10-16T09:00:00+07:00")
	}
	if at.Before(e.now.Add(-reminderPastTolerance)) {
		return nil, badInput("remind_at %s is in the past (now is %s); ask the user for a future time",
			at.In(bangkok).Format(time.RFC3339), e.now.Format(time.RFC3339))
	}
	n := &note.Note{UserID: e.userID, Content: &text, Type: note.NoteTypeReminder, RemindAt: &at, Source: note.NoteSourceAI}
	if err := e.store.SaveNote(ctx, n); err != nil {
		return nil, err
	}
	e.createdNoteIDs = append(e.createdNoteIDs, n.ID)
	return map[string]any{"note_id": n.ID, "text": text, "remind_at": at.In(bangkok).Format(time.RFC3339), "when": thaiDateTime(at)}, nil
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
	Items    []string `json:"items,omitempty"`
}

func (e *executor) viewNotes(ctx context.Context, notes []*note.Note) ([]noteView, error) {
	views := make([]noteView, 0, len(notes))
	for _, n := range notes {
		v := noteView{Type: string(n.Type), Created: n.CreatedAt.In(bangkok).Format("2006-01-02 15:04")}
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
	return map[string]any{"from": fromStr, "to": toStr, "count": len(views), "items": views}, nil
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
