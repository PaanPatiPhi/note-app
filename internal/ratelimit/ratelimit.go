// Package ratelimit จำกัดจำนวน request ต่อช่วงเวลา (เก็บในหน่วยความจำ ใช้ได้กับ server ตัวเดียว)
// กันเดารหัสผ่านรัวๆ และกันคนยิงข้อความใส่ AI จนเสียเงิน/เครื่องค้าง
package ratelimit

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// Rule คือขีดจำกัดของหนึ่ง route: ไม่เกิน Limit ครั้งต่อ Window ต่อหนึ่ง key (IP หรือ user)
type Rule struct {
	Limit  int
	Window time.Duration
}

type window struct {
	start time.Time
	count int
}

// Limiter นับแบบ fixed window แยกตาม route + key
type Limiter struct {
	mu      sync.Mutex
	rules   map[string]Rule // key = "METHOD path" เช่น "POST /api/auth/login"
	windows map[string]*window
	now     func() time.Time
	lastGC  time.Time
}

func New(rules map[string]Rule) *Limiter {
	return &Limiter{rules: rules, windows: map[string]*window{}, now: time.Now}
}

// allow คืน (อนุญาตไหม, ต้องรออีกกี่วินาที)
func (l *Limiter) allow(route, key string, r Rule) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.lastGC) > time.Minute {
		for k, w := range l.windows {
			if now.Sub(w.start) > time.Hour {
				delete(l.windows, k)
			}
		}
		l.lastGC = now
	}
	id := route + "\x00" + key
	w := l.windows[id]
	if w == nil || now.Sub(w.start) >= r.Window {
		l.windows[id] = &window{start: now, count: 1}
		return true, 0
	}
	if w.count >= r.Limit {
		return false, r.Window - now.Sub(w.start)
	}
	w.count++
	return true, 0
}

func (l *Limiter) middleware(keyOf func(*gin.Context) string) gin.HandlerFunc {
	return func(c *gin.Context) {
		route := c.Request.Method + " " + c.FullPath()
		r, ok := l.rules[route]
		if !ok {
			c.Next()
			return
		}
		key := keyOf(c)
		if key == "" {
			c.Next()
			return
		}
		if ok, wait := l.allow(route, key, r); !ok {
			secs := int(wait.Seconds()) + 1
			c.Header("Retry-After", strconv.Itoa(secs))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": fmt.Sprintf("ส่งคำขอถี่เกินไป ลองใหม่ในอีก %d วินาที", secs),
			})
			return
		}
		c.Next()
	}
}

// ByIP จำกัดตาม IP ของผู้เรียก (ใช้กับ route ที่ยังไม่ได้ login เช่น login/register)
func (l *Limiter) ByIP() gin.HandlerFunc {
	return l.middleware(func(c *gin.Context) string { return c.ClientIP() })
}

// ByUser จำกัดตาม user ที่ login แล้ว (ต้องอยู่หลัง auth middleware ที่ตั้ง "user_id")
func (l *Limiter) ByUser() gin.HandlerFunc {
	return l.middleware(func(c *gin.Context) string { return c.GetString("user_id") })
}

// BodyLimit จำกัดขนาด request body; route ใน overrides ใช้ขนาดที่กำหนดแทน (เช่น อัปโหลดรูป)
func BodyLimit(defaultMax int64, overrides map[string]int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		max := defaultMax
		if m, ok := overrides[c.FullPath()]; ok {
			max = m
		}
		if c.Request.ContentLength > max {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "ข้อมูลที่ส่งมาใหญ่เกินไป"})
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, max)
		c.Next()
	}
}
