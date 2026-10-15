package auth

import (
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"jodjod/internal/user"
)

const SessionCookieName = "jodjod_session"

// RequireAuth คือ Unified middleware สำหรับตรวจสอบสิทธิ์
// รองรับทั้ง:
// 1. Session Cookie "jodjod_session" (สำหรับ Web App บน Browser)
// 2. Authorization: Bearer <jodjod_jwt> (สำหรับ Web/Mobile API)
// 3. Authorization: Bearer <line_id_token> (สำหรับ LINE LIFF เดิม — backward compatible 100%)
func RequireAuth(userRepo *user.Repository, liffChannelID string) gin.HandlerFunc {
	return func(c *gin.Context) {
		var token string

		// 1. ลองหาจาก Session Cookie ก่อน
		if cookie, err := c.Cookie(SessionCookieName); err == nil && cookie != "" {
			token = cookie
		}

		// 2. ถ้าไม่มีใน Cookie ลองหาจาก Header "Authorization: Bearer <token>"
		if token == "" {
			header := c.GetHeader("Authorization")
			if strings.HasPrefix(header, "Bearer ") {
				token = strings.TrimPrefix(header, "Bearer ")
			}
		}

		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing authentication token"})
			return
		}

		// 3. ลอง verify เป็น JodJod JWT Session Token
		claims, err := ValidateSessionToken(token)
		if err == nil && claims.UserID != "" {
			c.Set("user_id", claims.UserID)
			c.Next()
			return
		}

		// 4. ถ้าไม่ใช่ JodJod JWT ให้ลอง verify เป็น LINE ID Token (Backward compatibility สำหรับ LIFF)
		if liffChannelID != "" {
			lineUserID, displayName, picture, err := VerifyIDToken(c.Request.Context(), token, liffChannelID)
			if err == nil && lineUserID != "" {
				u, err := userRepo.FindOrCreateByLineID(c.Request.Context(), lineUserID, displayName)
				if err == nil && u != nil {
					c.Set("user_id", u.ID)
					c.Set("line_user_id", lineUserID)
					c.Set("display_name", displayName)
					c.Set("picture", picture)
					c.Next()
					return
				}
				log.Printf("find or create user from LINE token failed: %v", err)
			}
		}

		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
	}
}

// OptionalAuth พยายามดึง user_id จาก Cookie หรือ Header ถ้ามี แต่ถ้าไม่มีก็ปล่อยให้ผ่านได้ (สำหรับ Guest)
func OptionalAuth(userRepo *user.Repository, liffChannelID string) gin.HandlerFunc {
	return func(c *gin.Context) {
		var token string

		if cookie, err := c.Cookie(SessionCookieName); err == nil && cookie != "" {
			token = cookie
		}
		if token == "" {
			header := c.GetHeader("Authorization")
			if strings.HasPrefix(header, "Bearer ") {
				token = strings.TrimPrefix(header, "Bearer ")
			}
		}

		if token != "" {
			if claims, err := ValidateSessionToken(token); err == nil && claims.UserID != "" {
				c.Set("user_id", claims.UserID)
				c.Next()
				return
			}
			if liffChannelID != "" {
				if lineUserID, displayName, picture, err := VerifyIDToken(c.Request.Context(), token, liffChannelID); err == nil && lineUserID != "" {
					if u, err := userRepo.FindOrCreateByLineID(c.Request.Context(), lineUserID, displayName); err == nil && u != nil {
						c.Set("user_id", u.ID)
						c.Set("line_user_id", lineUserID)
						c.Set("display_name", displayName)
						c.Set("picture", picture)
					}
				}
			}
		}

		c.Next()
	}
}
