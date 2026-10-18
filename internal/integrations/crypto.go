// Package integrations เชื่อมบัญชีผู้ใช้กับบริการภายนอก (เริ่มที่ Google Calendar)
package integrations

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// Sealer เข้ารหัส token ก่อนเก็บลง DB (AES-256-GCM) และเซ็น state ของ OAuth (HMAC-SHA256)
// key มาจาก INTEGRATIONS_KEY: 32 ไบต์ แบบ base64 — สร้างด้วย `openssl rand -base64 32`
type Sealer struct {
	aead    cipher.AEAD
	signKey []byte
}

func NewSealer(base64Key string) (*Sealer, error) {
	key, err := base64.StdEncoding.DecodeString(base64Key)
	if err != nil {
		return nil, fmt.Errorf("INTEGRATIONS_KEY is not valid base64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("INTEGRATIONS_KEY must decode to 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// แยก key สำหรับเซ็น ไม่ใช้ key เดียวกับเข้ารหัสตรงๆ
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("jodjod-integrations-state"))
	return &Sealer{aead: aead, signKey: mac.Sum(nil)}, nil
}

func (s *Sealer) Seal(plain string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	out := s.aead.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(out), nil
}

func (s *Sealer) Open(sealed string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return "", err
	}
	n := s.aead.NonceSize()
	if len(raw) < n {
		return "", errors.New("sealed value too short")
	}
	plain, err := s.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", errors.New("cannot decrypt token (wrong INTEGRATIONS_KEY?)")
	}
	return string(plain), nil
}

func (s *Sealer) sign(data []byte) []byte {
	mac := hmac.New(sha256.New, s.signKey)
	mac.Write(data)
	return mac.Sum(nil)
}
