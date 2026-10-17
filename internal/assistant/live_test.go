package assistant

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/joho/godotenv"

	"jodjod/internal/note"
)

// TestLiveSmoke เรียกโมเดลจริงตาม AI_PROVIDER/AI_MODEL แต่ใช้ fakeStore จึงไม่เขียน DB
//
//	LIVE_SMOKE=1 go test ./internal/assistant -run TestLiveSmoke -v                      (ค่าเริ่มต้น: ollama, local ฟรี)
//	LIVE_SMOKE=1 AI_PROVIDER=claude go test ./internal/assistant -run TestLiveSmoke -v   (ต้องมี ANTHROPIC_API_KEY)
func TestLiveSmoke(t *testing.T) {
	if os.Getenv("LIVE_SMOKE") == "" {
		t.Skip("set LIVE_SMOKE=1 to call a real model")
	}
	_ = godotenv.Load("../../.env")
	providerName := strings.ToLower(os.Getenv("AI_PROVIDER"))
	switch providerName {
	case "", "ollama":
		base := os.Getenv("OLLAMA_URL")
		if base == "" {
			base = DefaultOllamaURL
		}
		resp, err := http.Get(base + "/api/version")
		if err != nil {
			t.Skipf("ollama not reachable at %s: %v", base, err)
		}
		resp.Body.Close()
	default:
		if os.Getenv("ANTHROPIC_API_KEY") == "" {
			t.Skip("AI_PROVIDER=claude needs ANTHROPIC_API_KEY")
		}
	}
	provider, err := ProviderFromEnv(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("provider: %s", provider.Name())

	now := time.Now().In(bangkok)
	tomorrow := now.AddDate(0, 0, 1)
	at := func(d time.Time, hour int) time.Time {
		return time.Date(d.Year(), d.Month(), d.Day(), hour, 0, 0, 0, bangkok)
	}

	cases := []struct {
		text         string
		wantTool     string
		wantRemindAt time.Time // ตรวจเฉพาะ create_reminder
	}{
		{"จดว่าอย่าลืมซื้อของ", "create_note", time.Time{}},
		{"ซื้อนม ไข่ ขนมปัง", "add_to_list", time.Time{}},
		{"พรุ่งนี้ 9 โมงเตือนโทรหาหมอ", "create_reminder", at(tomorrow, 9)},
		{"พรุ่งนี้บ่ายสองเตือนไปรับลูก", "create_reminder", at(tomorrow, 14)},
		{"เตือนจ่ายค่าไฟพรุ่งนี้ทุ่มนึง", "create_reminder", at(tomorrow, 19)},
		{"ซื้อนมแล้ว", "complete_items", time.Time{}},
		{"อาทิตย์นี้มีนัดอะไรบ้าง", "list_agenda", time.Time{}},
		{"ประชุม 3, 4, 5 คนมา", "create_note", time.Time{}},
	}

	passed := 0
	for _, tc := range cases {
		ok := t.Run(tc.text, func(t *testing.T) {
			store := newFakeStore()
			title := "ซื้อของ"
			store.SaveChecklist(context.Background(), "user-1", &title, []string{"นม"}, note.NoteSourceLine)
			store.matches["นม"] = []*note.ItemMatch{{Item: &note.NoteItem{ID: "item-milk", Text: "นม"}, Similarity: 1, NoteContent: title}}
			store.notes = store.notes[:0:0] // ให้ลิสต์ยังอยู่ใน context แต่ไม่ปนกับโน้ตที่ test ตรวจ
			store.notes = append(store.notes, &note.Note{ID: "list", Type: note.NoteTypeChecklist, Content: &title})

			actions := &fakeActions{}
			a := New(provider, store, actions)

			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			start := time.Now()
			res, err := a.Handle(ctx, Request{UserID: "user-1", Text: tc.text})
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("Handle (%s): %v", elapsed.Round(time.Millisecond), err)
			}

			act := actions.saved[0]
			var tools []string
			for _, c := range act.ToolCalls {
				tools = append(tools, c.Name)
				in, _ := json.Marshal(c.Input)
				t.Logf("tool %s input=%s error=%q", c.Name, in, c.Error)
			}
			t.Logf("reply (%s, in=%d out=%d tokens): %s", elapsed.Round(time.Millisecond), act.InputTokens, act.OutputTokens, res.Reply)

			if len(tools) == 0 || tools[0] != tc.wantTool {
				t.Errorf("first tool = %v, want %s", tools, tc.wantTool)
			}
			if !tc.wantRemindAt.IsZero() {
				var got *time.Time
				for _, n := range store.notes {
					if n.Type == note.NoteTypeReminder {
						got = n.RemindAt
					}
				}
				if got == nil || !got.Equal(tc.wantRemindAt) {
					t.Errorf("remind_at = %v, want %v", got, tc.wantRemindAt.Format(time.RFC3339))
				}
			}
		})
		if ok {
			passed++
		}
	}
	t.Logf("%s: %d/%d cases passed", provider.Name(), passed, len(cases))
}
