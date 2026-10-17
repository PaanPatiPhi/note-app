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
	"jodjod/internal/note"
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

	webhookHandler := note.NewHandler(noteService, bot, channelSecret, newAssistant(pool, noteRepo))
	noteAPIHandler := note.NewAPIHandler(noteService)
	userAPIHandler := user.NewAPIHandler(userRepo)
	authHandler := auth.NewHandler(userRepo, liffChannelID)
	feedbackRepo := feedback.NewRepository(pool)
	feedbackHandler := feedback.NewHandler(feedbackRepo)
	subscriptionRepo := subscription.NewRepository(pool)
	subscriptionHandler := subscription.NewHandler(subscriptionRepo, userRepo)
	adminHandler := admin.NewHandler(userRepo, feedbackRepo, subscriptionRepo, noteRepo)

	r := gin.Default()

	// ตั้งค่า CORS ให้รองรับทั้ง webOrigin, localhost dev และส่ง Cookie ได้
	allowedOrigins := []string{"http://localhost:3000", "http://localhost:3001", "http://127.0.0.1:3000"}
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

	// Protected REST API — รองรับทั้ง Session Cookie, Bearer JWT, และ LINE ID Token (LIFF)
	protectedAPI := r.Group("/api", auth.RequireAuth(userRepo, liffChannelID))
	authHandler.RegisterProtectedRoutes(protectedAPI)
	noteAPIHandler.RegisterRoutes(protectedAPI)
	userAPIHandler.RegisterRoutes(protectedAPI)

	// Feedback API — เฉพาะผู้ใช้งานที่เข้าสู่ระบบแล้วเท่านั้น
	feedbackHandler.RegisterRoutes(protectedAPI)

	// Subscription & Admin APIs
	subscriptionHandler.RegisterRoutes(protectedAPI)
	adminHandler.RegisterRoutes(protectedAPI)

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
	return pgxpool.NewWithConfig(ctx, config)
}
// newAssistant สร้าง AI ตาม AI_PROVIDER (ollama ค่าเริ่มต้น, claude, หรือ off)
// คืน nil ถ้าปิดหรือตั้งค่าผิด → ทุกข้อความใน LINE ใช้คำสั่งแบบเดิม
func newAssistant(pool *pgxpool.Pool, noteRepo *note.Repository) note.Assistant {
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
	return assistant.New(provider, noteRepo, assistant.NewActionRepository(pool))
}
