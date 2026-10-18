package integrations

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

var testKey = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

func testSealer(t *testing.T) *Sealer {
	t.Helper()
	s, err := NewSealer(testKey)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSealerRoundTripAndTamper(t *testing.T) {
	s := testSealer(t)
	sealed, err := s.Seal("ya29.secret-token")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, "secret-token") {
		t.Fatal("sealed value leaks the plaintext")
	}
	if got, err := s.Open(sealed); err != nil || got != "ya29.secret-token" {
		t.Fatalf("Open = %q, %v", got, err)
	}
	raw, _ := base64.StdEncoding.DecodeString(sealed)
	raw[len(raw)-1] ^= 1
	if _, err := s.Open(base64.StdEncoding.EncodeToString(raw)); err == nil {
		t.Error("tampered ciphertext should not decrypt")
	}
	if _, err := NewSealer("c2hvcnQ="); err == nil {
		t.Error("a key that is not 32 bytes must be rejected")
	}
}

func TestOAuthState(t *testing.T) {
	g := NewGoogle(GoogleConfig{ClientID: "cid", ClientSecret: "sec", RedirectURL: "http://localhost:8080/cb"}, nil, testSealer(t))
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return now }

	u, nonce, err := g.AuthURL("user-123")
	if err != nil || len(nonce) < 32 {
		t.Fatalf("AuthURL nonce=%q err=%v", nonce, err)
	}
	for _, want := range []string{"access_type=offline", "prompt=consent", "calendar.events", "client_id=cid"} {
		if !strings.Contains(u, want) {
			t.Errorf("auth URL missing %q: %s", want, u)
		}
	}

	state, stateNonce, _ := g.newState("user-123")
	if got, n, err := g.verifyState(state); err != nil || got != "user-123" || n != stateNonce {
		t.Fatalf("verifyState = %q %q, %v", got, n, err)
	}
	// callback จาก browser อื่น (ไม่มี cookie หรือ cookie ไม่ตรง) ต้องไม่ผ่าน ก่อนจะไปแลก code กับ Google
	for _, browser := range []string{"", "someone-else"} {
		if _, err := g.HandleCallback(context.Background(), "code", state, browser); err == nil || !strings.Contains(err.Error(), "browser") {
			t.Errorf("HandleCallback with browser nonce %q = %v, want a browser mismatch error", browser, err)
		}
	}
	// แก้ user ใน state โดยไม่มีลายเซ็นที่ถูกต้อง → ต้องไม่ผ่าน
	p, s, _ := strings.Cut(state, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(p)
	forged := base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(payload), "user-123", "victim", 1))) + "." + s
	if _, _, err := g.verifyState(forged); err == nil {
		t.Error("a forged state must be rejected")
	}
	g.now = func() time.Time { return now.Add(stateTTL + time.Second) }
	if _, _, err := g.verifyState(state); err == nil {
		t.Error("an expired state must be rejected")
	}
}

func TestEventJSON(t *testing.T) {
	bkk := time.FixedZone("ICT", 7*3600)
	start := time.Date(2026, 10, 19, 20, 0, 0, 0, bkk)
	ge := toGEvent(Event{Title: "จ่ายค่าเน็ต", Start: start, End: start.Add(15 * time.Minute), PopupAtStart: true})
	b, _ := json.Marshal(ge)
	for _, want := range []string{`"dateTime":"2026-10-19T20:00:00+07:00"`, `"timeZone":"Asia/Bangkok"`, `"useDefault":false`, `"minutes":0`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("event JSON missing %s: %s", want, b)
		}
	}
	plain, _ := json.Marshal(toGEvent(Event{Title: "x", Start: start, End: start.Add(time.Hour)}))
	if strings.Contains(string(plain), "reminders") {
		t.Errorf("events without PopupAtStart should keep the user's default reminders: %s", plain)
	}
}

// ── ทดสอบกับ DB จริง + Google Calendar ปลอม ────────────────────

func dbPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	_ = godotenv.Load("../../.env")
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("skipping db integration test: DATABASE_URL not set")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestGoogleCalendarAgainstFakeAPI(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	var hasTable bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.integrations') IS NOT NULL`).Scan(&hasTable); err != nil || !hasTable {
		t.Skip("skipping: migration 0003_integrations.sql not applied yet")
	}
	var userID string
	if err := pool.QueryRow(ctx, `INSERT INTO users (display_name) VALUES ('jodjod-test-integrations') RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("create test user: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID) })

	var gotAuth []string
	var created map[string]any
	var revoked string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/revoke" {
			r.ParseForm()
			revoked = r.PostForm.Get("token")
			return
		}
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/calendars/primary/events":
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &created)
			io.WriteString(w, `{"id":"evt-1","summary":"ประชุมทีม","htmlLink":"https://calendar.google.com/x",
				"start":{"dateTime":"2026-10-19T14:00:00+07:00"},"end":{"dateTime":"2026-10-19T15:00:00+07:00"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/calendars/primary/events":
			if r.URL.Query().Get("singleEvents") != "true" {
				t.Errorf("list should expand recurring events: %s", r.URL.RawQuery)
			}
			io.WriteString(w, `{"items":[{"id":"evt-1","summary":"ประชุมทีม","start":{"dateTime":"2026-10-19T14:00:00+07:00"},"end":{"dateTime":"2026-10-19T15:00:00+07:00"}},
				{"id":"evt-2","summary":"วันหยุด","start":{"date":"2026-10-23"},"end":{"date":"2026-10-24"}}]}`)
		case r.Method == http.MethodPatch && r.URL.Path == "/calendars/primary/events/evt-1":
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "summary") || !strings.Contains(string(body), `"dateTime":"2026-10-19T15:00:00+07:00"`) {
				t.Errorf("patch should only send the new start/end: %s", body)
			}
			io.WriteString(w, `{"id":"evt-1","summary":"ประชุมทีม","start":{"dateTime":"2026-10-19T15:00:00+07:00"},"end":{"dateTime":"2026-10-19T16:00:00+07:00"}}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/calendars/primary/events/evt-gone":
			w.WriteHeader(http.StatusGone)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer fake.Close()

	sealer := testSealer(t)
	repo := NewRepository(pool, sealer)
	g := NewGoogle(GoogleConfig{ClientID: "cid", ClientSecret: "sec"}, repo, sealer)
	g.calendarBase = fake.URL

	if ok, _ := g.Connected(ctx, userID); ok {
		t.Fatal("should not be connected yet")
	}
	if _, err := g.CreateEvent(ctx, userID, Event{Title: "x"}); err != ErrNotConnected {
		t.Fatalf("CreateEvent before connecting = %v, want ErrNotConnected", err)
	}

	err := repo.Save(ctx, &Connection{UserID: userID, Provider: ProviderGoogle, AccountEmail: "me@example.com",
		AccessToken: "access-abc", RefreshToken: "refresh-xyz", TokenExpiry: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	// token ใน DB ต้องเป็นข้อความที่เข้ารหัสแล้ว
	var storedAccess, storedRefresh string
	pool.QueryRow(ctx, `SELECT access_token, refresh_token FROM integrations WHERE user_id = $1`, userID).Scan(&storedAccess, &storedRefresh)
	if strings.Contains(storedAccess, "access-abc") || strings.Contains(storedRefresh, "refresh-xyz") {
		t.Fatal("tokens are stored in plaintext")
	}

	bkk := time.FixedZone("ICT", 7*3600)
	start := time.Date(2026, 10, 19, 14, 0, 0, 0, bkk)
	ev, err := g.CreateEvent(ctx, userID, Event{Title: "ประชุมทีม", Start: start, End: start.Add(time.Hour)})
	if err != nil || ev.ID != "evt-1" {
		t.Fatalf("CreateEvent = %+v, %v", ev, err)
	}
	if created["summary"] != "ประชุมทีม" {
		t.Errorf("request body = %v", created)
	}
	events, err := g.ListEvents(ctx, userID, start.Add(-24*time.Hour), start.Add(7*24*time.Hour))
	if err != nil || len(events) != 2 || !events[1].AllDay || !events[0].Start.Equal(start) {
		t.Fatalf("ListEvents = %+v, %v", events, err)
	}
	if moved, err := g.MoveEvent(ctx, userID, "evt-1", start.Add(time.Hour), start.Add(2*time.Hour)); err != nil || moved.Title != "ประชุมทีม" || !moved.Start.Equal(start.Add(time.Hour)) {
		t.Errorf("MoveEvent = %+v, %v", moved, err)
	}
	if err := g.DeleteEvent(ctx, userID, "evt-gone"); err != nil {
		t.Errorf("deleting an already-deleted event should succeed: %v", err)
	}
	for _, a := range gotAuth {
		if a != "Bearer access-abc" {
			t.Errorf("Authorization = %q, want the decrypted access token", a)
		}
	}

	if connected, email, _ := g.Status(ctx, userID); !connected || email != "me@example.com" {
		t.Errorf("Status = %v %q", connected, email)
	}
	g.revokeURL = fake.URL + "/revoke"
	if err := g.Disconnect(ctx, userID); err != nil {
		t.Fatal(err)
	}
	if revoked != "refresh-xyz" {
		t.Errorf("revoked token = %q, want the decrypted refresh token", revoked)
	}
	if ok, _ := g.Connected(ctx, userID); ok {
		t.Error("should be disconnected")
	}
}
