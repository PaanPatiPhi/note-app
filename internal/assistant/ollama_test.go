package assistant

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeOllama ตอบตามลำดับ และเก็บ request ที่ได้รับไว้ตรวจ
func fakeOllama(t *testing.T, responses ...string) (*httptest.Server, *[]ollamaChatRequest) {
	t.Helper()
	var reqs []ollamaChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req ollamaChatRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		reqs = append(reqs, req)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, responses[min(len(reqs), len(responses))-1])
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

func TestOllamaToolLoop(t *testing.T) {
	srv, reqs := fakeOllama(t,
		// arguments เป็น JSON string ห่อ object (บางโมเดลส่งแบบนี้) ต้องรับได้
		`{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"create_note","arguments":"{\"content\":\"อย่าลืมซื้อของ\"}"}}]},"done_reason":"stop","prompt_eval_count":900,"eval_count":30}`,
		`{"message":{"role":"assistant","content":"<think>\n</think>\n✅ จดแล้วครับ"},"done_reason":"stop","prompt_eval_count":950,"eval_count":10}`,
	)
	store := newFakeStore()
	actions := &fakeActions{}
	a := New(NewOllamaProvider(srv.URL, ""), store, actions)
	a.now = func() time.Time { return testNow }

	res, err := a.Handle(context.Background(), Request{UserID: "user-1", Text: "จดว่าอย่าลืมซื้อของ"})
	if err != nil {
		t.Fatal(err)
	}

	if res.Reply != "✅ จดแล้วครับ" || !res.Wrote {
		t.Errorf("result = %+v (think tags should be stripped)", res)
	}
	if len(store.notes) != 1 || *store.notes[0].Content != "อย่าลืมซื้อของ" {
		t.Fatalf("notes = %v", store.notes)
	}
	act := actions.saved[0]
	if act.Model != "ollama/"+DefaultOllamaModel || act.InputTokens != 1850 || act.OutputTokens != 40 {
		t.Errorf("action = model %q in=%d out=%d", act.Model, act.InputTokens, act.OutputTokens)
	}

	first := (*reqs)[0]
	if first.Model != DefaultOllamaModel || first.Stream || first.Think || len(first.Tools) != 8 {
		t.Errorf("first request = model %q stream %v think %v tools %d", first.Model, first.Stream, first.Think, len(first.Tools))
	}
	if first.Options["num_ctx"] != float64(ollamaNumCtx) {
		t.Errorf("num_ctx = %v, want %d", first.Options["num_ctx"], ollamaNumCtx)
	}
	if first.Messages[0].Role != "system" || !strings.Contains(first.Messages[1].Content, "<message>") {
		t.Error("first request should send the system prompt and the user message with context")
	}

	second := (*reqs)[1]
	last := second.Messages[len(second.Messages)-1]
	prev := second.Messages[len(second.Messages)-2]
	if prev.Role != "assistant" || len(prev.ToolCalls) != 1 {
		t.Errorf("second request should replay the assistant tool call, got %+v", prev)
	}
	if last.Role != "tool" || last.ToolName != "create_note" || !strings.Contains(last.Content, "note_id") {
		t.Errorf("second request should end with the tool result, got %+v", last)
	}
}

func TestOllamaToolErrorIsMarked(t *testing.T) {
	srv, reqs := fakeOllama(t,
		`{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"create_reminder","arguments":{"text":"x","remind_at":"พรุ่งนี้"}}}]},"done_reason":"stop"}`,
		`{"message":{"role":"assistant","content":"ขอเวลาที่ชัดเจนหน่อยครับ"},"done_reason":"stop"}`,
	)
	a := New(NewOllamaProvider(srv.URL, ""), newFakeStore(), &fakeActions{})
	a.now = func() time.Time { return testNow }

	if _, err := a.Handle(context.Background(), Request{UserID: "user-1", Text: "เตือนพรุ่งนี้"}); err != nil {
		t.Fatal(err)
	}
	msgs := (*reqs)[1].Messages
	if last := msgs[len(msgs)-1]; !strings.HasPrefix(last.Content, "ERROR: remind_at must be RFC3339") {
		t.Errorf("tool error should be sent back prefixed with ERROR:, got %q", last.Content)
	}
}

func TestOllamaHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":"model \"qwen3:4b\" not found, try pulling it first"}`)
	}))
	defer srv.Close()
	a := New(NewOllamaProvider(srv.URL, ""), newFakeStore(), &fakeActions{})

	_, err := a.Handle(context.Background(), Request{UserID: "user-1", Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want the ollama error message", err)
	}
}

func TestNormalizeArgsAndStripThink(t *testing.T) {
	for in, want := range map[string]string{
		`{"a":1}`:         `{"a":1}`,
		`"{\"a\":1}"`:     `{"a":1}`,
		`null`:            `{}`,
		``:                `{}`,
		"  {\"a\":1}\n  ": `{"a":1}`,
	} {
		if got := string(normalizeArgs(json.RawMessage(in))); got != want {
			t.Errorf("normalizeArgs(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"<think>\nhmm\n</think>\n\nสวัสดี":   "สวัสดี",
		"สวัสดี":                             "สวัสดี",
		"ก<think>x</think>ข<think>y</think>": "กข",
		"<think>unterminated":                "",
		"reasoning only\n</think>\n\nคำตอบ":  "คำตอบ",
	} {
		if got := stripThink(in); got != want {
			t.Errorf("stripThink(%q) = %q, want %q", in, got, want)
		}
	}
}
