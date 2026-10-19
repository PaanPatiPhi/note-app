package auth

import (
	"testing"
	"time"
)

func TestHandoffCodeIsSingleUse(t *testing.T) {
	s := newHandoffStore()
	code, err := s.issue("user-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(code) < 40 {
		t.Errorf("code %q is too short to be unguessable", code)
	}
	if uid, ok := s.redeem(code); !ok || uid != "user-1" {
		t.Fatalf("first redeem = %q, %v; want user-1", uid, ok)
	}
	if _, ok := s.redeem(code); ok {
		t.Error("second redeem succeeded; codes must be single use")
	}
	if _, ok := s.redeem("made-up"); ok {
		t.Error("unknown code redeemed")
	}
}

func TestHandoffCodeExpires(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	s := newHandoffStore()
	s.now = func() time.Time { return now }
	code, _ := s.issue("user-1")

	now = now.Add(handoffTTL + time.Second)
	if _, ok := s.redeem(code); ok {
		t.Error("expired code redeemed")
	}

	// รหัสที่หมดอายุถูกล้างตอนออกรหัสใหม่
	stale, _ := s.issue("user-2")
	now = now.Add(handoffTTL + time.Second)
	_, _ = s.issue("user-3")
	if _, exists := s.codes[stale]; exists {
		t.Error("expired code was not pruned")
	}
}
