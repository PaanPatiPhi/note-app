package assistant

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// newTestRouter ต่อ APIHandler หลัง middleware จำลองที่ใส่ user_id (แทน auth.RequireAuth)
func newTestRouter(h *APIHandler, userID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api", func(c *gin.Context) {
		if userID != "" {
			c.Set("user_id", userID)
		}
		c.Next()
	})
	h.RegisterRoutes(api)
	return r
}

func doJSON(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestAPIMessageWritesAndIsUndoable(t *testing.T) {
	store := newFakeStore()
	llm := &fakeLLM{responses: []string{
		toolUseMsg("t1", "add_to_list", map[string]any{"list_name": "ซื้อของ", "items": []string{"นม", "ไข่"}}),
		textMsg("📋 เพิ่ม นม, ไข่ ในลิสต์ ซื้อของ แล้วครับ"),
	}}
	a, actions := newTestAssistant(llm, store)
	r := newTestRouter(NewAPIHandler(a), "user-web")

	w := doJSON(r, http.MethodPost, "/api/assistant/messages", `{"text":"ซื้อนม ไข่"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	var res messageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.Undoable || !res.Wrote || res.ActionID != "action-1" || !strings.Contains(res.Reply, "ซื้อของ") {
		t.Errorf("response = %+v", res)
	}
	if got := actions.saved[0].UserID; got != "user-web" {
		t.Errorf("action user = %q, want the authenticated user", got)
	}
	if actions.saved[0].WebhookEventID != "" {
		t.Error("web requests have no LINE webhook event id")
	}
}

func TestAPIMessageValidation(t *testing.T) {
	a, _ := newTestAssistant(&fakeLLM{responses: []string{textMsg("ครับ")}}, newFakeStore())

	cases := []struct {
		name   string
		user   string
		body   string
		status int
	}{
		{"no auth", "", `{"text":"x"}`, http.StatusUnauthorized},
		{"bad json", "u", `{`, http.StatusBadRequest},
		{"empty", "u", `{"text":"   "}`, http.StatusBadRequest},
		{"too long", "u", `{"text":"` + strings.Repeat("ก", maxContentRunes+1) + `"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doJSON(newTestRouter(NewAPIHandler(a), tc.user), http.MethodPost, "/api/assistant/messages", tc.body)
			if w.Code != tc.status {
				t.Errorf("status = %d, want %d (body %s)", w.Code, tc.status, w.Body)
			}
		})
	}
}

func TestAPIMessageModelDown(t *testing.T) {
	llm := &fakeLLM{responses: []string{textMsg("x")}, errAt: 1}
	a, _ := newTestAssistant(llm, newFakeStore())

	w := doJSON(newTestRouter(NewAPIHandler(a), "u"), http.MethodPost, "/api/assistant/messages", `{"text":"จดว่า x"}`)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), fallbackReply) {
		t.Errorf("status = %d body = %s, want 503 with a friendly reply", w.Code, w.Body)
	}
}

func TestAPIDisabledAssistant(t *testing.T) {
	r := newTestRouter(NewAPIHandler(nil), "u")
	for _, path := range []string{"/api/assistant/messages", "/api/assistant/actions/x/undo"} {
		if w := doJSON(r, http.MethodPost, path, `{"text":"x"}`); w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s status = %d, want 503 when AI is off", path, w.Code)
		}
	}
}

type undoStub struct {
	fakeActions
	res       *UndoResult
	gotUser   string
	gotAction string
}

func (u *undoStub) Undo(_ context.Context, userID, actionID string) (*UndoResult, error) {
	u.gotUser, u.gotAction = userID, actionID
	return u.res, nil
}

func TestAPIUndo(t *testing.T) {
	cases := []struct {
		res    UndoResult
		status int
	}{
		{UndoResult{DeletedNotes: 1}, http.StatusOK},
		{UndoResult{NotFound: true}, http.StatusNotFound},
		{UndoResult{AlreadyUndone: true}, http.StatusConflict},
		{UndoResult{Expired: true}, http.StatusConflict},
	}
	for _, tc := range cases {
		stub := &undoStub{res: &tc.res}
		a := New(NewClaudeProvider(&fakeLLM{}, ""), newFakeStore(), stub)
		w := doJSON(newTestRouter(NewAPIHandler(a), "user-web"), http.MethodPost, "/api/assistant/actions/act-9/undo", "")
		if w.Code != tc.status {
			t.Errorf("%+v: status = %d, want %d", tc.res, w.Code, tc.status)
		}
		if stub.gotUser != "user-web" || stub.gotAction != "act-9" {
			t.Errorf("Undo called with user %q action %q", stub.gotUser, stub.gotAction)
		}
		if !strings.Contains(w.Body.String(), `"message"`) {
			t.Errorf("response should carry a message: %s", w.Body)
		}
	}
}

type historyStub struct {
	fakeActions
	items     []HistoryItem
	gotLimit  int
	gotBefore *time.Time
}

func (s *historyStub) History(_ context.Context, _ string, limit int, before *time.Time) ([]HistoryItem, error) {
	s.gotLimit, s.gotBefore = limit, before
	return s.items, nil
}

func TestAPIHistory(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	stub := &historyStub{items: []HistoryItem{
		{ID: "a", Text: "x", CreatedAt: t0},
		{ID: "b", Text: "y", CreatedAt: t0.Add(time.Minute)},
	}}
	a := New(NewClaudeProvider(&fakeLLM{}, ""), newFakeStore(), stub)
	r := newTestRouter(NewAPIHandler(a), "user-web")

	w := doJSON(r, http.MethodGet, "/api/assistant/history?limit=2", "")
	if w.Code != http.StatusOK || stub.gotLimit != 2 {
		t.Fatalf("status = %d limit = %d body = %s", w.Code, stub.gotLimit, w.Body)
	}
	var res struct {
		Items      []HistoryItem `json:"items"`
		NextBefore *string       `json:"next_before"`
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	if len(res.Items) != 2 || res.NextBefore == nil || *res.NextBefore != t0.Format(time.RFC3339Nano) {
		t.Errorf("response = %+v, next_before should be the oldest item when the page is full", res)
	}

	w = doJSON(r, http.MethodGet, "/api/assistant/history?limit=500&before="+t0.Format(time.RFC3339Nano), "")
	if w.Code != http.StatusOK || stub.gotLimit != maxHistoryLimit || stub.gotBefore == nil || !stub.gotBefore.Equal(t0) {
		t.Errorf("limit capped to %d? got %d; before = %v", maxHistoryLimit, stub.gotLimit, stub.gotBefore)
	}

	for _, q := range []string{"limit=0", "limit=x", "before=yesterday"} {
		if w := doJSON(r, http.MethodGet, "/api/assistant/history?"+q, ""); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, w.Code)
		}
	}
	if w := doJSON(newTestRouter(NewAPIHandler(a), ""), http.MethodGet, "/api/assistant/history", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no auth: status = %d, want 401", w.Code)
	}
}
