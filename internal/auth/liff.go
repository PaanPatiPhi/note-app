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

	if resp.StatusCode != http.StatusOK || v.Error != "" {
		log.Printf("LINE verify failed: status %d, %s", resp.StatusCode, v.Error)
		return "", "", "", fmt.Errorf("invalid id token: %s", v.ErrorDetails)
	}
	if v.Aud != channelID {
		return "", "", "", errors.New("token audience mismatch")
	}

	return v.Sub, v.Name, v.Picture, nil
}
