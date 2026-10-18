package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"jodjod/internal/note"
)

// ── fake Claude ─────────────────────────────────────────────────

type fakeLLM struct {
	responses []string // JSON ของ anthropic.Message ตามลำดับรอบ
	errAt     int      // รอบที่ (1-based) จะคืน error; 0 = ไม่มี
	requests  []string // JSON ของ params ที่ถูกส่งมาแต่ละรอบ
}

func (f *fakeLLM) New(_ context.Context, body anthropic.MessageNewParams, _ ...option.RequestOption) (*anthropic.Message, error) {
	data, _ := json.Marshal(body)
	f.requests = append(f.requests, string(data))
	n := len(f.requests)
	if n == f.errAt {
		return nil, errors.New("boom")
	}
	raw := f.responses[min(n, len(f.responses))-1]
	var msg anthropic.Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		return nil, fmt.Errorf("bad fake response: %w", err)
	}
	return &msg, nil
}

func toolUseMsg(id, name string, input any) string {
	in, _ := json.Marshal(input)
	return fmt.Sprintf(`{"id":"msg_%s","type":"message","role":"assistant","model":"test",
		"content":[{"type":"tool_use","id":%q,"name":%q,"input":%s}],
		"stop_reason":"tool_use","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":50}}`,
		id, id, name, in)
}

func textMsg(text string) string {
	t, _ := json.Marshal(text)
	return fmt.Sprintf(`{"id":"msg_final","type":"message","role":"assistant","model":"test",
		"content":[{"type":"text","text":%s}],
		"stop_reason":"end_turn","usage":{"input_tokens":120,"output_tokens":10}}`, t)
}

// ── fake store ──────────────────────────────────────────────────

type fakeStore struct {
	notes    []*note.Note
	items    map[string][]*note.NoteItem
	matches  map[string][]*note.ItemMatch
	setDone  map[string]bool
	appended []string
	nextID   int
}

func newFakeStore() *fakeStore {
	return &fakeStore{items: map[string][]*note.NoteItem{}, matches: map[string][]*note.ItemMatch{}, setDone: map[string]bool{}}
}

func (s *fakeStore) id(prefix string) string {
	s.nextID++
	return fmt.Sprintf("%s-%d", prefix, s.nextID)
}

func (s *fakeStore) SaveNote(_ context.Context, n *note.Note) error {
	n.ID = s.id("note")
	n.CreatedAt = time.Now()
	s.notes = append(s.notes, n)
	return nil
}

func (s *fakeStore) SaveChecklist(_ context.Context, userID string, content *string, items []string, source note.NoteSource) (*note.Note, error) {
	n := &note.Note{ID: s.id("note"), UserID: userID, Content: content, Type: note.NoteTypeChecklist, Source: source}
	s.notes = append(s.notes, n)
	for _, it := range items {
		s.items[n.ID] = append(s.items[n.ID], &note.NoteItem{ID: s.id("item"), NoteID: n.ID, Text: it})
	}
	return n, nil
}

func (s *fakeStore) FindOpenChecklistByTitle(_ context.Context, _ string, title string) (*note.Note, error) {
	for _, n := range s.notes {
		if n.Type == note.NoteTypeChecklist && n.Content != nil && strings.EqualFold(*n.Content, title) {
			return n, nil
		}
	}
	return nil, nil
}

func (s *fakeStore) ListOpenChecklistTitles(context.Context, string, int) ([]string, error) {
	var titles []string
	for _, n := range s.notes {
		if n.Type == note.NoteTypeChecklist && n.Content != nil {
			titles = append(titles, *n.Content)
		}
	}
	return titles, nil
}

func (s *fakeStore) AppendItems(_ context.Context, _ string, noteID string, items []string) ([]*note.NoteItem, error) {
	var added []*note.NoteItem
	for _, it := range items {
		ni := &note.NoteItem{ID: s.id("item"), NoteID: noteID, Text: it}
		s.items[noteID] = append(s.items[noteID], ni)
		added = append(added, ni)
		s.appended = append(s.appended, it)
	}
	return added, nil
}

