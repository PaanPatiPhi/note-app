package auth

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"jodjod/internal/user"
)

type Handler struct {
	userRepo      *user.Repository
	liffChannelID string
}

func NewHandler(userRepo *user.Repository, liffChannelID string) *Handler {
	return &Handler{
		userRepo:      userRepo,
		liffChannelID: liffChannelID,
	}
}

func (h *Handler) RegisterPublicRoutes(rg *gin.RouterGroup) {
	rg.POST("/auth/register", h.registerWithEmail)
	rg.POST("/auth/login", h.loginWithEmail)
	rg.POST("/auth/google", h.loginWithGoogle)
	rg.POST("/auth/line", h.loginWithLine)
	rg.POST("/auth/logout", h.logout)
}

func (h *Handler) RegisterProtectedRoutes(rg *gin.RouterGroup) {
	rg.GET("/auth/me", h.getMe)
	rg.POST("/auth/avatar", h.uploadAvatar)
	rg.PATCH("/auth/profile", h.updateProfile)
	rg.POST("/auth/link/line", h.linkLine)
	rg.POST("/auth/unlink/line", h.unlinkLine)
	rg.POST("/auth/link/google", h.linkGoogle)
	rg.POST("/auth/unlink/google", h.unlinkGoogle)
}

type emailRegisterRequest struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

type emailLoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type googleLoginRequest struct {
	IDToken    string `json:"id_token"`
	Credential string `json:"credential"` // Google One Tap / @react-oauth/google sends "credential"
}

type lineLoginRequest struct {
	IDToken string `json:"id_token"`
}

func (h *Handler) setSessionCookie(c *gin.Context, token string, maxAge int) {
	isHTTPS := c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" || os.Getenv("ENV") == "production"

	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(SessionCookieName, token, maxAge, "/", "", isHTTPS, true)
}

// ──────────────────────────────────────────────
// Email & Password Auth
// ──────────────────────────────────────────────

func (h *Handler) registerWithEmail(c *gin.Context) {
	var req emailRegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ข้อมูลที่ส่งมาไม่ถูกต้อง"})
		return
	}

	email := strings.TrimSpace(strings.ToLower(req.Email))
	if _, err := mail.ParseAddress(email); err != nil || email == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "รูปแบบอีเมลไม่ถูกต้อง"})
		return
	}

	if len(req.Password) < 6 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "รหัสผ่านต้องมีความยาวอย่างน้อย 6 ตัวอักษร"})
		return
	}

	displayName := strings.TrimSpace(req.DisplayName)
	if displayName == "" {
		displayName = strings.Split(email, "@")[0]
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ไม่สามารถประมวลผลรหัสผ่านได้"})
		return
	}

	u, err := h.userRepo.CreateUserWithPassword(c.Request.Context(), email, string(hash), displayName)
	if err != nil {
		if errors.Is(err, user.ErrEmailAlreadyRegistered) {
			c.JSON(http.StatusConflict, gin.H{"error": "อีเมลนี้ถูกลงทะเบียนไว้แล้ว"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "สร้างบัญชีผู้ใช้ล้มเหลว"})
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

	c.JSON(http.StatusCreated, gin.H{
		"user":       u,
		"token":      sessionToken,
		"identities": identities,
	})
}

func (h *Handler) loginWithEmail(c *gin.Context) {
	var req emailLoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ข้อมูลที่ส่งมาไม่ถูกต้อง"})
		return
	}

	email := strings.TrimSpace(strings.ToLower(req.Email))
	if email == "" || req.Password == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "กรุณากรอกอีเมลและรหัสผ่าน"})
		return
	}

	u, err := h.userRepo.FindByEmail(c.Request.Context(), email)
	if err != nil || u == nil || u.PasswordHash == nil || *u.PasswordHash == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "อีเมลหรือรหัสผ่านไม่ถูกต้อง"})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(*u.PasswordHash), []byte(req.Password)); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "อีเมลหรือรหัสผ่านไม่ถูกต้อง"})
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

// ──────────────────────────────────────────────
// Google & LINE Login
// ──────────────────────────────────────────────

