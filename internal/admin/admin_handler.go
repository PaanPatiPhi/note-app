package admin

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"jodjod/internal/feedback"
	"jodjod/internal/note"
	"jodjod/internal/subscription"
	"jodjod/internal/user"
)

type Handler struct {
	userRepo         *user.Repository
	feedbackRepo     *feedback.Repository
	subscriptionRepo *subscription.Repository
	noteRepo         *note.Repository
}

func NewHandler(
	userRepo *user.Repository,
	feedbackRepo *feedback.Repository,
	subscriptionRepo *subscription.Repository,
	noteRepo *note.Repository,
) *Handler {
	return &Handler{
		userRepo:         userRepo,
		feedbackRepo:     feedbackRepo,
		subscriptionRepo: subscriptionRepo,
		noteRepo:         noteRepo,
	}
}

// RequireAdmin middleware ตรวจสอบว่าผู้ใช้มีสิทธิ์เป็น admin หรือไม่
func (h *Handler) RequireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, ok := c.Get("user_id")
		if !ok || userID == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		uid := userID.(string)

		u, err := h.userRepo.FindByID(c.Request.Context(), uid)
		if err != nil || u == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "user not found"})
			return
		}

		if u.Role != "admin" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden: admin access required"})
			return
		}

		c.Next()
	}
}

func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	adminGroup := rg.Group("/admin", h.RequireAdmin())
	{
		adminGroup.GET("/stats", h.GetStats)
		adminGroup.GET("/feedbacks", h.GetFeedbacks)
		adminGroup.PATCH("/feedbacks/:id/status", h.UpdateFeedbackStatus)
		adminGroup.GET("/subscriptions", h.GetSubscriptions)
		adminGroup.GET("/users", h.GetUsers)
		adminGroup.PATCH("/users/:id/tier", h.UpdateUserTier)
		adminGroup.PATCH("/users/:id/role", h.UpdateUserRole)
	}
}

func (h *Handler) GetStats(c *gin.Context) {
	ctx := c.Request.Context()

	totalUsers, _ := h.userRepo.CountUsers(ctx)
	premiumUsers, _ := h.userRepo.CountUsersByTier(ctx, "premium")
	freeUsers, _ := h.userRepo.CountUsersByTier(ctx, "free")

	totalFeedbacks, _ := h.feedbackRepo.CountFeedbacks(ctx)
	pendingFeedbacks, _ := h.feedbackRepo.CountPendingFeedbacks(ctx)

	activeSubs, _ := h.subscriptionRepo.CountActiveSubscriptions(ctx)
	totalRevenue, _ := h.subscriptionRepo.TotalRevenue(ctx)

	totalNotes, _ := h.noteRepo.CountAll(ctx)

	c.JSON(http.StatusOK, gin.H{
		"total_users":         totalUsers,
		"premium_users":       premiumUsers,
		"free_users":          freeUsers,
		"total_feedbacks":     totalFeedbacks,
		"pending_feedbacks":   pendingFeedbacks,
		"active_subs":         activeSubs,
		"total_revenue":       totalRevenue,
		"total_notes":         totalNotes,
	})
}

func (h *Handler) GetFeedbacks(c *gin.Context) {
	typeFilter := strings.TrimSpace(c.Query("type"))
	statusFilter := strings.TrimSpace(c.Query("status"))

	feedbacks, err := h.feedbackRepo.ListAll(c.Request.Context(), typeFilter, statusFilter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list feedbacks"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"feedbacks": feedbacks})
}

type updateStatusRequest struct {
	Status string `json:"status"`
}

func (h *Handler) UpdateFeedbackStatus(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing feedback id"})
		return
	}

	var req updateStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
		return
	}

	req.Status = strings.ToLower(strings.TrimSpace(req.Status))
	if req.Status != "pending" && req.Status != "investigating" && req.Status != "resolved" && req.Status != "dismissed" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
		return
	}

	if err := h.feedbackRepo.UpdateStatus(c.Request.Context(), id, req.Status); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update feedback status"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"status":  req.Status,
		"message": "อัปเดตสถานะสำเร็จ",
	})
}

func (h *Handler) GetSubscriptions(c *gin.Context) {
	subs, err := h.subscriptionRepo.ListAll(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list subscriptions"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"subscriptions": subs})
}

func (h *Handler) GetUsers(c *gin.Context) {
	users, err := h.userRepo.ListAllUsers(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list users"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"users": users})
}

type updateUserTierRequest struct {
	Tier string `json:"tier"`
}

func (h *Handler) UpdateUserTier(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing user id"})
		return
	}

	var req updateUserTierRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
		return
	}

	req.Tier = strings.ToLower(strings.TrimSpace(req.Tier))
	if req.Tier != "free" && req.Tier != "premium" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tier must be free or premium"})
		return
	}

	if err := h.userRepo.UpdateTier(c.Request.Context(), id, req.Tier); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update user tier"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"tier":    req.Tier,
		"message": "อัปเดตแพ็กเกจของผู้ใช้สำเร็จ",
	})
}

type updateUserRoleRequest struct {
	Role string `json:"role"`
}

func (h *Handler) UpdateUserRole(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing user id"})
		return
	}

	var req updateUserRoleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
		return
	}

	req.Role = strings.ToLower(strings.TrimSpace(req.Role))
	if req.Role != "user" && req.Role != "admin" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "role must be user or admin"})
		return
	}

	if err := h.userRepo.UpdateRole(c.Request.Context(), id, req.Role); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update user role"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"role":    req.Role,
		"message": "อัปเดตสิทธิ์ของผู้ใช้สำเร็จ",
	})
}
