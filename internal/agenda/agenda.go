// Package agenda รวมสิ่งที่กำลังจะมาถึงของผู้ใช้ (เตือน, โน้ตที่มีกำหนด, นัดใน Google Calendar)
// เป็นรายการเดียวเรียงตามเวลา ให้หน้า LIFF / เว็บแสดง
package agenda

import (
	"context"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"jodjod/internal/integrations"
	"jodjod/internal/note"
)

const (
	defaultDays     = 14
	maxDays         = 60
	calendarTimeout = 8 * time.Second
)

var bangkok = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Bangkok")
	if err != nil {
		return time.FixedZone("Asia/Bangkok", 7*60*60)
	}
	return loc
}()

// NoteFinder คือ operation ของ note.Repository ที่ใช้
type NoteFinder interface {
	FindAgenda(ctx context.Context, userID string, from, to time.Time) ([]*note.Note, error)
}

// Calendar คือปฏิทินภายนอก (implement โดย *integrations.Google)
type Calendar interface {
	Connected(ctx context.Context, userID string) (bool, error)
	ListEvents(ctx context.Context, userID string, from, to time.Time) ([]integrations.Event, error)
}

// Item คือหนึ่งรายการในตาราง
type Item struct {
	Kind   string     `json:"kind"` // reminder | due | event
	ID     string     `json:"id"`
	Title  string     `json:"title"`
	Start  time.Time  `json:"start"`
	End    *time.Time `json:"end,omitempty"`
	AllDay bool       `json:"all_day,omitempty"`
	Done   bool       `json:"done,omitempty"`
	Link   string     `json:"link,omitempty"`
}

type Response struct {
	Items             []Item    `json:"items"`
	From              time.Time `json:"from"`
	To                time.Time `json:"to"`
	CalendarConnected bool      `json:"calendar_connected"`
	// CalendarError = เชื่อมไว้แต่อ่านปฏิทินไม่สำเร็จ (รายการจากโน้ตยังแสดงได้)
	CalendarError bool `json:"calendar_error,omitempty"`
}

type Handler struct {
	notes    NoteFinder
	calendar Calendar // nil = ไม่ได้ตั้งค่า Google Calendar
	now      func() time.Time
}

func NewHandler(notes NoteFinder, calendar Calendar) *Handler {
	return &Handler{notes: notes, calendar: calendar, now: time.Now}
}

func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/agenda", h.list)
}

// list: GET /api/agenda?days=14 → ตั้งแต่ต้นวันนี้ (เวลาไทย) ไปอีก days วัน
func (h *Handler) list(c *gin.Context) {
	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	days := defaultDays
	if s := c.Query("days"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxDays {
			c.JSON(http.StatusBadRequest, gin.H{"error": "days must be between 1 and 60"})
			return
		}
		days = n
	}
	res, err := h.Build(c.Request.Context(), userID, days)
	if err != nil {
		log.Println("โหลด agenda ล้มเหลว:", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "agenda failed"})
		return
	}
	c.JSON(http.StatusOK, res)
}

// Build รวมรายการจากโน้ตและปฏิทิน
func (h *Handler) Build(ctx context.Context, userID string, days int) (*Response, error) {
	now := h.now().In(bangkok)
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, bangkok)
	to := from.AddDate(0, 0, days)
	res := &Response{Items: []Item{}, From: from, To: to}

	notes, err := h.notes.FindAgenda(ctx, userID, from, to)
	if err != nil {
		return nil, err
	}
	for _, n := range notes {
		title := ""
		if n.Content != nil {
			title = strings.TrimSpace(*n.Content)
		}
		switch {
		case n.RemindAt != nil:
			res.Items = append(res.Items, Item{Kind: "reminder", ID: n.ID, Title: title, Start: n.RemindAt.In(bangkok), Done: n.IsDone})
		case n.DueDate != nil:
			d := n.DueDate.In(bangkok)
			res.Items = append(res.Items, Item{Kind: "due", ID: n.ID, Title: title, Start: time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, bangkok), AllDay: true, Done: n.IsDone})
		}
	}

	if h.calendar != nil {
		connected, err := h.calendar.Connected(ctx, userID)
		if err != nil {
			log.Println("เช็คการเชื่อม Google Calendar ล้มเหลว:", err)
		}
		res.CalendarConnected = connected
		if connected {
			calCtx, cancel := context.WithTimeout(ctx, calendarTimeout)
			events, err := h.calendar.ListEvents(calCtx, userID, from, to)
			cancel()
			if err != nil {
				log.Println("อ่าน Google Calendar ล้มเหลว:", err)
				res.CalendarError = true
			}
			for _, e := range events {
				// "⏰ ..." คือสำเนาของ reminder ที่เราลงปฏิทินให้ มีรายการ reminder อยู่แล้ว
				if strings.HasPrefix(e.Title, "⏰") {
					continue
				}
				it := Item{Kind: "event", ID: e.ID, Title: e.Title, Link: e.Link, AllDay: e.AllDay}
				if e.AllDay {
					// วันที่ของนัดทั้งวันมาเป็นวันที่ล้วน ให้เป็นเที่ยงคืนเวลาไทยของวันนั้น
					it.Start = time.Date(e.Start.Year(), e.Start.Month(), e.Start.Day(), 0, 0, 0, 0, bangkok)
				} else {
					it.Start = e.Start.In(bangkok)
					end := e.End.In(bangkok)
					it.End = &end
				}
				res.Items = append(res.Items, it)
			}
		}
	}

	sort.SliceStable(res.Items, func(i, j int) bool { return res.Items[i].Start.Before(res.Items[j].Start) })
	return res, nil
}
