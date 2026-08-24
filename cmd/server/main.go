package main

import (
	"log"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	// "github.com/line/line-bot-sdk-go/v8/linebot"
	"github.com/line/line-bot-sdk-go/v8/linebot/messaging_api"
	"github.com/line/line-bot-sdk-go/v8/linebot/webhook"
	"jodjod/internal/database"
)

var bot *messaging_api.MessagingApiAPI

func main() {
	// โหลด .env (ตอน dev เท่านั้น บน production ใช้ real env vars)
	if err := godotenv.Load(); err != nil {
		log.Println("ไม่พบ .env ไฟล์ ใช้ env vars ของระบบแทน")
	}

	database.Connect()

	channelSecret := os.Getenv("LINE_CHANNEL_SECRET")
	channelToken := os.Getenv("LINE_CHANNEL_ACCESS_TOKEN")

	var err error
	bot, err = messaging_api.NewMessagingApiAPI(channelToken)
	if err != nil {
		log.Fatal("สร้าง bot client ไม่สำเร็จ:", err)
	}

	r := gin.Default()
	r.POST("/webhook", func(c *gin.Context) {
		// ParseRequest ทำ signature verification ให้อัตโนมัติ
		cb, err := webhook.ParseRequest(channelSecret, c.Request)
		if err != nil {
			log.Println("parse/verify ล้มเหลว:", err)
			c.Status(400)
			return
		}

		for _, event := range cb.Events {
			switch e := event.(type) {
			case webhook.MessageEvent:
				switch message := e.Message.(type) {
				case webhook.TextMessageContent:
					log.Println("ได้ข้อความ:", message.Text)
					// TODO: เซฟลง Supabase ตรงนี้
				}
			}
		}

		c.Status(200)
	})

	r.Run()
}