func (s *fakeStore) FindItemsBySimilarText(_ context.Context, _ string, query string, _ int) ([]*note.ItemMatch, error) {
	return s.matches[query], nil
}

func (s *fakeStore) SetItemDone(_ context.Context, _ string, itemID string, done bool) error {
	s.setDone[itemID] = done
	return nil
}

func (s *fakeStore) SearchByUser(context.Context, string, string) ([]*note.Note, error) {
	return nil, nil
}

func (s *fakeStore) FindAgenda(context.Context, string, time.Time, time.Time) ([]*note.Note, error) {
	return nil, nil
}

func (s *fakeStore) FindItemsByNoteID(_ context.Context, noteID string) ([]*note.NoteItem, error) {
	return s.items[noteID], nil
}

// ── fake action log ─────────────────────────────────────────────

type fakeActions struct{ saved []*Action }

func (f *fakeActions) Insert(_ context.Context, a *Action) (string, error) {
	f.saved = append(f.saved, a)
	return fmt.Sprintf("action-%d", len(f.saved)), nil
}

// ── helpers ─────────────────────────────────────────────────────

var testNow = time.Date(2026, 10, 15, 14, 30, 0, 0, bangkok)

func newTestAssistant(llm *fakeLLM, store *fakeStore) (*Assistant, *fakeActions) {
	actions := &fakeActions{}
	a := New(NewClaudeProvider(llm, ""), store, actions)
	a.now = func() time.Time { return testNow }
	return a, actions
}

func handle(t *testing.T, a *Assistant, text string) *Result {
	t.Helper()
	res, err := a.Handle(context.Background(), Request{UserID: "user-1", WebhookEventID: "evt-1", Text: text})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	return res
}

// ── tests ───────────────────────────────────────────────────────

func TestAddToListAppendsToOpenList(t *testing.T) {
	store := newFakeStore()
	title := "ซื้อของ"
	store.SaveChecklist(context.Background(), "user-1", &title, []string{"ขนมปัง"}, note.NoteSourceLine)

	llm := &fakeLLM{responses: []string{
		toolUseMsg("t1", "add_to_list", map[string]any{"list_name": "ซื้อของ", "items": []string{"นม", "  ", "ไข่"}}),
		textMsg("📋 เพิ่ม นม, ไข่ ในลิสต์ ซื้อของ แล้วครับ"),
	}}
	a, actions := newTestAssistant(llm, store)

	res := handle(t, a, "ซื้อนม ไข่ด้วย")

	if got := strings.Join(store.appended, ","); got != "นม,ไข่" {
		t.Errorf("appended = %q, want นม,ไข่ (blank items dropped)", got)
	}
	if !res.Wrote || res.ActionID != "action-1" || !strings.Contains(res.Reply, "ซื้อของ") {
		t.Errorf("result = %+v", res)
	}
	if !strings.Contains(llm.requests[0], `\"ซื้อของ\"`) {
		t.Error("first request should list the open checklist in <context>")
	}
	if !strings.Contains(llm.requests[0], "2026-10-15T14:30:00+07:00") {
		t.Error("first request should include the current Bangkok time")
	}
	if !strings.Contains(llm.requests[1], `created_new_list`) {
		t.Error("second request should carry the tool_result")
	}

	if len(actions.saved) != 1 {
		t.Fatalf("saved actions = %d, want 1", len(actions.saved))
	}
	act := actions.saved[0]
	if len(act.AddedItemIDs) != 2 || len(act.CreatedNoteIDs) != 0 {
		t.Errorf("action ids: added=%v created=%v", act.AddedItemIDs, act.CreatedNoteIDs)
	}
	if act.InputTokens != 220 || act.CacheReadTokens != 50 || act.WebhookEventID != "evt-1" {
		t.Errorf("action usage/event = %+v", act)
	}
}

