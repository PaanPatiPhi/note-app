package auth

import (
	"errors"
	"fmt"
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

// minJWTSecretLen: secret สั้นเดาได้ด้วยการลองรัวๆ แบบ offline จาก token ที่มีอยู่ในมือ
const minJWTSecretLen = 32

var errNoJWTSecret = errors.New("JWT_SECRET is not set")

// getJWTSecret ไม่มีค่า fallback: ถ้าไม่ได้ตั้ง JWT_SECRET จะออก/ตรวจ token ไม่ได้เลย
// (เดิมมี dev secret อยู่ใน source code ใครอ่านโค้ดก็ปลอม token เป็นใครก็ได้)
func getJWTSecret() ([]byte, error) {
	secret := os.Getenv("JWT_SECRET")
	if len(secret) < minJWTSecretLen {
		return nil, errNoJWTSecret
	}
	return []byte(secret), nil
}

// CheckJWTConfig เรียกตอน start server: ต้องตั้ง JWT_SECRET ยาวอย่างน้อย 32 ตัวอักษรเสมอ
// สร้างด้วย `openssl rand -base64 48`
func CheckJWTConfig() error {
	if len(os.Getenv("JWT_SECRET")) < minJWTSecretLen {
		return fmt.Errorf("JWT_SECRET must be set and at least %d characters (generate one with: openssl rand -base64 48)", minJWTSecretLen)
	}
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

	secret, err := getJWTSecret()
	if err != nil {
		return "", time.Time{}, err
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString(secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign jwt: %w", err)
	}
	return tokenString, exp, nil
}

// ValidateSessionToken ตรวจสอบความถูกต้องของ JWT token
func ValidateSessionToken(tokenString string) (*Claims, error) {
	secret, err := getJWTSecret()
	if err != nil {
		return nil, ErrInvalidToken
	}
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(t *jwt.Token) (interface{}, error) {
		return secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithIssuer("jodjod-api"), jwt.WithExpirationRequired())

	if err != nil {
		return nil, ErrInvalidToken
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, ErrInvalidToken
	}

	return claims, nil
}
