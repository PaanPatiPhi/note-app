package subscription

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"jodjod/internal/user"
)

type Handler struct {
	repo     *Repository
	userRepo *user.Repository
}

func NewHandler(repo *Repository, userRepo *user.Repository) *Handler {
	return &Handler{
		repo:     repo,
		userRepo: userRepo,
	}
}

func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	group := rg.Group("/subscription")
	{
		group.POST("/subscribe", h.Subscribe)
		group.GET("/current", h.GetCurrent)
		group.GET("/history", h.GetHistory)
		group.POST("/cancel", h.Cancel)
	}
}

type subscribeRequest struct {
	Tier          string `json:"tier"`           // "free", "premium"
	BillingCycle  string `json:"billing_cycle"`  // "monthly", "yearly"
	PaymentMethod string `json:"payment_method"` // "promptpay", "credit_card", "free"
}

func (h *Handler) Subscribe(c *gin.Context) {
	userID, ok := c.Get("user_id")
	if !ok || userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	uid := userID.(string)

	var req subscribeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ข้อมูลการสมัครไม่ถูกต้อง"})
		return
	}

	req.Tier = strings.ToLower(strings.TrimSpace(req.Tier))
	req.BillingCycle = strings.ToLower(strings.TrimSpace(req.BillingCycle))
	req.PaymentMethod = strings.ToLower(strings.TrimSpace(req.PaymentMethod))

	if req.Tier != "free" && req.Tier != "premium" {
		req.Tier = "premium"
	}
	// ยังไม่มีระบบรับเงินจริง: ถ้าให้สมัคร premium ผ่าน endpoint นี้ได้ ใครก็กดเป็น premium ฟรีได้
	// ระหว่างนี้ admin ให้ premium ผ่าน /api/admin/users/:id/tier แทน
	if req.Tier == "premium" {
		c.JSON(http.StatusPaymentRequired, gin.H{"error": "ยังไม่เปิดรับชำระเงิน ติดต่อผู้ดูแลระบบเพื่อเปิด Premium"})
		return
	}
	if req.BillingCycle == "" {
		req.BillingCycle = "monthly"
	}
	if req.PaymentMethod == "" {
		if req.Tier == "free" {
			req.PaymentMethod = "free"
		} else {
			req.PaymentMethod = "promptpay"
		}
	}

	now := time.Now()
	var amount float64
	var expiresAt *time.Time

	if req.Tier == "premium" {
		if req.BillingCycle == "yearly" {
			amount = 990.00
			exp := now.AddDate(1, 0, 0)
			expiresAt = &exp
		} else {
			amount = 99.00
			exp := now.AddDate(0, 1, 0)
			expiresAt = &exp
		}
	} else {
		amount = 0
		req.BillingCycle = "lifetime"
		req.PaymentMethod = "free"
	}

	// 1. ยกเลิก subscription เดิมที่ active อยู่
	_ = h.repo.CancelActiveByUser(c.Request.Context(), uid)

	// 2. สร้าง subscription record ใหม่
	sub := &Subscription{
		UserID:        uid,
		Tier:          req.Tier,
		BillingCycle:  req.BillingCycle,
		Amount:        amount,
		Currency:      "THB",
		Status:        "active",
		StartsAt:      now,
		ExpiresAt:     expiresAt,
		PaymentMethod: req.PaymentMethod,
	}

	if err := h.repo.Create(c.Request.Context(), sub); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "บันทึกการสมัครไม่สำเร็จ"})
		return
	}

	// 3. อัปเดต Tier ของ User ในตาราง users
	if err := h.userRepo.UpdateTier(c.Request.Context(), uid, req.Tier); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "อัปเดตสถานะผู้ใช้ไม่สำเร็จ"})
		return
	}

	msg := "อัปเกรดเป็นแพ็กเกจ Premium เรียบร้อยแล้ว ยินดีต้อนรับสู่ประสบการณ์ไร้ขีดจำกัด!"
	if req.Tier == "free" {
		msg = "เปลี่ยนเป็นแพ็กเกจ Free เรียบร้อยแล้ว"
	}

	c.JSON(http.StatusOK, gin.H{
		"success":      true,
		"subscription": sub,
		"tier":         req.Tier,
		"message":      msg,
	})
}

func (h *Handler) GetCurrent(c *gin.Context) {
	userID, ok := c.Get("user_id")
	if !ok || userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	uid := userID.(string)

	sub, err := h.repo.GetActiveByUserID(c.Request.Context(), uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ไม่สามารถดึงข้อมูลแพ็กเกจได้"})
		return
	}

	u, err := h.userRepo.FindByID(c.Request.Context(), uid)
	tier := "free"
	if u != nil && u.Tier != "" {
		tier = u.Tier
	}

	c.JSON(http.StatusOK, gin.H{
		"tier":         tier,
		"subscription": sub,
	})
}

func (h *Handler) GetHistory(c *gin.Context) {
	userID, ok := c.Get("user_id")
	if !ok || userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	uid := userID.(string)

	list, err := h.repo.ListByUserID(c.Request.Context(), uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ไม่สามารถดึงประวัติได้"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"subscriptions": list,
	})
}

func (h *Handler) Cancel(c *gin.Context) {
	userID, ok := c.Get("user_id")
	if !ok || userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	uid := userID.(string)

	if err := h.repo.CancelActiveByUser(c.Request.Context(), uid); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ยกเลิกแพ็กเกจไม่สำเร็จ"})
		return
	}

	// Downgrade user back to free
	_ = h.userRepo.UpdateTier(c.Request.Context(), uid, "free")

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"tier":    "free",
		"message": "ยกเลิกแพ็กเกจเรียบร้อยแล้ว บัญชีของคุณถูกปรับเป็น Free",
	})
}
