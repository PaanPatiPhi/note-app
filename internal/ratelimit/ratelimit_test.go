package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestLimiterByIP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	l := New(map[string]Rule{"POST /login": {Limit: 2, Window: time.Minute}})
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }

	r := gin.New()
	r.Use(l.ByIP())
	r.POST("/login", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/login", func(c *gin.Context) { c.Status(http.StatusOK) })

	do := func(method, ip string) int {
		req := httptest.NewRequest(method, "/login", nil)
		req.RemoteAddr = ip + ":1234"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	if do("POST", "1.1.1.1") != 200 || do("POST", "1.1.1.1") != 200 {
		t.Fatal("first two requests should pass")
	}
	if got := do("POST", "1.1.1.1"); got != http.StatusTooManyRequests {
		t.Errorf("third request = %d, want 429", got)
	}
	if do("POST", "2.2.2.2") != 200 || do("GET", "1.1.1.1") != 200 {
		t.Error("other IPs and other methods have their own limits")
	}
	now = now.Add(time.Minute)
	if do("POST", "1.1.1.1") != 200 {
		t.Error("limit should reset after the window")
	}
}

func TestBodyLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(BodyLimit(10, map[string]int64{"/big": 100}))
	handler := func(c *gin.Context) {
		if _, err := c.GetRawData(); err != nil {
			c.Status(http.StatusRequestEntityTooLarge)
			return
		}
		c.Status(http.StatusOK)
	}
	r.POST("/small", handler)
	r.POST("/big", handler)

	for _, tc := range []struct {
		path string
		size int
		want int
	}{{"/small", 5, 200}, {"/small", 50, 413}, {"/big", 50, 200}, {"/big", 500, 413}} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("POST", tc.path, strings.NewReader(strings.Repeat("x", tc.size))))
		if w.Code != tc.want {
			t.Errorf("POST %s with %d bytes = %d, want %d", tc.path, tc.size, w.Code, tc.want)
		}
	}
}
