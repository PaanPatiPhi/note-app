package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/line/line-bot-sdk-go/v8/linebot/messaging_api"

	"jodjod/internal/admin"
	"jodjod/internal/assistant"
	"jodjod/internal/auth"
	"jodjod/internal/feedback"
	"jodjod/internal/integrations"
	"jodjod/internal/note"
	"jodjod/internal/ratelimit"
	"jodjod/internal/subscription"
	"jodjod/internal/user"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("ไม่พบ .env ไฟล์ ใช้ env vars ของระบบแทน")
	}

	channelSecret := os.Getenv("LINE_CHANNEL_SECRET")
	channelToken := os.Getenv("LINE_CHANNEL_ACCESS_TOKEN")
	databaseURL := os.Getenv("DATABASE_URL")

	// liffChannelID คือ Channel ID ของ LINE Login channel
	liffChannelID := os.Getenv("LIFF_CHANNEL_ID")

	// webOrigin คือ URL ของเว็บแอพ Next.js เช่น http://localhost:3000 ตอน dev
	webOrigin := os.Getenv("WEB_ORIGIN")

	if err := auth.CheckJWTConfig(); err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()

	pool, err := newPool(ctx, databaseURL)
	if err != nil {
		log.Fatal("เชื่อมต่อ database ไม่สำเร็จ:", err)
	}
	defer pool.Close()

	bot, err := messaging_api.NewMessagingApiAPI(channelToken)
	if err != nil {
		log.Fatal("สร้าง bot client ไม่สำเร็จ:", err)
	}

	userRepo := user.NewRepository(pool)
	noteRepo := note.NewRepository(pool)
	noteService := note.NewService(noteRepo, userRepo)

	ai := newAssistant(pool, noteRepo)
	googleCal := newGoogleCalendar(pool)
	if ai != nil && googleCal != nil {
		ai.WithCalendar(googleCal)
	}
	integrationsHandler := integrations.NewHandler(googleCal, firstOrigin(webOrigin))
	// ส่ง *Assistant ที่เป็น nil เข้า interface ตรงๆ ไม่ได้ (จะได้ interface ที่ไม่ nil)
	var lineAI note.Assistant
	if ai != nil {
		lineAI = ai
	}
	webhookHandler := note.NewHandler(noteService, bot, channelSecret, lineAI)
	assistantAPIHandler := assistant.NewAPIHandler(ai)
	noteAPIHandler := note.NewAPIHandler(noteService)
	userAPIHandler := user.NewAPIHandler(userRepo)
	authHandler := auth.NewHandler(userRepo, liffChannelID)
	feedbackRepo := feedback.NewRepository(pool)
	feedbackHandler := feedback.NewHandler(feedbackRepo)
	subscriptionRepo := subscription.NewRepository(pool)
	subscriptionHandler := subscription.NewHandler(subscriptionRepo, userRepo)
	adminHandler := admin.NewHandler(userRepo, feedbackRepo, subscriptionRepo, noteRepo)

	r := gin.Default()
	production := os.Getenv("ENV") == "production"

	// ค่าเริ่มต้นของ gin เชื่อ X-Forwarded-For จากทุกคน → ใครก็ปลอม IP หลบ rate limit ได้
	// ตั้ง TRUSTED_PROXIES (คั่นด้วย comma) เฉพาะตอนอยู่หลัง proxy/load balancer ที่รู้จัก
	if err := r.SetTrustedProxies(splitList(os.Getenv("TRUSTED_PROXIES"))); err != nil {
		log.Fatal("TRUSTED_PROXIES ไม่ถูกต้อง:", err)
	}
	r.MaxMultipartMemory = 6 << 20
	r.Use(securityHeaders(production))
	r.Use(ratelimit.BodyLimit(1<<20, map[string]int64{"/api/auth/avatar": 6 << 20}))
	r.Use(ratelimit.New(map[string]ratelimit.Rule{
		"POST /api/auth/login":    {Limit: 10, Window: time.Minute},
		"POST /api/auth/register": {Limit: 5, Window: time.Minute},
		"POST /api/auth/google":   {Limit: 20, Window: time.Minute},
		"POST /api/auth/line":     {Limit: 20, Window: time.Minute},
	}).ByIP())
	userLimits := ratelimit.New(map[string]ratelimit.Rule{
		"POST /api/assistant/messages":          {Limit: 20, Window: time.Minute},
		"POST /api/feedback":                    {Limit: 5, Window: time.Minute},
		"POST /api/auth/avatar":                 {Limit: 10, Window: time.Minute},
		"POST /api/integrations/google/connect": {Limit: 10, Window: time.Minute},
		"POST /api/assistant/actions/:id/undo":  {Limit: 30, Window: time.Minute},
	}).ByUser()

	// ตั้งค่า CORS: localhost ใช้ได้เฉพาะตอน dev, production อนุญาตเฉพาะ WEB_ORIGIN
	var allowedOrigins []string
	if !production {
		allowedOrigins = []string{"http://localhost:3000", "http://localhost:3001", "http://127.0.0.1:3000"}
	}
	if webOrigin != "" {
		for _, o := range strings.Split(webOrigin, ",") {
			o = strings.TrimSpace(o)
			if o != "" {
				allowedOrigins = append(allowedOrigins, o)
			}
		}
	}

	r.Use(cors.New(cors.Config{
		AllowOrigins:     allowedOrigins,
		AllowMethods:     []string{"GET", "POST", "DELETE", "PATCH", "OPTIONS"},
		AllowHeaders:     []string{"Authorization", "Content-Type", "Cookie", "Accept", "Origin"},
		ExposeHeaders:    []string{"Set-Cookie"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	// Serve static uploads (เช่น รูปโปรไฟล์ avatar)
	_ = os.MkdirAll("./uploads/avatars", 0755)
	r.Static("/uploads", "./uploads")

	// LINE webhook — ปลอดภัยด้วย signature verification ข้างใน handler เอง
	r.POST("/webhook", webhookHandler.HandleWebhook)

	// Public Auth Endpoints (ล็อกอินไม่ต้องมี token ก่อน)
	publicAPI := r.Group("/api")
	authHandler.RegisterPublicRoutes(publicAPI)
	integrationsHandler.RegisterPublicRoutes(publicAPI)

	// Protected REST API — รองรับทั้ง Session Cookie, Bearer JWT, และ LINE ID Token (LIFF)
	protectedAPI := r.Group("/api", auth.RequireAuth(userRepo, liffChannelID), userLimits)
	authHandler.RegisterProtectedRoutes(protectedAPI)
	noteAPIHandler.RegisterRoutes(protectedAPI)
	userAPIHandler.RegisterRoutes(protectedAPI)

	// Feedback API — เฉพาะผู้ใช้งานที่เข้าสู่ระบบแล้วเท่านั้น
	feedbackHandler.RegisterRoutes(protectedAPI)

	// Subscription & Admin APIs
	subscriptionHandler.RegisterRoutes(protectedAPI)
	adminHandler.RegisterRoutes(protectedAPI)

	// AI assistant สำหรับเว็บ/แอป (ไม่ผ่าน LINE)
	assistantAPIHandler.RegisterRoutes(protectedAPI)
	integrationsHandler.RegisterProtectedRoutes(protectedAPI)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	srv := &http.Server{Addr: ":" + port, Handler: r}

	go func() {
		log.Println("server listening on", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal("server ล้มเหลว:", err)
		}
	}()

	// Graceful shutdown: หยุดรับ request ใหม่ก่อน แล้วรอ webhook event ที่ค้างอยู่ให้เสร็จ
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-sigCtx.Done()
	log.Println("กำลังปิด server...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Println("shutdown http server ล้มเหลว:", err)
	}
	if err := webhookHandler.Wait(shutdownCtx); err != nil {
		log.Println("รอ webhook event ค้างไม่ทัน:", err)
	}
}

// newPool สร้าง pgx pool ที่ปิด prepared statement caching (ใช้ simple protocol แทน)
// จำเป็นเมื่อต่อผ่าน Supabase Transaction Pooler (PgBouncer โหมด transaction)
func newPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	// DB อยู่ไกล (Supabase Sydney) การเปิด connection ใหม่ใช้เวลาเป็นวินาที
	// เก็บ connection เปิดไว้อย่างน้อย 2 อัน และเปิดตั้งแต่ start ข้อความแรกจะได้ไม่ต้องรอ
	config.MinConns = 2
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// newAssistant สร้าง AI ตาม AI_PROVIDER (ollama ค่าเริ่มต้น, claude, หรือ off)
// คืน nil ถ้าปิดหรือตั้งค่าผิด → ทุกข้อความใน LINE ใช้คำสั่งแบบเดิม
func newAssistant(pool *pgxpool.Pool, noteRepo *note.Repository) *assistant.Assistant {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("AI_PROVIDER")), "off") {
		log.Println("AI assistant: ปิด (AI_PROVIDER=off)")
		return nil
	}
	provider, err := assistant.ProviderFromEnv(os.Getenv)
	if err != nil {
		log.Println("AI assistant: ปิด เพราะตั้งค่าไม่ถูกต้อง:", err)
		return nil
	}
	log.Println("AI assistant:", provider.Name())
	ai := assistant.New(provider, noteRepo, assistant.NewActionRepository(pool))

	// โหลดโมเดลเข้า GPU ตั้งแต่ start ข้อความแรกจะได้ไม่ต้องรอโหลด
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		start := time.Now()
		if err := ai.Warmup(ctx); err != nil {
			log.Println("AI warmup ล้มเหลว (จะลองโหลดตอนมีข้อความแรก):", err)
			return
		}
		log.Printf("AI warmup เสร็จใน %s", time.Since(start).Round(time.Millisecond))
	}()
	return ai
}

