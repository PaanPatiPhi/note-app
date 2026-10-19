package assistant

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

// เว็บ/แอปรอได้นานกว่า LINE (ไม่มี reply token หมดอายุ) แต่ไม่ควรค้างนานเกินไป
const apiTimeout = 30 * time.Second

// APIHandler เปิด assistant ให้เว็บและแอปใช้ได้โดยไม่ผ่าน LINE
// ต้องอยู่หลัง auth middleware (ใช้ "user_id" จาก context เสมอ ไม่รับ user จาก body)
type APIHandler struct {
	ai *Assistant
}

func NewAPIHandler(ai *Assistant) *APIHandler {
	return &APIHandler{ai: ai}
}

func (h *APIHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.POST("/assistant/messages", h.message)
	rg.POST("/assistant/actions/:id/undo", h.undo)
	rg.GET("/assistant/history", h.history)
}

type messageRequest struct {
	Text string `json:"text"`
}

type messageResponse struct {
	Reply    string `json:"reply"`
	ActionID string `json:"action_id,omitempty"`
	// Undoable = AI สร้าง/แก้ข้อมูล และกดยกเลิกได้ (ส่ง action_id ไปที่ /undo)
	Undoable bool `json:"undoable"`
	// Wrote = มีข้อมูลเปลี่ยน ให้ client โหลดรายการโน้ตใหม่
	Wrote bool `json:"wrote"`
}

// message: POST /api/assistant/messages {"text": "ซื้อนม ไข่ ขนมปัง"}
func (h *APIHandler) message(c *gin.Context) {
	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	if h.ai == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "assistant is disabled"})
		return
	}

	var req messageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "text is empty"})
		return
	}
	if utf8.RuneCountInString(text) > maxContentRunes {
		c.JSON(http.StatusBadRequest, gin.H{"error": "text is too long"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), apiTimeout)
	defer cancel()
	res, err := h.ai.Handle(ctx, Request{UserID: userID, Text: text})
	if err != nil {
		// error = ยังไม่ได้เขียนข้อมูลใดเลย (เช่น Ollama ไม่ได้เปิด) ผู้ใช้ลองใหม่ได้ปลอดภัย
		log.Println("assistant API ล้มเหลว:", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "assistant unavailable", "reply": fallbackReply})
		return
	}

	c.JSON(http.StatusOK, messageResponse{
		Reply:    res.Reply,
		ActionID: res.ActionID,
		Undoable: res.Wrote && res.ActionID != "",
		Wrote:    res.Wrote,
	})
}

// undo: POST /api/assistant/actions/:id/undo
// ย้อนได้เฉพาะ action ของ user คนนี้ (Undo ผูก user_id ทุกคำสั่ง)
func (h *APIHandler) undo(c *gin.Context) {
	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	if h.ai == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "assistant is disabled"})
		return
	}
	if h.ai.actions == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "undo is not available"})
		return
	}

	res, err := h.ai.UndoAction(c.Request.Context(), userID, c.Param("id"))
	if err != nil {
		log.Println("assistant undo API ล้มเหลว:", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "undo failed"})
		return
	}
	status := http.StatusOK
	switch {
	case res.NotFound:
		status = http.StatusNotFound
	case res.AlreadyUndone, res.Expired:
		status = http.StatusConflict
	}
	c.JSON(status, gin.H{
		"message":           undoReplyText(res),
		"deleted_notes":     res.DeletedNotes,
		"removed_items":     res.RemovedItems,
		"restored_items":    res.RestoredItems,
		"restored_pins":     res.RestoredPins,
		"deleted_events":    res.DeletedEvents,
		"moved_back_events": res.MovedBack,
		"recreated_events":  res.RecreatedEvents,
	})
}

const (
	defaultHistoryLimit = 30
	maxHistoryLimit     = 100
)

// history: GET /api/assistant/history?limit=30&before=<RFC3339>
// ประวัติที่สั่ง AI ทั้งจาก LINE และเว็บ เรียงเก่า→ใหม่; next_before ใช้โหลดหน้าที่เก่ากว่า (null = หมดแล้ว)
func (h *APIHandler) history(c *gin.Context) {
	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	if h.ai == nil || h.ai.actions == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "assistant is disabled"})
		return
	}

	limit := defaultHistoryLimit
	if s := c.Query("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit"})
			return
		}
		limit = min(n, maxHistoryLimit)
	}
	var before *time.Time
	if s := c.Query("before"); s != "" {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid before"})
			return
		}
		before = &t
	}

	items, err := h.ai.actions.History(c.Request.Context(), userID, limit, before)
	if err != nil {
		log.Println("assistant history API ล้มเหลว:", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "history failed"})
		return
	}
	if items == nil {
		items = []HistoryItem{}
	}
	var next *string
	if len(items) == limit {
		s := items[0].CreatedAt.Format(time.RFC3339Nano)
		next = &s
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "next_before": next})
}
