package auth

import (
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	ErrInvalidToken = errors.New("invalid or expired session token")
)

type Claims struct {
	UserID string `json:"user_id"`
	jwt.RegisteredClaims
}

// devJWTSecret ใช้เฉพาะตอน dev เท่านั้น ค่านี้อยู่ใน source code จึงไม่ใช่ความลับ
const devJWTSecret = "jodjod-dev-secret-key-change-in-production-2026"

func getJWTSecret() []byte {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		return []byte(devJWTSecret)
	}
	return []byte(secret)
}

// CheckJWTConfig เรียกตอน start server: production ต้องตั้ง JWT_SECRET เสมอ
// ไม่งั้นใครก็ปลอม session token ได้ด้วย devJWTSecret ที่อยู่ใน source code
func CheckJWTConfig() error {
	if os.Getenv("JWT_SECRET") != "" {
		return nil
	}
	if os.Getenv("ENV") == "production" {
		return errors.New("JWT_SECRET is required when ENV=production")
	}
	log.Println("คำเตือน: ไม่ได้ตั้ง JWT_SECRET ใช้ dev secret แทน (ห้ามใช้ใน production)")
	return nil
}

// GenerateSessionToken สร้าง JWT token อายุ 30 วัน
func GenerateSessionToken(userID string) (string, time.Time, error) {
	exp := time.Now().Add(30 * 24 * time.Hour)
	claims := &Claims{
		UserID: userID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(exp),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Subject:   userID,
			Issuer:    "jodjod-api",
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString(getJWTSecret())
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign jwt: %w", err)
	}
	return tokenString, exp, nil
}

// ValidateSessionToken ตรวจสอบความถูกต้องของ JWT token
func ValidateSessionToken(tokenString string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return getJWTSecret(), nil
	})

	if err != nil {
		return nil, ErrInvalidToken
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, ErrInvalidToken
	}

	return claims, nil
}
