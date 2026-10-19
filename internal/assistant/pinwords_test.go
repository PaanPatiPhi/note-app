package assistant

import (
	"strings"
	"testing"

	"jodjod/internal/note"
)

func TestIsPinnedQuestion(t *testing.T) {
	cases := map[string]bool{
		"ตอนนี้ปักอะไรไว้บ้าง":          true,
		"มีอะไรอยู่บน widget บ้าง":      true,
		"ปักอะไรไว้":                    true,
		"ปักลิสต์อะไรไว้บ้าง":           true,
		"บนวิดเจ็ตมีอะไร":               true,
		"ปักลิสต์ซื้อของไว้":            false,
		"เอาอะไรออกจาก widget บ้าง":     false,
		"เลิกปักอะไรได้บ้าง":            false,
		"ซื้ออะไรบ้าง":                  false,
		"จดลิสต์ซื้อของ แล้วปักไว้ด้วย": false,
	}
	for text, want := range cases {
		if got := isPinnedQuestion(text); got != want {
			t.Errorf("isPinnedQuestion(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestPinnedQuestionAnswersWithoutModel(t *testing.T) {
	store := newFakeStore()
	str := func(s string) *string { return &s }
	at := testNow
	remind := testNow.Add(20 * 60 * 60 * 1e9)
	store.notes = []*note.Note{
		{ID: "list", Content: str("ซื้อของ"), Type: note.NoteTypeChecklist, PinnedAt: &at},
		{ID: "rem", Content: str("จ่ายค่าเน็ต"), Type: note.NoteTypeReminder, RemindAt: &remind, PinnedAt: &at},
		{ID: "other", Content: str("ไม่ได้ปัก"), Type: note.NoteTypeNote},
	}
	store.items["list"] = []*note.NoteItem{{ID: "i1", Text: "ผ้าปู"}, {ID: "i2", Text: "พัดลม", IsDone: true}}
	llm := &fakeLLM{} // ถ้าเรียกโมเดลจะได้ error เพราะไม่มีคำตอบเตรียมไว้
	a, actions := newTestAssistant(llm, store)

	res := handle(t, a, "ตอนนี้ปักอะไรไว้บ้าง")

	if len(llm.requests) != 0 {
		t.Fatalf("model was called %d times, want 0", len(llm.requests))
	}
	for _, want := range []string{"ปักไว้ 2 รายการ", "ซื้อของ (เหลือ ผ้าปู)", "⏰ จ่ายค่าเน็ต (", "ศ. 16 ต.ค. 10:30"} {
		if !strings.Contains(res.Reply, want) {
			t.Errorf("reply %q should contain %q", res.Reply, want)
		}
	}
	if strings.Contains(res.Reply, "ไม่ได้ปัก") || res.Wrote {
		t.Errorf("reply = %q, wrote = %v", res.Reply, res.Wrote)
	}
	if calls := actions.saved[0].ToolCalls; len(calls) != 1 || calls[0].Name != "list_pinned" {
		t.Errorf("logged tool calls = %+v, want one list_pinned", calls)
	}
}

func TestPinnedReplyEmpty(t *testing.T) {
	if got := pinnedReply(nil); !strings.Contains(got, "ยังไม่ได้ปักอะไร") {
		t.Errorf("pinnedReply(nil) = %q", got)
	}
}