func TestReminderInPastIsToolError(t *testing.T) {
	store := newFakeStore()
	llm := &fakeLLM{responses: []string{
		toolUseMsg("t1", "create_reminder", map[string]any{"text": "โทรหาหมอ", "remind_at": "2026-10-15T09:00:00+07:00"}),
		textMsg("เวลา 9 โมงวันนี้ผ่านไปแล้ว หมายถึงพรุ่งนี้ไหมครับ"),
	}}
	a, actions := newTestAssistant(llm, store)

	res := handle(t, a, "เตือนโทรหาหมอ 9 โมงวันนี้")

	if len(store.notes) != 0 || res.Wrote {
		t.Errorf("no note should be saved for a past reminder (notes=%d wrote=%v)", len(store.notes), res.Wrote)
	}
	if !strings.Contains(llm.requests[1], `"is_error":true`) || !strings.Contains(llm.requests[1], "in the past") {
		t.Errorf("second request should carry an is_error tool_result, got %s", llm.requests[1])
	}
	if !actions.saved[0].ToolCalls[0].IsError {
		t.Error("action log should record the failed tool call")
	}
}

func TestCreateReminderSavesAINote(t *testing.T) {
	store := newFakeStore()
	llm := &fakeLLM{responses: []string{
		toolUseMsg("t1", "create_reminder", map[string]any{"text": "โทรหาหมอ", "remind_at": "2026-10-16T09:00:00+07:00"}),
		textMsg("⏰ ตั้งเตือนแล้ว: โทรหาหมอ — ศ. 16 ต.ค. 09:00"),
	}}
	a, _ := newTestAssistant(llm, store)

	handle(t, a, "พรุ่งนี้ 9 โมงเตือนโทรหาหมอ")

	if len(store.notes) != 1 {
		t.Fatalf("notes = %d, want 1", len(store.notes))
	}
	n := store.notes[0]
	want := time.Date(2026, 10, 16, 9, 0, 0, 0, bangkok)
	if n.Type != note.NoteTypeReminder || n.Source != note.NoteSourceAI || n.RemindAt == nil || !n.RemindAt.Equal(want) {
		t.Errorf("saved note = type:%s source:%s remind_at:%v", n.Type, n.Source, n.RemindAt)
	}
}

func TestRefusalReturnsPoliteReply(t *testing.T) {
	llm := &fakeLLM{responses: []string{`{"id":"m","type":"message","role":"assistant","model":"test",
		"content":[],"stop_reason":"refusal","usage":{"input_tokens":1,"output_tokens":1}}`}}
	a, _ := newTestAssistant(llm, newFakeStore())

	res := handle(t, a, "...")
	if res.Reply != refusalReply || res.Wrote {
		t.Errorf("result = %+v", res)
	}
}

func TestStopsAfterMaxRounds(t *testing.T) {
	llm := &fakeLLM{responses: []string{toolUseMsg("t1", "search_notes", map[string]any{"query": "x"})}}
	a, actions := newTestAssistant(llm, newFakeStore())

	_, err := a.Handle(context.Background(), Request{UserID: "user-1", Text: "หา x"})
	if err == nil {
		t.Fatal("expected an error when the model never stops calling tools")
	}
	if len(llm.requests) != maxRounds {
		t.Errorf("requests = %d, want %d", len(llm.requests), maxRounds)
	}
	if len(actions.saved) != 1 || actions.saved[0].Error == "" {
		t.Error("the failed run should still be logged with its error")
	}
}

func TestLLMErrorAfterWriteStillReturnsResult(t *testing.T) {
	store := newFakeStore()
	llm := &fakeLLM{
		responses: []string{toolUseMsg("t1", "create_note", map[string]any{"content": "อย่าลืมซื้อของ"})},
		errAt:     2,
	}
	a, actions := newTestAssistant(llm, store)

	res := handle(t, a, "จดว่าอย่าลืมซื้อของ")
	if !res.Wrote || res.Reply != fallbackReply || res.ActionID == "" {
		t.Errorf("result = %+v, want fallback reply with undo-able action", res)
	}
	if len(actions.saved[0].CreatedNoteIDs) != 1 {
		t.Error("created note id should be logged for undo")
	}
}

