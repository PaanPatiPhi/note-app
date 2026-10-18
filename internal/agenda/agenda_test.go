package agenda

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"jodjod/internal/integrations"
	"jodjod/internal/note"
)

type fakeNotes struct {
	notes    []*note.Note
	from, to time.Time
}

func (f *fakeNotes) FindAgenda(_ context.Context, _ string, from, to time.Time) ([]*note.Note, error) {
	f.from, f.to = from, to
	return f.notes, nil
}

type fakeCal struct {
	connected bool
	events    []integrations.Event
	err       error
}

func (f *fakeCal) Connected(context.Context, string) (bool, error) { return f.connected, nil }
func (f *fakeCal) ListEvents(context.Context, string, time.Time, time.Time) ([]integrations.Event, error) {
	return f.events, f.err
}

func str(s string) *string { return &s }

var now = time.Date(2026, 10, 18, 15, 30, 0, 0, bangkok)

func TestBuildMergesNotesAndCalendar(t *testing.T) {
	remind := time.Date(2026, 10, 19, 9, 0, 0, 0, bangkok)
	due := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	notes := &fakeNotes{notes: []*note.Note{
		{ID: "n1", Content: str("โทรหาหมอ"), Type: note.NoteTypeReminder, RemindAt: &remind},
		{ID: "n2", Content: str("ส่งรายงาน"), Type: note.NoteTypeNote, DueDate: &due},
	}}
	meeting := time.Date(2026, 10, 18, 17, 0, 0, 0, bangkok)
	cal := &fakeCal{connected: true, events: []integrations.Event{
		{ID: "e1", Title: "ประชุมทีม", Start: meeting, End: meeting.Add(time.Hour), Link: "https://calendar.google.com/x"},
		{ID: "e2", Title: "⏰ โทรหาหมอ", Start: remind, End: remind.Add(15 * time.Minute)},
		{ID: "e3", Title: "วันหยุด", Start: time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC), AllDay: true},
	}}
	h := NewHandler(notes, cal)
	h.now = func() time.Time { return now }

	res, err := h.Build(context.Background(), "u1", 14)
	if err != nil {
		t.Fatal(err)
	}
	if !notes.from.Equal(time.Date(2026, 10, 18, 0, 0, 0, 0, bangkok)) || !notes.to.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, bangkok)) {
		t.Errorf("range = %s .. %s, want from the start of today for 14 days", notes.from, notes.to)
	}
	var got []string
	for _, it := range res.Items {
		got = append(got, it.Kind+":"+it.ID)
	}
	want := "event:e1,reminder:n1,due:n2,event:e3"
	if s := join(got); s != want {
		t.Errorf("items = %s, want %s (sorted, reminder shadow e2 skipped)", s, want)
	}
	if !res.CalendarConnected || res.CalendarError {
		t.Errorf("calendar flags = %+v", res)
	}
	if res.Items[3].Start.Day() != 23 || !res.Items[3].AllDay || res.Items[0].End == nil {
		t.Errorf("event details = %+v", res.Items)
	}
}

func TestBuildWithoutCalendar(t *testing.T) {
	for _, cal := range []Calendar{nil, &fakeCal{connected: false}, &fakeCal{connected: true, err: errors.New("google down")}} {
		h := NewHandler(&fakeNotes{}, cal)
		h.now = func() time.Time { return now }
		res, err := h.Build(context.Background(), "u1", 7)
		if err != nil {
			t.Fatalf("calendar problems must not fail the agenda: %v", err)
		}
		if res.Items == nil {
			t.Error("items should be an empty list, not null")
		}
		if fc, ok := cal.(*fakeCal); ok && fc.err != nil && !res.CalendarError {
			t.Error("a calendar read error should be reported")
		}
	}
}

func TestAgendaRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewHandler(&fakeNotes{}, nil)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if c.GetHeader("X-User") != "" {
			c.Set("user_id", c.GetHeader("X-User"))
		}
	})
	h.RegisterRoutes(r.Group("/api"))

	do := func(path, user string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if user != "" {
			req.Header.Set("X-User", user)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	w := do("/api/agenda?days=3", "u1")
	var body Response
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Items == nil {
		t.Errorf("agenda = %d %s", w.Code, w.Body.String())
	}
	for _, bad := range []string{"0", "61", "abc"} {
		if w := do("/api/agenda?days="+bad, "u1"); w.Code != http.StatusBadRequest {
			t.Errorf("days=%s = %d, want 400", bad, w.Code)
		}
	}
	if w := do("/api/agenda", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no user = %d, want 401", w.Code)
	}
}

func join(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}
