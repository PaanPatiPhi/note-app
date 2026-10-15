package user

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

type APIHandler struct {
	repo *Repository
}

func NewAPIHandler(repo *Repository) *APIHandler {
	return &APIHandler{repo: repo}
}

// RegisterRoutes ผูก endpoint เข้ากับ router group ที่ผ่าน auth middleware มาแล้ว
func (h *APIHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/profile", h.getProfile)
}

type profileDTO struct {
	ID      string `json:"id,omitempty"`
	Email   string `json:"email,omitempty"`
	Name    string `json:"name"`
	Picture string `json:"picture,omitempty"`
	Tier    string `json:"tier"`
	Role    string `json:"role"`
}

func (h *APIHandler) getProfile(c *gin.Context) {
	var u *User
	var err error

	// 1. ถ้ามี user_id ใน context (จาก JWT Session หรือ LINE verify)
	if uidVal, ok := c.Get("user_id"); ok {
		if uid, ok := uidVal.(string); ok && uid != "" {
			u, err = h.repo.FindByID(c.Request.Context(), uid)
		}
	}

	// 2. ถ้ายังไม่ได้ ให้ fallback ไปหาจาก line_user_id
	if u == nil {
		lineUserID, _ := c.Get("line_user_id")
		displayName, _ := c.Get("display_name")
		lID, _ := lineUserID.(string)
		dName, _ := displayName.(string)
		if lID != "" {
			u, err = h.repo.FindOrCreateByLineID(c.Request.Context(), lID, dName)
		}
	}

	if err != nil || u == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "profile failed"})
		return
	}

	picture, _ := c.Get("picture")
	pictureStr, _ := picture.(string)
	if pictureStr == "" && u.AvatarURL != nil && *u.AvatarURL != "" {
		pictureStr = *u.AvatarURL
	}
	if pictureStr == "" {
		identities, _ := h.repo.GetIdentitiesByUserID(c.Request.Context(), u.ID)
		for _, ident := range identities {
			if ident.Provider == "line" && ident.AvatarURL != nil && *ident.AvatarURL != "" {
				pictureStr = *ident.AvatarURL
				break
			}
		}
	}

	emailStr := ""
	if u.Email != nil {
		emailStr = *u.Email
	}

	userTier := u.Tier
	if userTier == "" {
		userTier = "free"
	}

	userRole := u.Role
	if userRole == "" {
		userRole = "user"
	}

	c.JSON(http.StatusOK, gin.H{
		"profile": profileDTO{
			ID:      u.ID,
			Email:   emailStr,
			Name:    u.DisplayName,
			Picture: pictureStr,
			Tier:    userTier,
			Role:    userRole,
		},
	})
}
