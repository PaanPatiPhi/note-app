package auth

import (
	"errors"
	"log"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"jodjod/internal/user"
)

const minPasswordLen = 6

type linkEmailRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

type handoffExchangeRequest struct {
	Code string `json:"code"`
}

// linkEmail: POST /api/auth/link/email — เพิ่ม login ด้วยอีเมล+รหัสผ่านให้บัญชีที่ใช้ LINE/Google อยู่
func (h *Handler) linkEmail(c *gin.Context) {
	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	var req linkEmailRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ข้อมูลที่ส่งมาไม่ถูกต้อง"})
		return
	}
	email := strings.TrimSpace(strings.ToLower(req.Email))
	if _, err := mail.ParseAddress(email); err != nil || email == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "รูปแบบอีเมลไม่ถูกต้อง"})
		return
	}
	if len(req.Password) < minPasswordLen {
		c.JSON(http.StatusBadRequest, gin.H{"error": "รหัสผ่านต้องมีความยาวอย่างน้อย 6 ตัวอักษร"})
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ไม่สามารถประมวลผลรหัสผ่านได้"})
		return
	}

	u, err := h.userRepo.SetEmailPassword(c.Request.Context(), userID, email, string(hash))
	switch {
	case errors.Is(err, user.ErrEmailAlreadyRegistered), errors.Is(err, user.ErrIdentityAlreadyLinked):
		c.JSON(http.StatusConflict, gin.H{"error": "อีเมลนี้ถูกใช้กับบัญชีอื่นแล้ว"})
		return
	case errors.Is(err, user.ErrPasswordAlreadySet):
		c.JSON(http.StatusConflict, gin.H{"error": "บัญชีนี้ตั้งรหัสผ่านไว้แล้ว ใช้เปลี่ยนรหัสผ่านแทน"})
		return
	case errors.Is(err, user.ErrEmailMismatch):
		c.JSON(http.StatusBadRequest, gin.H{"error": "บัญชีนี้ใช้อีเมลอื่นอยู่แล้ว ให้ตั้งรหัสผ่านกับอีเมลนั้น"})
		return
	case err != nil:
		log.Println("link email ล้มเหลว:", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "เพิ่มอีเมลไม่สำเร็จ"})
		return
	}

	identities, _ := h.userRepo.GetIdentitiesByUserID(c.Request.Context(), userID)
	c.JSON(http.StatusOK, gin.H{
		"success":    true,
		"message":    "ตั้งอีเมลและรหัสผ่านแล้ว",
		"user":       u,
		"identities": identities,
	})
}

// changePassword: POST /api/auth/password — ต้องใส่รหัสผ่านเดิมให้ถูกก่อน
func (h *Handler) changePassword(c *gin.Context) {
	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	var req changePasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ข้อมูลที่ส่งมาไม่ถูกต้อง"})
		return
	}
	if len(req.NewPassword) < minPasswordLen {
		c.JSON(http.StatusBadRequest, gin.H{"error": "รหัสผ่านต้องมีความยาวอย่างน้อย 6 ตัวอักษร"})
		return
	}
	u, err := h.userRepo.FindByID(c.Request.Context(), userID)
	if err != nil || u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	if u.PasswordHash == nil || *u.PasswordHash == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "บัญชีนี้ยังไม่ได้ตั้งรหัสผ่าน"})
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(*u.PasswordHash), []byte(req.CurrentPassword)) != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "รหัสผ่านเดิมไม่ถูกต้อง"})
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ไม่สามารถประมวลผลรหัสผ่านได้"})
		return
	}
	if err := h.userRepo.UpdatePassword(c.Request.Context(), userID, string(hash)); err != nil {
		log.Println("change password ล้มเหลว:", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "เปลี่ยนรหัสผ่านไม่สำเร็จ"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "เปลี่ยนรหัสผ่านแล้ว"})
}

// createHandoff: POST /api/auth/handoff — ออกรหัสใช้ครั้งเดียวให้เปิดเว็บใน browser ภายนอกแบบ login ค้างไว้
func (h *Handler) createHandoff(c *gin.Context) {
	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	code, err := h.handoffs.issue(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "สร้างรหัสไม่สำเร็จ"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": code, "expires_in": int(handoffTTL.Seconds())})
}

// exchangeHandoff: POST /api/auth/handoff/exchange — แลกรหัสเป็น session (public: browser ภายนอกยังไม่มี session)
func (h *Handler) exchangeHandoff(c *gin.Context) {
	var req handoffExchangeRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Code == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ข้อมูลที่ส่งมาไม่ถูกต้อง"})
		return
	}
	userID, ok := h.handoffs.redeem(req.Code)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "ลิงก์หมดอายุหรือถูกใช้ไปแล้ว ลองกดใหม่จากใน LINE"})
		return
	}
	u, err := h.userRepo.FindByID(c.Request.Context(), userID)
	if err != nil || u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	sessionToken, exp, err := GenerateSessionToken(u.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "สร้างเซสชันล้มเหลว"})
		return
	}
	maxAge := int(time.Until(exp).Seconds())
	if maxAge <= 0 {
		maxAge = 30 * 24 * 3600
	}
	h.setSessionCookie(c, sessionToken, maxAge)

	identities, _ := h.userRepo.GetIdentitiesByUserID(c.Request.Context(), u.ID)
	c.JSON(http.StatusOK, gin.H{
		"user":       u,
		"token":      sessionToken,
		"identities": identities,
	})
}
