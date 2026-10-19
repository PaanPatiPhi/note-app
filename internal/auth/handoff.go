package auth

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// handoffTTL คืออายุของรหัสส่งต่อ: แค่พอให้เปิด browser ภายนอกจากหน้า LIFF
const handoffTTL = 2 * time.Minute

// handoffStore เก็บรหัสส่งต่อ session แบบใช้ครั้งเดียว
// ใช้ตอนหน้า LIFF (browser ในแอป LINE) ต้องพาผู้ใช้ไป browser ภายนอก เช่นเชื่อม Google
// ซึ่ง Google ไม่ให้ทำใน browser ฝังของแอป — browser ภายนอกไม่มี session เดิม จึงแลกรหัสนี้เป็น session ใหม่
// เก็บในหน่วยความจำพอ: รหัสอยู่ไม่กี่นาที server รีสตาร์ทก็แค่ให้กดใหม่
type handoffStore struct {
	mu    sync.Mutex
	codes map[string]handoffEntry
	now   func() time.Time
}

type handoffEntry struct {
	userID  string
	expires time.Time
}

func newHandoffStore() *handoffStore {
	return &handoffStore{codes: map[string]handoffEntry{}, now: time.Now}
}

// issue สร้างรหัสใหม่ให้ user (สุ่ม 256 bit เดาไม่ได้)
func (s *handoffStore) issue(userID string) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	code := base64.RawURLEncoding.EncodeToString(buf)

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for c, e := range s.codes {
		if now.After(e.expires) {
			delete(s.codes, c)
		}
	}
	s.codes[code] = handoffEntry{userID: userID, expires: now.Add(handoffTTL)}
	return code, nil
}

// redeem คืน user ของรหัส แล้วลบรหัสทิ้งเสมอ (ใช้ได้ครั้งเดียว แม้หมดอายุแล้วก็ลบ)
func (s *handoffStore) redeem(code string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.codes[code]
	if !ok {
		return "", false
	}
	delete(s.codes, code)
	if s.now().After(e.expires) {
		return "", false
	}
	return e.userID, true
}
