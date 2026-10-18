package integrations

import (
	"log"
	"net/http"
	"net/url"
	"os"

	"github.com/gin-gonic/gin"
)

// Handler เปิด endpoint ให้เว็บเชื่อม/ยกเลิกการเชื่อมบริการภายนอก
type Handler struct {
	google    *Google // nil = ยังไม่ได้ตั้งค่า GOOGLE_CLIENT_SECRET ฯลฯ
	webOrigin string  // หน้าเว็บที่จะพากลับหลังเชื่อมเสร็จ
}

func NewHandler(google *Google, webOrigin string) *Handler {
	return &Handler{google: google, webOrigin: webOrigin}
}

// oauthCookie เก็บ nonce ของการเชื่อม Google ไว้ใน browser ที่กดเชื่อม (อ่านได้เฉพาะ callback)
const (
	oauthCookie     = "jodjod_gcal_oauth"
	oauthCookiePath = "/api/integrations/google/callback"
)

func setOAuthCookie(c *gin.Context, value string, maxAge int) {
	// production: เว็บกับ API อยู่คนละโดเมน cookie ที่ตั้งจาก fetch ต้องเป็น SameSite=None; Secure
	if os.Getenv("ENV") == "production" {
		c.SetSameSite(http.SameSiteNoneMode)
		c.SetCookie(oauthCookie, value, maxAge, oauthCookiePath, "", true, true)
		return
	}
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(oauthCookie, value, maxAge, oauthCookiePath, "", c.Request.TLS != nil, true)
}

// RegisterProtectedRoutes ต้องอยู่หลัง auth middleware
func (h *Handler) RegisterProtectedRoutes(rg *gin.RouterGroup) {
	rg.GET("/integrations", h.status)
	rg.POST("/integrations/google/connect", h.connectGoogle)
	rg.DELETE("/integrations/google", h.disconnectGoogle)
}

// RegisterPublicRoutes: Google redirect กลับมาที่นี่ (ไม่มี token ของเรา ใช้ state ที่เซ็นไว้ระบุ user)
func (h *Handler) RegisterPublicRoutes(rg *gin.RouterGroup) {
	rg.GET("/integrations/google/callback", h.googleCallback)
}

// status: GET /api/integrations
func (h *Handler) status(c *gin.Context) {
	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	google := gin.H{"available": h.google != nil, "connected": false}
	if h.google != nil {
		connected, email, err := h.google.Status(c.Request.Context(), userID)
		if err != nil {
			log.Println("integration status ล้มเหลว:", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "status failed"})
			return
		}
		google["connected"] = connected
		if email != "" {
			google["email"] = email
		}
	}
	c.JSON(http.StatusOK, gin.H{"google_calendar": google})
}

// connectGoogle: POST /api/integrations/google/connect → {"url": "<หน้าขอสิทธิ์ของ Google>"}
// เว็บพาผู้ใช้ไปที่ url นั้น แล้ว Google จะ redirect กลับมาที่ callback
func (h *Handler) connectGoogle(c *gin.Context) {
	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	if h.google == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "google calendar is not configured on the server"})
		return
	}
	u, nonce, err := h.google.AuthURL(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cannot start google sign-in"})
		return
	}
	setOAuthCookie(c, nonce, int(stateTTL.Seconds()))
	c.JSON(http.StatusOK, gin.H{"url": u})
}

// disconnectGoogle: DELETE /api/integrations/google
func (h *Handler) disconnectGoogle(c *gin.Context) {
	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	if h.google == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "google calendar is not configured on the server"})
		return
	}
	if err := h.google.Disconnect(c.Request.Context(), userID); err != nil {
		log.Println("disconnect google ล้มเหลว:", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "disconnect failed"})
		return
	}
	c.Status(http.StatusNoContent)
}

// googleCallback: GET /api/integrations/google/callback?code=&state= (Google เรียกมา)
// เสร็จแล้วพากลับไปหน้า account ของเว็บพร้อมผลลัพธ์ใน query
func (h *Handler) googleCallback(c *gin.Context) {
	back := func(result string) {
		c.Redirect(http.StatusFound, h.webOrigin+"/account?"+url.Values{"google_calendar": {result}}.Encode())
	}
	if h.google == nil {
		back("unavailable")
		return
	}
	nonce, _ := c.Cookie(oauthCookie)
	setOAuthCookie(c, "", -1)           // ใช้ได้ครั้งเดียว
	if e := c.Query("error"); e != "" { // ผู้ใช้กดไม่อนุญาต
		back("denied")
		return
	}
	if _, err := h.google.HandleCallback(c.Request.Context(), c.Query("code"), c.Query("state"), nonce); err != nil {
		log.Println("google calendar callback ล้มเหลว:", err)
		back("error")
		return
	}
	back("connected")
}