func TestUnknownToolIsError(t *testing.T) {
	llm := &fakeLLM{responses: []string{toolUseMsg("t1", "delete_everything", map[string]any{}), textMsg("ทำไม่ได้ครับ")}}
	a, _ := newTestAssistant(llm, newFakeStore())

	handle(t, a, "ลบทั้งหมด")
	if !strings.Contains(llm.requests[1], `unknown tool`) {
		t.Error("unknown tool should come back as an error tool_result")
	}
}

func TestCompleteItemsRecordsPreviousState(t *testing.T) {
	store := newFakeStore()
	store.matches["นม"] = []*note.ItemMatch{{Item: &note.NoteItem{ID: "item-milk", Text: "นม"}, Similarity: 1, NoteContent: "ซื้อของ"}}
	store.matches["ไข่"] = []*note.ItemMatch{
		{Item: &note.NoteItem{ID: "egg-1", Text: "ไข่ไก่"}, Similarity: 0.6},
		{Item: &note.NoteItem{ID: "egg-2", Text: "ไข่เป็ด"}, Similarity: 0.55},
	}
	llm := &fakeLLM{responses: []string{
		toolUseMsg("t1", "complete_items", map[string]any{"queries": []string{"นม", "ไข่", "กล้วย"}, "done": true}),
		textMsg("✅ ติ๊ก นม แล้ว ส่วนไข่ หมายถึงไข่ไก่หรือไข่เป็ดครับ"),
	}}
	a, actions := newTestAssistant(llm, store)

	handle(t, a, "ซื้อนม ไข่ กล้วยแล้ว")

	if !store.setDone["item-milk"] || len(store.setDone) != 1 {
		t.Errorf("setDone = %v, want only item-milk", store.setDone)
	}
	changes := actions.saved[0].ItemChanges
	if len(changes) != 1 || changes[0].PreviousDone || !changes[0].Done {
		t.Errorf("item changes = %+v", changes)
	}
	for _, want := range []string{`ambiguous`, `ไข่เป็ด`, `not_found`, `กล้วย`} {
		if !strings.Contains(llm.requests[1], want) {
			t.Errorf("tool_result should mention %q", want)
		}
	}
}

func TestPickConfident(t *testing.T) {
	m := func(id string, sim float64, done bool) *note.ItemMatch {
		return &note.ItemMatch{Item: &note.NoteItem{ID: id, IsDone: done}, Similarity: sim}
	}
	cases := []struct {
		name    string
		matches []*note.ItemMatch
		done    bool
		wantID  string
	}{
		{"none", nil, true, ""},
		{"single", []*note.ItemMatch{m("a", 0.4, false)}, true, "a"},
		{"clear winner", []*note.ItemMatch{m("a", 0.9, false), m("b", 0.5, false)}, true, "a"},
		{"close tie", []*note.ItemMatch{m("a", 0.6, false), m("b", 0.55, false)}, true, ""},
		{"tie but only one still pending", []*note.ItemMatch{m("a", 0.6, true), m("b", 0.55, false)}, true, "b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := pickConfident(tc.matches, tc.done)
			gotID := ""
			if ok {
				gotID = got.Item.ID
			}
			if gotID != tc.wantID {
				t.Errorf("pickConfident = %q, want %q", gotID, tc.wantID)
			}
		})
	}
}