// newGoogleCalendar เปิดการเชื่อม Google Calendar เมื่อตั้งค่าครบ ไม่งั้นคืน nil (ปุ่มเชื่อมบนเว็บจะบอกว่ายังไม่พร้อม)
// ต้องมี GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET และ INTEGRATIONS_KEY (สร้างด้วย `openssl rand -base64 32`)
func newGoogleCalendar(pool *pgxpool.Pool) *integrations.Google {
	clientID := strings.TrimSpace(os.Getenv("GOOGLE_CLIENT_ID"))
	secret := strings.TrimSpace(os.Getenv("GOOGLE_CLIENT_SECRET"))
	key := strings.TrimSpace(os.Getenv("INTEGRATIONS_KEY"))
	var missing []string
	for name, v := range map[string]string{"GOOGLE_CLIENT_ID": clientID, "GOOGLE_CLIENT_SECRET": secret, "INTEGRATIONS_KEY": key} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		log.Println("Google Calendar: ปิด (ยังไม่ได้ตั้ง", strings.Join(missing, ", ")+")")
		return nil
	}
	sealer, err := integrations.NewSealer(key)
	if err != nil {
		log.Println("Google Calendar: ปิด เพราะ", err)
		return nil
	}
	redirect := strings.TrimSpace(os.Getenv("GOOGLE_REDIRECT_URL"))
	if redirect == "" {
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080"
		}
		redirect = "http://localhost:" + port + "/api/integrations/google/callback"
	}
	log.Println("Google Calendar: เปิด (redirect", redirect+")")
	return integrations.NewGoogle(integrations.GoogleConfig{ClientID: clientID, ClientSecret: secret, RedirectURL: redirect},
		integrations.NewRepository(pool, sealer), sealer)
}

// securityHeaders ใส่ header พื้นฐานกันการเดาชนิดไฟล์ (เช่น รูปที่อัปโหลดถูกเปิดเป็น HTML) และกันฝังหน้าใน iframe
func securityHeaders(production bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; frame-ancestors 'none'")
		if production {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		c.Next()
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// firstOrigin คืน origin แรกของ WEB_ORIGIN (คั่นด้วย comma) ใช้เป็นหน้าที่พากลับหลังเชื่อมบริการ
func firstOrigin(webOrigin string) string {
	for _, o := range strings.Split(webOrigin, ",") {
		if o = strings.TrimSpace(o); o != "" {
			return strings.TrimRight(o, "/")
		}
	}
	return "http://localhost:3000"
}
