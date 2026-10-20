package note

import (
	"strings"
	"testing"
	"time"
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
		if got := IsPinnedQuestion(text); got != want {
			t.Errorf("IsPinnedQuestion(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestParsePinCommand(t *testing.T) {
	cases := []struct {
		text   string
		query  string
		pinned bool
		ok     bool
	}{
		{"ปักลิสต์ซื้อของไว้", "ซื้อของ", true, true},
		{"ปัก ซื้อของ ไว้หน้าจอหน่อย", "ซื้อของ", true, true},
		{"ปักหมุดเลขห้องประชุม 504", "เลขห้องประชุม 504", true, true},
		{"ปักเตือนจ่ายค่าเน็ตไว้บน widget ด้วย", "จ่ายค่าเน็ต", true, true},
		{"เลิกปักลิสต์ซื้อของ", "ซื้อของ", false, true},
		{"ถอดหมุด เลขห้อง ออกจาก widget", "เลขห้อง", false, true},
		{"ปัก", "", true, false},
		{"ปักไว้", "", true, false},
		{"ซื้อนม", "", false, false},
	}
	for _, tc := range cases {
		q, pinned, ok := parsePinCommand(tc.text)
		if q != tc.query || ok != tc.ok || (ok && pinned != tc.pinned) {
			t.Errorf("parsePinCommand(%q) = %q, %v, %v; want %q, %v, %v", tc.text, q, pinned, ok, tc.query, tc.pinned, tc.ok)
		}
	}
}

func TestPickPinTarget(t *testing.T) {
	at := time.Now()
	n := func(id, content string, pinned, done bool) *Note {
		x := &Note{ID: id, Content: &content, IsDone: done}
		if pinned {
			x.PinnedAt = &at
		}
		return x
	}
	cases := []struct {
		name    string
		matches []*Note
		query   string
		pinned  bool
		wantID  string
	}{
		{"none", nil, "x", true, ""},
		{"single partial match", []*Note{n("a", "ซื้อของเข้าบ้าน", false, false)}, "ซื้อของ", true, "a"},
		{"exact title wins", []*Note{n("a", "ซื้อของขวัญ", false, false), n("b", "ซื้อของ", false, false)}, "ซื้อของ", true, "b"},
		{"two partial matches", []*Note{n("a", "ประชุมทีม", false, false), n("b", "ประชุมลูกค้า", false, false)}, "ประชุม", true, ""},
		{"unpin the only pinned one", []*Note{n("a", "ประชุมทีม", false, false), n("b", "ประชุมลูกค้า", true, false)}, "ประชุม", false, "b"},
		{"skip done when pinning", []*Note{n("a", "ซื้อของ 1", false, true), n("b", "ซื้อของ 2", false, false)}, "ซื้อของ", true, "b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := PickPinTarget(tc.matches, tc.query, tc.pinned)
			gotID := ""
			if ok {
				gotID = got.ID
			}
			if gotID != tc.wantID {
				t.Errorf("PickPinTarget = %q, want %q", gotID, tc.wantID)
			}
		})
	}
}

func TestFormatPinned(t *testing.T) {
	str := func(s string) *string { return &s }
	remind := time.Date(2026, 10, 16, 10, 30, 0, 0, bangkok)
	notes := []*Note{
		{ID: "list", Content: str("ซื้อของ"), Type: NoteTypeChecklist},
		{ID: "rem", Content: str("จ่ายค่าเน็ต"), Type: NoteTypeReminder, RemindAt: &remind},
		{ID: "done", Content: str("ของขวัญ"), Type: NoteTypeChecklist},
	}
	items := map[string][]*NoteItem{
		"list": {{Text: "ผ้าปู"}, {Text: "พัดลม", IsDone: true}},
		"done": {{Text: "การ์ด", IsDone: true}},
	}
	got := FormatPinned(notes, items)
	for _, want := range []string{"ปักไว้ 3 รายการ", "- ซื้อของ (เหลือ ผ้าปู)", "- ⏰ จ่ายค่าเน็ต (ศ. 16 ต.ค. 10:30)", "- ของขวัญ (ครบแล้ว)"} {
		if !strings.Contains(got, want) {
			t.Errorf("FormatPinned = %q, should contain %q", got, want)
		}
	}
	if empty := FormatPinned(nil, nil); !strings.Contains(empty, "ยังไม่ได้ปักอะไร") {
		t.Errorf("FormatPinned(nil) = %q", empty)
	}
}

func TestSplitChecklistDropsCommaAfterTitle(t *testing.T) {
	for _, text := range []string{"ซื้อของ, ผ้าปู, พัดลม", "ซื้อของ ผ้าปู, พัดลม", "ซื้อของ: ผ้าปู,พัดลม"} {
		title, items := splitChecklist(text)
		if title != "ซื้อของ" || len(items) != 2 || items[0] != "ผ้าปู" || items[1] != "พัดลม" {
			t.Errorf("splitChecklist(%q) = %q, %q", text, title, items)
		}
	}
}