func TestClaudeRequestHasStrictTools(t *testing.T) {
	llm := &fakeLLM{responses: []string{textMsg("ครับ")}}
	a, _ := newTestAssistant(llm, newFakeStore())
	handle(t, a, "สวัสดี")

	var req struct {
		Tools []struct {
			Name        string         `json:"name"`
			Strict      bool           `json:"strict"`
			InputSchema map[string]any `json:"input_schema"`
		} `json:"tools"`
		Model        string         `json:"model"`
		OutputConfig map[string]any `json:"output_config"`
	}
	if err := json.Unmarshal([]byte(llm.requests[0]), &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 6 || req.Model != DefaultClaudeModel || req.OutputConfig["effort"] != "low" {
		t.Fatalf("request = %d tools, model %q, output_config %v", len(req.Tools), req.Model, req.OutputConfig)
	}
	for _, tool := range req.Tools {
		if !tool.Strict || tool.InputSchema["additionalProperties"] != false || tool.InputSchema["type"] != "object" {
			t.Errorf("tool %s is not a strict object schema: %+v", tool.Name, tool)
		}
	}
}

func TestNudgesWhenClaimingWithoutToolCall(t *testing.T) {
	store := newFakeStore()
	llm := &fakeLLM{responses: []string{
		textMsg("✅ ติ๊ก นม แล้วครับ"), // อ้างว่าทำแล้วแต่ไม่ได้เรียก tool
		toolUseMsg("t1", "create_note", map[string]any{"content": "ซื้อนมแล้ว"}),
		textMsg("✅ จดแล้วครับ"),
	}}
	a, _ := newTestAssistant(llm, store)

	res := handle(t, a, "ซื้อนมแล้ว")

	if len(llm.requests) != 3 || !strings.Contains(llm.requests[1], "ยังไม่ได้เรียก tool") {
		t.Fatalf("second request should carry the nudge (requests=%d)", len(llm.requests))
	}
	if !res.Wrote || len(store.notes) != 1 {
		t.Errorf("after the nudge the tool should run: wrote=%v notes=%d", res.Wrote, len(store.notes))
	}
}

func TestRepeatedClaimWithoutToolCallFails(t *testing.T) {
	llm := &fakeLLM{responses: []string{textMsg("⏰ ตั้งเตือนแล้วครับ")}}
	a, _ := newTestAssistant(llm, newFakeStore())

	_, err := a.Handle(context.Background(), Request{UserID: "user-1", Text: "เตือนหน่อย"})
	if err == nil || !strings.Contains(err.Error(), "without calling any tool") {
		t.Fatalf("err = %v, want refusal to pass on a false confirmation", err)
	}
	if len(llm.requests) != maxNudges+1 {
		t.Errorf("requests = %d, want %d nudges then give up", len(llm.requests), maxNudges)
	}
}

func TestQuestionWithoutToolIsNotNudged(t *testing.T) {
	llm := &fakeLLM{responses: []string{textMsg("หมายถึงวันไหนครับ")}}
	a, _ := newTestAssistant(llm, newFakeStore())

	if res := handle(t, a, "เตือนหน่อย"); res.Reply != "หมายถึงวันไหนครับ" || len(llm.requests) != 1 {
		t.Errorf("a clarifying question should pass through unchanged: %+v (requests=%d)", res, len(llm.requests))
	}
}

func TestThaiDateTime(t *testing.T) {
	got := thaiDateTime(time.Date(2026, 10, 18, 9, 0, 0, 0, bangkok))
	if got != "อา. 18 ต.ค. 09:00" {
		t.Errorf("thaiDateTime = %q", got)
	}
}

func TestNudgesPlainReplyWithoutTool(t *testing.T) {
	store := newFakeStore()
	llm := &fakeLLM{responses: []string{
		textMsg("ครับ"), // ไม่เรียก tool, ไม่ได้ถาม, ไม่ได้อ้างว่าทำแล้ว → ต้องเตือน
		textMsg("🔍 กำลังค้นหาให้ครับ"),
		toolUseMsg("t1", "create_note", map[string]any{"content": "ประชุม 3, 4, 5 คนมา"}),
		textMsg("✅ จดแล้วครับ"),
	}}
	a, _ := newTestAssistant(llm, store)

	res := handle(t, a, "ประชุม 3, 4, 5 คนมา")

	if len(llm.requests) != 4 || !res.Wrote || len(store.notes) != 1 {
		t.Fatalf("requests=%d wrote=%v notes=%d, want two nudges then a saved note", len(llm.requests), res.Wrote, len(store.notes))
	}
	for _, i := range []int{1, 2} {
		if !strings.Contains(llm.requests[i], "create_note เก็บข้อความ") {
			t.Errorf("request %d should carry the nudge", i+1)
		}
	}
}

func TestAcceptsNonClaimReplyAfterMaxNudges(t *testing.T) {
	llm := &fakeLLM{responses: []string{textMsg("ยินดีครับ")}}
	a, _ := newTestAssistant(llm, newFakeStore())

	res := handle(t, a, "ขอบคุณ")
	if res.Reply != "ยินดีครับ" || len(llm.requests) != maxNudges+1 {
		t.Errorf("reply=%q requests=%d, want the reply after %d nudges", res.Reply, len(llm.requests), maxNudges)
	}
}

func TestNoNudgeAfterToolWasCalled(t *testing.T) {
	llm := &fakeLLM{responses: []string{
		toolUseMsg("t1", "search_notes", map[string]any{"query": "หมอ"}),
		textMsg("ไม่เจอโน้ตเรื่องหมอครับ"),
	}}
	a, _ := newTestAssistant(llm, newFakeStore())

	handle(t, a, "เคยจดเรื่องหมอไว้ไหม")
	if len(llm.requests) != 2 {
		t.Errorf("requests = %d, a final reply after a tool call must not be nudged", len(llm.requests))
	}
}

func TestIsQuestion(t *testing.T) {
	for reply, want := range map[string]bool{
		"หมายถึงวันไหนครับ":        true,
		"ไข่ไก่หรือไข่เป็ดครับ?":   true,
		"ให้ตั้งเตือนกี่โมงดีครับ": true,
		"จะให้จดไว้ไหมครับ":        true,
		"ครับ": false,
		"ไม่มีนัดอะไรในวันอาทิตย์นี้ครับ": false,
		"🔍 กำลังค้นหาข้อมูลนัดให้ครับ":    false,
	} {
		if got := isQuestion(reply); got != want {
			t.Errorf("isQuestion(%q) = %v, want %v", reply, got, want)
		}
	}
}

func TestReminderWithoutTimeBecomesNote(t *testing.T) {
	store := newFakeStore()
	llm := &fakeLLM{responses: []string{
		// โมเดลแต่งเวลาเอง ทั้งที่ผู้ใช้ไม่ได้บอก
		toolUseMsg("t1", "create_reminder", map[string]any{"text": "ซื้อพัดลมใหม่", "remind_at": "2026-10-15T18:00:00+07:00"}),
		toolUseMsg("t2", "create_note", map[string]any{"content": "ซื้อพัดลมใหม่"}),
		textMsg("📌 จดไว้แล้วครับ ถ้าอยากให้เตือน พิมพ์บอกเวลามาได้เลย"),
	}}
	a, _ := newTestAssistant(llm, store)

	res := handle(t, a, "เตือนให้ซื้อพัดลมใหม่หน่อย")

	if !strings.Contains(llm.requests[1], "did not say any date or time") {
		t.Errorf("the invented reminder should be rejected with guidance, got %s", llm.requests[1])
	}
	if len(store.notes) != 1 || store.notes[0].Type != note.NoteTypeNote || store.notes[0].RemindAt != nil {
		t.Fatalf("want exactly one plain note and no reminder, got %+v", store.notes)
	}
	if !res.Wrote {
		t.Error("result should report the note write")
	}
	if !strings.HasSuffix(res.Reply, reminderHint) {
		t.Errorf("reply should end with the reminder hint, got %q", res.Reply)
	}
}

func TestMentionsTime(t *testing.T) {
	for text, want := range map[string]bool{
		"เตือนให้ซื้อพัดลมใหม่หน่อย": false,
		"อย่าลืมโทรหาแม่":            false,
		"ตีแบดกับเพื่อน":             false,
		"พรุ่งนี้เตือนซื้อพัดลม":     true,
		"เตือนจ่ายค่าไฟทุ่มนึง":      true,
		"บ่ายสองไปรับลูก":            true,
		"เตือนวันศุกร์ส่งงาน":        true,
		"ประชุม 14:30":               true,
		"นัดหมอ ๑๕ ต.ค.":             true,
		"อีก 30 นาทีเตือนปิดเตา":     true,
		"ตีห้าตื่นไปวิ่ง":            true,
		"remind me tomorrow": true,
		"เตือนให้กินยาก่อนนอนตอนเย็นๆ": true,
	} {
		if got := mentionsTime(text); got != want {
			t.Errorf("mentionsTime(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestReminderHintNotAddedWhenTimeGiven(t *testing.T) {
	llm := &fakeLLM{responses: []string{
		toolUseMsg("t1", "create_reminder", map[string]any{"text": "ซื้อพัดลม", "remind_at": "2026-10-16T09:00:00+07:00"}),
		textMsg("⏰ ตั้งเตือนแล้วครับ"),
	}}
	a, _ := newTestAssistant(llm, newFakeStore())
	if res := handle(t, a, "พรุ่งนี้ 9 โมงเตือนซื้อพัดลม"); strings.Contains(res.Reply, reminderHint) {
		t.Errorf("hint must not be added when a reminder was set: %q", res.Reply)
	}
}

func (f *fakeActions) Undo(context.Context, string, string) (*UndoResult, error) {
	return &UndoResult{}, nil
}

func TestHandleLineMarksUndoable(t *testing.T) {
	llm := &fakeLLM{responses: []string{
		toolUseMsg("t1", "create_note", map[string]any{"content": "x"}),
		textMsg("✅ จดแล้วครับ"),
	}}
	a, _ := newTestAssistant(llm, newFakeStore())
	reply, err := a.HandleLine(context.Background(), "user-1", "evt", "จดว่า x")
	if err != nil || !reply.Undoable || reply.ActionID != "action-1" || reply.Text != "✅ จดแล้วครับ" {
		t.Fatalf("HandleLine = %+v, %v", reply, err)
	}

	read := &fakeLLM{responses: []string{
		toolUseMsg("t1", "search_notes", map[string]any{"query": "x"}),
		textMsg("ไม่เจอครับ"),
	}}
	a2, _ := newTestAssistant(read, newFakeStore())
	if reply, _ := a2.HandleLine(context.Background(), "user-1", "evt", "หาเรื่อง x"); reply.Undoable {
		t.Error("read-only actions should not get an undo button")
	}
}

func TestUndoReplyText(t *testing.T) {
	for _, tc := range []struct {
		res  UndoResult
		want string
	}{
		{UndoResult{NotFound: true}, "ไม่พบรายการที่จะยกเลิกครับ"},
		{UndoResult{AlreadyUndone: true}, "รายการนี้ยกเลิกไปแล้วครับ"},
		{UndoResult{Expired: true}, "ยกเลิกได้ภายใน 24 ชั่วโมงครับ รายการนี้เก่าเกินไปแล้ว"},
		{UndoResult{DeletedNotes: 1, RemovedItems: 2}, "↩️ ยกเลิกแล้วครับ: ลบที่จดไว้ 1 รายการ, เอาออกจากลิสต์ 2 รายการ"},
		{UndoResult{}, "↩️ ยกเลิกแล้วครับ (ข้อมูลถูกลบหรือแก้ไปก่อนหน้านี้แล้ว)"},
	} {
		if got := undoReplyText(&tc.res); got != tc.want {
			t.Errorf("undoReplyText(%+v) = %q, want %q", tc.res, got, tc.want)
		}
	}
}
