package integrations

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	scopeCalendarEvents = "https://www.googleapis.com/auth/calendar.events"
	stateTTL            = 10 * time.Minute
	defaultCalendarBase = "https://www.googleapis.com/calendar/v3"
	defaultUserinfoURL  = "https://openidconnect.googleapis.com/v1/userinfo"
	defaultRevokeURL    = "https://oauth2.googleapis.com/revoke"
)

var ErrNotConnected = errors.New("google calendar is not connected")

// GoogleConfig มาจาก env: GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET, GOOGLE_REDIRECT_URL
type GoogleConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

// Event คือ event ในปฏิทินแบบกลาง (ไม่ผูกกับ Google)
type Event struct {
	ID          string
	Title       string
	Location    string
	Description string
	Start       time.Time
	End         time.Time
	AllDay      bool
	Link        string
	// PopupAtStart = ให้ Google เตือนตรงเวลาเริ่ม (ใช้กับ reminder แทน LINE push)
	PopupAtStart bool
}

// Google จัดการ OAuth และเรียก Google Calendar API แทน user
type Google struct {
	oauth  *oauth2.Config
	repo   *Repository
	sealer *Sealer
	now    func() time.Time

	// แยกไว้ให้ test ชี้ไป server ปลอมได้
	calendarBase string
	userinfoURL  string
	revokeURL    string
	httpClient   *http.Client
}

func NewGoogle(cfg GoogleConfig, repo *Repository, sealer *Sealer) *Google {
	return &Google{
		oauth: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint:     google.Endpoint,
			Scopes:       []string{"openid", "email", scopeCalendarEvents},
		},
		repo:         repo,
		sealer:       sealer,
		now:          time.Now,
		calendarBase: defaultCalendarBase,
		userinfoURL:  defaultUserinfoURL,
		revokeURL:    defaultRevokeURL,
		httpClient:   http.DefaultClient,
	}
}

// ── OAuth ───────────────────────────────────────────────────────

// AuthURL คืนลิงก์หน้าขอสิทธิ์ของ Google และ nonce ที่ต้องเก็บไว้ใน cookie ของ browser นี้
// state ผูก user + nonce ไว้และหมดอายุใน 10 นาที; ตอน callback nonce ต้องตรงกับ cookie
// กันคนส่งลิงก์ที่มี state ของบัญชีตัวเองให้เหยื่อกด แล้วได้ปฏิทินของเหยื่อมาอยู่ในบัญชีตัวเอง
func (g *Google) AuthURL(userID string) (authURL, nonce string, err error) {
	state, nonce, err := g.newState(userID)
	if err != nil {
		return "", "", err
	}
	// offline + consent: ให้ได้ refresh token ทุกครั้ง (Google ให้เฉพาะตอนขอสิทธิ์ครั้งแรกถ้าไม่บังคับ)
	return g.oauth.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent")), nonce, nil
}

// HandleCallback ตรวจ state, แลก code เป็น token แล้วบันทึก; คืน user ID ที่เชื่อมสำเร็จ
// browserNonce คือค่าใน cookie ของ browser ที่กดเชื่อม ต้องตรงกับ nonce ใน state
func (g *Google) HandleCallback(ctx context.Context, code, state, browserNonce string) (string, error) {
	userID, nonce, err := g.verifyState(state)
	if err != nil {
		return "", err
	}
	if browserNonce == "" || !hmac.Equal([]byte(nonce), []byte(browserNonce)) {
		return "", errors.New("this sign-in was not started from this browser")
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, g.httpClient)
	tok, err := g.oauth.Exchange(ctx, code)
	if err != nil {
		return "", fmt.Errorf("exchange code: %w", err)
	}
	if tok.RefreshToken == "" {
		return "", errors.New("google did not return a refresh token")
	}
	scopes, _ := tok.Extra("scope").(string)
	if !strings.Contains(scopes, scopeCalendarEvents) {
		return "", errors.New("calendar permission was not granted")
	}
	email, _ := g.fetchEmail(ctx, tok)

	return userID, g.repo.Save(ctx, &Connection{
		UserID:       userID,
		Provider:     ProviderGoogle,
		AccountEmail: email,
		Scopes:       scopes,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		TokenExpiry:  tok.Expiry,
	})
}

// Disconnect ยกเลิก token ที่ Google (ถ้าทำได้) แล้วลบออกจาก DB
func (g *Google) Disconnect(ctx context.Context, userID string) error {
	conn, err := g.repo.Get(ctx, userID, ProviderGoogle)
	if err != nil {
		return err
	}
	if conn != nil {
		form := url.Values{"token": {conn.RefreshToken}}
		if req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.revokeURL, strings.NewReader(form.Encode())); err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if resp, err := g.httpClient.Do(req); err == nil {
				resp.Body.Close()
			}
		}
	}
	return g.repo.Delete(ctx, userID, ProviderGoogle)
}

