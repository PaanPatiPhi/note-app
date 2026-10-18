package auth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// fakeGoogle ตอบ tokeninfo/userinfo ตาม token ที่ส่งมา
func fakeGoogle(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tokeninfo":
			body, _ := io.ReadAll(r.Body)
			form, _ := url.ParseQuery(string(body))
			switch {
			case form.Get("id_token") == "a.ours.jwt":
				io.WriteString(w, `{"sub":"g-1","aud":"our-client","email":"me@example.com","email_verified":"true","name":"Me"}`)
			case form.Get("id_token") == "a.other.jwt":
				io.WriteString(w, `{"sub":"g-1","aud":"someone-elses-app","email":"me@example.com","email_verified":"true"}`)
			case form.Get("access_token") == "ours-access":
				io.WriteString(w, `{"sub":"g-1","azp":"our-client","aud":"our-client","email":"me@example.com","email_verified":"true"}`)
			case form.Get("access_token") == "other-access":
				io.WriteString(w, `{"sub":"g-1","azp":"someone-elses-app","aud":"someone-elses-app","email":"me@example.com","email_verified":"true"}`)
			case form.Get("access_token") == "unverified-access":
				io.WriteString(w, `{"sub":"g-2","azp":"our-client","aud":"our-client","email":"x@example.com","email_verified":"false"}`)
			default:
				w.WriteHeader(http.StatusBadRequest)
			}
		case "/userinfo":
			io.WriteString(w, `{"sub":"g-1","name":"Me","picture":"https://example.com/me.png","email":"me@example.com","email_verified":true}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	oldInfo, oldUser := googleTokenInfoURL, googleUserInfoURL
	googleTokenInfoURL, googleUserInfoURL = srv.URL+"/tokeninfo", srv.URL+"/userinfo"
	t.Cleanup(func() { googleTokenInfoURL, googleUserInfoURL = oldInfo, oldUser })
}

func TestVerifyGoogleTokenChecksAudience(t *testing.T) {
	fakeGoogle(t)
	t.Setenv("GOOGLE_CLIENT_ID", "our-client")
	ctx := context.Background()

	for _, tok := range []string{"a.ours.jwt", "ours-access"} {
		g, err := VerifyGoogleToken(ctx, tok)
		if err != nil || g.Sub != "g-1" || !g.EmailVerified || g.Name != "Me" {
			t.Errorf("VerifyGoogleToken(%q) = %+v, %v", tok, g, err)
		}
	}
	// token ที่ออกให้แอปอื่นต้องใช้ login แอปเราไม่ได้
	for _, tok := range []string{"a.other.jwt", "other-access", "garbage"} {
		if g, err := VerifyGoogleToken(ctx, tok); err == nil {
			t.Errorf("VerifyGoogleToken(%q) = %+v, want error", tok, g)
		}
	}
	if g, err := VerifyGoogleToken(ctx, "unverified-access"); err != nil || g.EmailVerified || verifiedEmail(g) != "" {
		t.Errorf("unverified email must not be used: %+v, %v", g, err)
	}

	t.Setenv("GOOGLE_CLIENT_ID", "")
	if _, err := VerifyGoogleToken(ctx, "a.ours.jwt"); err == nil {
		t.Error("google sign-in must fail closed when GOOGLE_CLIENT_ID is not set")
	}
}
