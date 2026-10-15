package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

// verifyResponse คือ response จาก LINE token verification endpoint
type verifyResponse struct {
	Sub          string `json:"sub"`     // LINE user ID
	Name         string `json:"name"`    // display name (ต้องขอ scope "profile" ตอน login)
	Picture      string `json:"picture"` // URL รูปโปรไฟล์ (มีเมื่อขอ scope "profile" เช่นกัน)
	Aud          string `json:"aud"`     // ต้องตรงกับ LIFF channel ID ของเรา
	Error        string `json:"error"`
	ErrorDetails string `json:"error_description"`
}

// VerifyIDToken ยืนยัน LIFF ID Token กับ LINE โดยตรง (เรียก LINE API ทุกครั้ง
// ไม่ได้ verify ด้วย public key เอง เพื่อความง่ายของ MVP)
// คืน (lineUserID, displayName, pictureURL, error)
func VerifyIDToken(ctx context.Context, idToken, channelID string) (string, string, string, error) {
	form := url.Values{}
	form.Set("id_token", idToken)
	form.Set("client_id", channelID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.line.me/oauth2/v2.1/verify", strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", "", fmt.Errorf("build verify request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("call line verify: %w", err)
	}
	defer resp.Body.Close()

	var v verifyResponse
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", "", "", fmt.Errorf("decode verify response: %w", err)
	}

	log.Printf("LINE verify response - Status: %d, Error: %s, ErrorDetails: %s, Aud: %s",
		resp.StatusCode, v.Error, v.ErrorDetails, v.Aud)

	if resp.StatusCode != http.StatusOK || v.Error != "" {
		return "", "", "", fmt.Errorf("invalid id token: %s", v.ErrorDetails)
	}
	if v.Aud != channelID {
		return "", "", "", errors.New("token audience mismatch")
	}

	return v.Sub, v.Name, v.Picture, nil
}

// RequireLIFFAuth เป็น Gin middleware: อ่าน "Authorization: Bearer <idToken>",
// verify กับ LINE แล้วเก็บ line_user_id / display_name / picture ไว้ใน context
// ให้ handler ถัดไปใช้
func RequireLIFFAuth(channelID string) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		token := strings.TrimPrefix(header, "Bearer ")
		if token == "" || token == header {
			log.Println("Auth failed: missing bearer token")
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}

		log.Printf("Auth attempt - Channel ID: %s, Token length: %d, Token prefix: %s",
			channelID, len(token), func() string {
				if len(token) > 20 {
					return token[:20] + "..."
				}
				return token
			}())

		lineUserID, displayName, picture, err := VerifyIDToken(c.Request.Context(), token, channelID)
		if err != nil {
			log.Printf("Auth failed: %v", err)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token", "details": err.Error()})
			return
		}

		log.Printf("Auth success for user: %s, display name: %s", lineUserID, displayName)
		c.Set("line_user_id", lineUserID)
		c.Set("display_name", displayName)
		c.Set("picture", picture)
		c.Next()
	}
}