func (h *Handler) loginWithGoogle(c *gin.Context) {
	var req googleLoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	token := req.Credential
	if token == "" {
		token = req.IDToken
	}
	if token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing google credential or id_token"})
		return
	}

	sub, email, name, picture, err := VerifyGoogleIDToken(c.Request.Context(), token)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "verify google token failed", "details": err.Error()})
		return
	}

	u, err := h.userRepo.FindOrCreateByProvider(c.Request.Context(), "google", sub, email, name, picture)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to process user account"})
		return
	}

	sessionToken, exp, err := GenerateSessionToken(u.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate session"})
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

func (h *Handler) loginWithLine(c *gin.Context) {
	var req lineLoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if req.IDToken == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing id_token"})
		return
	}

	sub, name, picture, err := VerifyIDToken(c.Request.Context(), req.IDToken, h.liffChannelID)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "verify line token failed", "details": err.Error()})
		return
	}

	u, err := h.userRepo.FindOrCreateByProvider(c.Request.Context(), "line", sub, "", name, picture)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to process user account"})
		return
	}

	// ถ้า login ผ่าน line โดยตรง ให้ใช้รูปโปรไฟล์ของ line เสมอ (ถ้า LINE มี picture)
	if picture != "" {
		_ = h.userRepo.UpdateAvatar(c.Request.Context(), u.ID, picture)
		u.AvatarURL = &picture
	}

	sessionToken, exp, err := GenerateSessionToken(u.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate session"})
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

func (h *Handler) getMe(c *gin.Context) {
	val, ok := c.Get("user_id")
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	userID := val.(string)

	u, err := h.userRepo.FindByID(c.Request.Context(), userID)
	if err != nil || u == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	identities, err := h.userRepo.GetIdentitiesByUserID(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "fetch identities failed"})
		return
	}

	// ถ้าไม่มีรูปโปรไฟล์ ให้ใช้รูปโปรไฟล์ของ LINE (หรือ Google) จาก identity ที่เชื่อมต่อไว้
	if u.AvatarURL == nil || *u.AvatarURL == "" {
		for _, ident := range identities {
			if ident.Provider == "line" && ident.AvatarURL != nil && *ident.AvatarURL != "" {
				u.AvatarURL = ident.AvatarURL
				_ = h.userRepo.UpdateAvatar(c.Request.Context(), u.ID, *ident.AvatarURL)
				break
			}
		}
		if u.AvatarURL == nil || *u.AvatarURL == "" {
			for _, ident := range identities {
				if ident.Provider == "google" && ident.AvatarURL != nil && *ident.AvatarURL != "" {
					u.AvatarURL = ident.AvatarURL
					_ = h.userRepo.UpdateAvatar(c.Request.Context(), u.ID, *ident.AvatarURL)
					break
				}
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"user":       u,
		"identities": identities,
	})
}

// ──────────────────────────────────────────────
// Account Linking & Unlinking (LINE & Google)
// ──────────────────────────────────────────────

func (h *Handler) linkLine(c *gin.Context) {
	val, ok := c.Get("user_id")
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	userID := val.(string)

	var req lineLoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	sub, name, picture, err := VerifyIDToken(c.Request.Context(), req.IDToken, h.liffChannelID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid line token", "details": err.Error()})
		return
	}

	err = h.userRepo.LinkIdentity(c.Request.Context(), userID, "line", sub, "", name, picture)
	if err != nil {
		if errors.Is(err, user.ErrIdentityAlreadyLinked) {
			c.JSON(http.StatusConflict, gin.H{"error": "บัญชี LINE นี้ถูกเชื่อมต่อกับบัญชี JodJod อื่นแล้ว"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "link line failed"})
		return
	}

	// ถ้า user ยังไม่มีรูปโปรไฟล์ ให้ใช้รูปโปรไฟล์ของ LINE ทันที
	if picture != "" {
		currUser, _ := h.userRepo.FindByID(c.Request.Context(), userID)
		if currUser != nil && (currUser.AvatarURL == nil || *currUser.AvatarURL == "") {
			_ = h.userRepo.UpdateAvatar(c.Request.Context(), userID, picture)
		}
	}

	identities, _ := h.userRepo.GetIdentitiesByUserID(c.Request.Context(), userID)
	c.JSON(http.StatusOK, gin.H{
		"success":    true,
		"message":    "เชื่อมต่อ LINE สำเร็จ",
		"identities": identities,
	})
}

func (h *Handler) unlinkLine(c *gin.Context) {
	val, ok := c.Get("user_id")
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	userID := val.(string)

	err := h.userRepo.UnlinkIdentity(c.Request.Context(), userID, "line")
	if err != nil {
		if errors.Is(err, user.ErrCannotUnlinkOnlyMethod) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "ไม่สามารถยกเลิกการเชื่อมต่อช่องทางเข้าสู่ระบบเดียวที่เหลืออยู่ได้"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unlink line failed"})
		return
	}

	identities, _ := h.userRepo.GetIdentitiesByUserID(c.Request.Context(), userID)
	c.JSON(http.StatusOK, gin.H{
		"success":    true,
		"message":    "ยกเลิกการเชื่อมต่อ LINE สำเร็จ",
		"identities": identities,
	})
}

func (h *Handler) linkGoogle(c *gin.Context) {
	val, ok := c.Get("user_id")
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	userID := val.(string)

	var req googleLoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	token := req.Credential
	if token == "" {
		token = req.IDToken
	}
	if token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing google credential"})
		return
	}

	sub, email, name, picture, err := VerifyGoogleIDToken(c.Request.Context(), token)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid google token", "details": err.Error()})
		return
	}

	err = h.userRepo.LinkIdentity(c.Request.Context(), userID, "google", sub, email, name, picture)
	if err != nil {
		if errors.Is(err, user.ErrIdentityAlreadyLinked) {
			c.JSON(http.StatusConflict, gin.H{"error": "บัญชี Google นี้ถูกเชื่อมต่อกับบัญชี JodJod อื่นแล้ว"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "link google failed"})
		return
	}

	identities, _ := h.userRepo.GetIdentitiesByUserID(c.Request.Context(), userID)
	c.JSON(http.StatusOK, gin.H{
		"success":    true,
		"message":    "เชื่อมต่อ Google สำเร็จ",
		"identities": identities,
	})
}