// Status คืนอีเมลบัญชีที่เชื่อม หรือ "" ถ้ายังไม่ได้เชื่อม
func (g *Google) Status(ctx context.Context, userID string) (connected bool, email string, err error) {
	conn, err := g.repo.Get(ctx, userID, ProviderGoogle)
	if err != nil || conn == nil {
		return false, "", err
	}
	return true, conn.AccountEmail, nil
}

func (g *Google) fetchEmail(ctx context.Context, tok *oauth2.Token) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.userinfoURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var info struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", err
	}
	return info.Email, nil
}

// state = base64url(userID|expiryUnix|nonce) "." base64url(hmac)
func (g *Google) newState(userID string) (state, nonce string, err error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	nonce = hex.EncodeToString(raw)
	payload := []byte(fmt.Sprintf("%s|%d|%s", userID, g.now().Add(stateTTL).Unix(), nonce))
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(g.sealer.sign(payload)), nonce, nil
}

func (g *Google) verifyState(state string) (userID, nonce string, err error) {
	p, s, ok := strings.Cut(state, ".")
	if !ok {
		return "", "", errors.New("invalid state")
	}
	payload, err1 := base64.RawURLEncoding.DecodeString(p)
	sig, err2 := base64.RawURLEncoding.DecodeString(s)
	if err1 != nil || err2 != nil || !hmac.Equal(sig, g.sealer.sign(payload)) {
		return "", "", errors.New("invalid state")
	}
	parts := strings.Split(string(payload), "|")
	if len(parts) != 3 || parts[0] == "" || parts[2] == "" {
		return "", "", errors.New("invalid state")
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || g.now().Unix() > exp {
		return "", "", errors.New("state expired, please try connecting again")
	}
	return parts[0], parts[2], nil
}

// ── Calendar API ────────────────────────────────────────────────

// persistingSource refresh token อัตโนมัติ และบันทึก access token ใหม่ลง DB
type persistingSource struct {
	ctx    context.Context
	g      *Google
	userID string
	base   oauth2.TokenSource
	last   string
}

func (s *persistingSource) Token() (*oauth2.Token, error) {
	tok, err := s.base.Token()
	if err != nil {
		return nil, err
	}
	if tok.AccessToken != s.last {
		s.last = tok.AccessToken
		_ = s.g.repo.UpdateAccessToken(s.ctx, s.userID, ProviderGoogle, tok.AccessToken, tok.Expiry)
	}
	return tok, nil
}

func (g *Google) client(ctx context.Context, userID string) (*http.Client, *Connection, error) {
	conn, err := g.repo.Get(ctx, userID, ProviderGoogle)
	if err != nil {
		return nil, nil, err
	}
	if conn == nil {
		return nil, nil, ErrNotConnected
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, g.httpClient)
	stored := &oauth2.Token{AccessToken: conn.AccessToken, RefreshToken: conn.RefreshToken, Expiry: conn.TokenExpiry, TokenType: "Bearer"}
	src := &persistingSource{ctx: ctx, g: g, userID: userID, base: g.oauth.TokenSource(ctx, stored), last: conn.AccessToken}
	return oauth2.NewClient(ctx, oauth2.ReuseTokenSource(stored, src)), conn, nil
}

// Connected บอกว่า user เชื่อม Google Calendar ไว้หรือไม่
func (g *Google) Connected(ctx context.Context, userID string) (bool, error) {
	conn, err := g.repo.Get(ctx, userID, ProviderGoogle)
	return conn != nil, err
}

type gEventTime struct {
	DateTime string `json:"dateTime,omitempty"`
	Date     string `json:"date,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
}

type gEvent struct {
	ID          string     `json:"id,omitempty"`
	Summary     string     `json:"summary"`
	Location    string     `json:"location,omitempty"`
	Description string     `json:"description,omitempty"`
	Start       gEventTime `json:"start"`
	End         gEventTime `json:"end"`
	HTMLLink    string     `json:"htmlLink,omitempty"`
	Reminders   *struct {
		UseDefault bool `json:"useDefault"`
		Overrides  []struct {
			Method  string `json:"method"`
			Minutes int    `json:"minutes"`
		} `json:"overrides,omitempty"`
	} `json:"reminders,omitempty"`
}

const calendarTZ = "Asia/Bangkok"

func toGEvent(e Event) gEvent {
	ge := gEvent{Summary: e.Title, Location: e.Location, Description: e.Description}
	if e.AllDay {
		ge.Start = gEventTime{Date: e.Start.Format("2006-01-02")}
		ge.End = gEventTime{Date: e.End.Format("2006-01-02")}
	} else {
		ge.Start = gEventTime{DateTime: e.Start.Format(time.RFC3339), TimeZone: calendarTZ}
		ge.End = gEventTime{DateTime: e.End.Format(time.RFC3339), TimeZone: calendarTZ}
	}
	if e.PopupAtStart {
		ge.Reminders = &struct {
			UseDefault bool `json:"useDefault"`
			Overrides  []struct {
				Method  string `json:"method"`
				Minutes int    `json:"minutes"`
			} `json:"overrides,omitempty"`
		}{UseDefault: false, Overrides: []struct {
			Method  string `json:"method"`
			Minutes int    `json:"minutes"`
		}{{Method: "popup", Minutes: 0}}}
	}
	return ge
}

func fromGEvent(ge gEvent) Event {
	e := Event{ID: ge.ID, Title: ge.Summary, Location: ge.Location, Description: ge.Description, Link: ge.HTMLLink}
	if ge.Start.Date != "" {
		e.AllDay = true
		e.Start, _ = time.Parse("2006-01-02", ge.Start.Date)
		e.End, _ = time.Parse("2006-01-02", ge.End.Date)
	} else {
		e.Start, _ = time.Parse(time.RFC3339, ge.Start.DateTime)
		e.End, _ = time.Parse(time.RFC3339, ge.End.DateTime)
	}
	return e
}

func (g *Google) do(ctx context.Context, userID, method, path string, query url.Values, body any, out any) (*Connection, error) {
	hc, conn, err := g.client(ctx, userID)
	if err != nil {
		return nil, err
	}
	u := g.calendarBase + "/calendars/" + url.PathEscape(conn.CalendarID) + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("google calendar: %w", err)
	}
	defer resp.Body.Close()
	if method == http.MethodDelete && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone) {
		return conn, nil // ถูกลบไปแล้ว ถือว่าสำเร็จ
	}
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return nil, fmt.Errorf("google calendar HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return nil, fmt.Errorf("decode google calendar response: %w", err)
		}
	}
	return conn, nil
}

// CreateEvent สร้าง event ในปฏิทินหลักของ user
func (g *Google) CreateEvent(ctx context.Context, userID string, e Event) (Event, error) {
	var created gEvent
	if _, err := g.do(ctx, userID, http.MethodPost, "/events", nil, toGEvent(e), &created); err != nil {
		return Event{}, err
	}
	return fromGEvent(created), nil
}

// ListEvents คืน event ในช่วง [from, to) เรียงตามเวลาเริ่ม (รวม event ที่ซ้ำแยกเป็นรายครั้ง)
func (g *Google) ListEvents(ctx context.Context, userID string, from, to time.Time) ([]Event, error) {
	q := url.Values{
		"timeMin":      {from.Format(time.RFC3339)},
		"timeMax":      {to.Format(time.RFC3339)},
		"singleEvents": {"true"},
		"orderBy":      {"startTime"},
		"maxResults":   {"50"},
	}
	var res struct {
		Items []gEvent `json:"items"`
	}
	if _, err := g.do(ctx, userID, http.MethodGet, "/events", q, nil, &res); err != nil {
		return nil, err
	}
	events := make([]Event, 0, len(res.Items))
	for _, it := range res.Items {
		events = append(events, fromGEvent(it))
	}
	return events, nil
}

// DeleteEvent ลบ event (ใช้ตอนกดยกเลิก); event ที่ถูกลบไปแล้วถือว่าสำเร็จ
func (g *Google) DeleteEvent(ctx context.Context, userID, eventID string) error {
	_, err := g.do(ctx, userID, http.MethodDelete, "/events/"+url.PathEscape(eventID), nil, nil, nil)
	return err
}

// MoveEvent เปลี่ยนเวลาของ event เดิม (PATCH เฉพาะ start/end ส่วนอื่นคงเดิม)
func (g *Google) MoveEvent(ctx context.Context, userID, eventID string, start, end time.Time) (Event, error) {
	patch := map[string]gEventTime{
		"start": {DateTime: start.Format(time.RFC3339), TimeZone: calendarTZ},
		"end":   {DateTime: end.Format(time.RFC3339), TimeZone: calendarTZ},
	}
	var updated gEvent
	if _, err := g.do(ctx, userID, http.MethodPatch, "/events/"+url.PathEscape(eventID), nil, patch, &updated); err != nil {
		return Event{}, err
	}
	return fromGEvent(updated), nil
}
