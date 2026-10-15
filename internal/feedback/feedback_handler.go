package feedback

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

type Handler struct {
	repo *Repository
}

func NewHandler(repo *Repository) *Handler {
	return &Handler{repo: repo}
}

type createFeedbackRequest struct {
	Type         string  `json:"type"`
	Title        string  `json:"title"`
	Message      string  `json:"message"`
	ContactEmail *string `json:"contact_email,omitempty"`
}

func (h *Handler) RegisterRoutes(protectedGroup *gin.RouterGroup) {
	protectedGroup.POST("/feedback", h.Create)
	protectedGroup.GET("/feedback", h.List)
}

func (h *Handler) Create(c *gin.Context) {
	var req createFeedbackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ข้อมูลไม่ถูกต้อง"})
		return
	}

	req.Title = strings.TrimSpace(req.Title)
	req.Message = strings.TrimSpace(req.Message)
	req.Type = strings.TrimSpace(req.Type)

	if req.Type == "" {
		req.Type = "suggestion"
	}
	if req.Title == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "กรุณาระบุหัวข้อเรื่อง"})
		return
	}
	if req.Message == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "กรุณาระบุรายละเอียดข้อความ"})
		return
	}

	var userID string
	if v, ok := c.Get("user_id"); ok {
		if uid, ok := v.(string); ok && uid != "" {
			userID = uid
		}
	}
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "กรุณาเข้าสู่ระบบก่อนส่งข้อเสนอแนะ"})
		return
	}

	if req.ContactEmail != nil {
		trimmedEmail := strings.TrimSpace(*req.ContactEmail)
		if trimmedEmail == "" {
			req.ContactEmail = nil
		} else {
			req.ContactEmail = &trimmedEmail
		}
	}

	f := &Feedback{
		UserID:       &userID,
		Type:         req.Type,
		Title:        req.Title,
		Message:      req.Message,
		ContactEmail: req.ContactEmail,
	}

	if err := h.repo.Create(c.Request.Context(), f); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "บันทึกข้อเสนอแนะไม่สำเร็จ"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"success":  true,
		"feedback": f,
		"message":  "ส่งข้อเสนอแนะสำเร็จ ขอบคุณสำหรับความคิดเห็นของคุณ!",
	})
}

func (h *Handler) List(c *gin.Context) {
	var userID string
	if v, ok := c.Get("user_id"); ok {
		userID, _ = v.(string)
	}
	if userID == "" {
		c.JSON(http.StatusOK, gin.H{"feedbacks": []any{}})
		return
	}

	feedbacks, err := h.repo.ListByUserID(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "โหลดข้อมูลไม่สำเร็จ"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"feedbacks": feedbacks})
}