func (h *Handler) unlinkGoogle(c *gin.Context) {
	val, ok := c.Get("user_id")
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	userID := val.(string)

	err := h.userRepo.UnlinkIdentity(c.Request.Context(), userID, "google")
	if err != nil {
		if errors.Is(err, user.ErrCannotUnlinkOnlyMethod) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "ไม่สามารถยกเลิกการเชื่อมต่อช่องทางเข้าสู่ระบบเดียวที่เหลืออยู่ได้"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unlink google failed"})
		return
	}

	identities, _ := h.userRepo.GetIdentitiesByUserID(c.Request.Context(), userID)
	c.JSON(http.StatusOK, gin.H{
		"success":    true,
		"message":    "ยกเลิกการเชื่อมต่อ Google สำเร็จ",
		"identities": identities,
	})
}

func (h *Handler) logout(c *gin.Context) {
	isHTTPS := c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" || os.Getenv("ENV") == "production"
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(SessionCookieName, "", -1, "/", "", isHTTPS, true)
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// ──────────────────────────────────────────────
// Profile & Avatar Management
// ──────────────────────────────────────────────

type updateProfileRequest struct {
	DisplayName string `json:"display_name"`
	AvatarURL   string `json:"avatar_url"`
}

func (h *Handler) uploadAvatar(c *gin.Context) {
	val, ok := c.Get("user_id")
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	userID := val.(string)

	file, header, err := c.Request.FormFile("avatar")
	if err != nil {
		file, header, err = c.Request.FormFile("file")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "กรุณาเลือกไฟล์รูปภาพ (field 'avatar' หรือ 'file')"})
			return
		}
	}
	defer file.Close()

	if header.Size > 5*1024*1024 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ขนาดไฟล์ต้องไม่เกิน 5 MB"})
		return
	}

	ext := strings.ToLower(filepath.Ext(header.Filename))
	if ext != ".jpg" && ext != ".jpeg" && ext != ".png" && ext != ".webp" && ext != ".gif" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "รองรับเฉพาะไฟล์รูปภาพ .jpg, .jpeg, .png, .webp, .gif"})
		return
	}

	uploadDir := "./uploads/avatars"
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "สร้างไดเรกทอรีสำหรับบันทึกไฟล์ไม่สำเร็จ"})
		return
	}

	fileName := fmt.Sprintf("avatar_%s_%d%s", userID, time.Now().UnixNano(), ext)
	dstPath := filepath.Join(uploadDir, fileName)

	out, err := os.Create(dstPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "บันทึกไฟล์รูปภาพไม่สำเร็จ"})
		return
	}
	defer out.Close()

	if _, err := io.Copy(out, file); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "คัดลอกข้อมูลไฟล์ไม่สำเร็จ"})
		return
	}

	avatarURL := fmt.Sprintf("/uploads/avatars/%s", fileName)

	if err := h.userRepo.UpdateAvatar(c.Request.Context(), userID, avatarURL); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "อัปเดตข้อมูลรูปโปรไฟล์ไม่สำเร็จ"})
		return
	}

	updatedUser, _ := h.userRepo.FindByID(c.Request.Context(), userID)

	c.JSON(http.StatusOK, gin.H{
		"success":    true,
		"avatar_url": avatarURL,
		"user":       updatedUser,
	})
}

func (h *Handler) updateProfile(c *gin.Context) {
	val, ok := c.Get("user_id")
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	userID := val.(string)

	var req updateProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ข้อมูลที่ส่งมาไม่ถูกต้อง"})
		return
	}

	updatedUser, err := h.userRepo.UpdateProfile(c.Request.Context(), userID, req.DisplayName, req.AvatarURL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "อัปเดตโปรไฟล์ไม่สำเร็จ"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"user":    updatedUser,
	})
}
