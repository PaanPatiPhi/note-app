package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
)

type googleTokenInfo struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified string `json:"email_verified"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
	Aud           string `json:"aud"`
	Error         string `json:"error"`
	ErrorDesc     string `json:"error_description"`
}

// VerifyGoogleIDToken ตรวจสอบ Google ID token หรือ OAuth2 Access token กับ Google API
// คืน (googleUserID, email, name, picture, error)
func VerifyGoogleIDToken(ctx context.Context, token string) (string, string, string, string, error) {
	if token == "" {
		return "", "", "", "", errors.New("empty google token")
	}

	expectedClientID := os.Getenv("GOOGLE_CLIENT_ID")

	// 1. ตรวจสอบในฐานะ ID Token (JWT)
	endpoint := "https://oauth2.googleapis.com/tokeninfo?id_token=" + url.QueryEscape(token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err == nil {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var info googleTokenInfo
				if err := json.NewDecoder(resp.Body).Decode(&info); err == nil && info.Error == "" && info.Sub != "" {
					if expectedClientID != "" && info.Aud != expectedClientID {
						return "", "", "", "", errors.New("google token audience mismatch")
					}
					return info.Sub, info.Email, info.Name, info.Picture, nil
				}
			}
		}
	}

	// 2. ถ้าไม่ใช่ ID Token ให้ตรวจสอบในฐานะ OAuth2 Access Token ผ่าน userinfo API
	userinfoEndpoint := "https://www.googleapis.com/oauth2/v3/userinfo"
	uReq, err := http.NewRequestWithContext(ctx, http.MethodGet, userinfoEndpoint, nil)
	if err != nil {
		return "", "", "", "", fmt.Errorf("create google userinfo request: %w", err)
	}
	uReq.Header.Set("Authorization", "Bearer "+token)

	uResp, err := http.DefaultClient.Do(uReq)
	if err != nil {
		return "", "", "", "", fmt.Errorf("call google userinfo: %w", err)
	}
	defer uResp.Body.Close()

	if uResp.StatusCode == http.StatusOK {
		var uInfo struct {
			Sub     string `json:"sub"`
			Email   string `json:"email"`
			Name    string `json:"name"`
			Picture string `json:"picture"`
			Error   string `json:"error"`
		}
		if err := json.NewDecoder(uResp.Body).Decode(&uInfo); err == nil && uInfo.Error == "" && uInfo.Sub != "" {
			return uInfo.Sub, uInfo.Email, uInfo.Name, uInfo.Picture, nil
		}
	}

	return "", "", "", "", errors.New("invalid google token: verification failed for id_token and access_token")
}
