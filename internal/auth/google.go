package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// GoogleIdentity คือข้อมูลผู้ใช้จาก Google ที่ยืนยันแล้วว่า token ออกให้แอปเรา
type GoogleIdentity struct {
	Sub           string
	Email         string
	EmailVerified bool
	Name          string
	Picture       string
}

// endpoint ของ Google (เปลี่ยนได้ในเทส)
var (
	googleTokenInfoURL = "https://oauth2.googleapis.com/tokeninfo"
	googleUserInfoURL  = "https://www.googleapis.com/oauth2/v3/userinfo"
	googleHTTPClient   = &http.Client{Timeout: 10 * time.Second}
)

// tokeninfo คืนทั้งแบบ id_token และ access_token; email_verified เป็น string หรือ bool แล้วแต่ชนิด token
type googleTokenInfo struct {
	Sub           string          `json:"sub"`
	Aud           string          `json:"aud"`
	Azp           string          `json:"azp"`
	Email         string          `json:"email"`
	EmailVerified json.RawMessage `json:"email_verified"`
	Name          string          `json:"name"`
	Picture       string          `json:"picture"`
}

func (t googleTokenInfo) emailVerified() bool {
	v := strings.Trim(strings.TrimSpace(string(t.EmailVerified)), `"`)
	return v == "true"
}

// VerifyGoogleToken ตรวจ Google ID token หรือ access token และต้องเป็น token ที่ออกให้ GOOGLE_CLIENT_ID ของเราเท่านั้น
// ถ้าไม่ตรวจ audience ใครที่ได้ token ของเหยื่อจากแอปอื่นของ Google ก็เอามา login เป็นเหยื่อได้
func VerifyGoogleToken(ctx context.Context, token string) (*GoogleIdentity, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("empty google token")
	}
	clientID := strings.TrimSpace(os.Getenv("GOOGLE_CLIENT_ID"))
	if clientID == "" {
		return nil, errors.New("google sign-in is not configured on the server")
	}

	// ID token เป็น JWT (มีจุดสองจุด) ส่วน access token ไม่ใช่
	param := "access_token"
	if strings.Count(token, ".") == 2 {
		param = "id_token"
	}
	info, err := googleTokenInfoFor(ctx, param, token)
	if err != nil {
		return nil, err
	}
	if info.Sub == "" {
		return nil, errors.New("invalid google token")
	}
	if info.Aud != clientID && info.Azp != clientID {
		return nil, errors.New("google token was not issued for this app")
	}

	id := &GoogleIdentity{Sub: info.Sub, Email: info.Email, EmailVerified: info.emailVerified(), Name: info.Name, Picture: info.Picture}
	if param == "access_token" && (id.Name == "" || id.Picture == "") {
		// tokeninfo ของ access token ไม่มีชื่อ/รูป — ดึงจาก userinfo (หลังยืนยัน audience แล้วเท่านั้น)
		if u, err := googleUserInfo(ctx, token); err == nil && u.Sub == id.Sub {
			id.Name, id.Picture = u.Name, u.Picture
			if id.Email == "" {
				id.Email, id.EmailVerified = u.Email, u.emailVerified()
			}
		}
	}
	return id, nil
}

func googleTokenInfoFor(ctx context.Context, param, token string) (*googleTokenInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, googleTokenInfoURL,
		strings.NewReader(url.Values{param: {token}}.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := googleHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call google tokeninfo: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("invalid google token")
	}
	var info googleTokenInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decode google tokeninfo: %w", err)
	}
	return &info, nil
}

func googleUserInfo(ctx context.Context, accessToken string) (*googleTokenInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, googleUserInfoURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := googleHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("google userinfo HTTP %d", resp.StatusCode)
	}
	var info googleTokenInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}
	return &info, nil
}
