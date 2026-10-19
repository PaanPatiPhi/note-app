package assistant

import (
	"fmt"
	"strings"
	"time"
)

// pinQuestionTopics / pinQuestionWords ใช้จับคำถามว่า "ปักอะไรไว้บ้าง"
// โมเดลเล็ก (qwen3:4b) มักตอบคำถามนี้โดยไม่เรียก list_pinned หรือแต่งคำตอบจากชื่อลิสต์ใน <context>
// คำถามนี้อ่านอย่างเดียวและคำตอบตายตัว จึงตอบจาก DB ตรงๆ ไม่ผ่านโมเดล (เร็วกว่าด้วย)
var (
	pinQuestionTopics = []string{"ปัก", "หมุด", "widget", "วิดเจ็ต", "วิดเจต"}
	pinQuestionWords  = []string{"อะไร", "บ้าง"}
	// คำสั่งเปลี่ยนสถานะ เช่น "เอาอะไรออกจาก widget บ้าง" ให้โมเดลจัดการตามปกติ
	pinCommandWords = []string{"เลิก", "ถอด", "เอาออก", "ออกจาก", "ลบ"}
)

const maxPinnedReplyLines = 10

// isPinnedQuestion บอกว่าข้อความถามว่าตอนนี้ปักอะไรไว้บน widget
func isPinnedQuestion(text string) bool {
	t := strings.ToLower(strings.Join(strings.Fields(text), ""))
	return containsAny(t, pinQuestionTopics) && containsAny(t, pinQuestionWords) && !containsAny(t, pinCommandWords)
}

func containsAny(s string, words []string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// pinnedReply จัดรูปคำตอบรายการที่ปักไว้ จากผลของ list_pinned
func pinnedReply(views []noteView) string {
	if len(views) == 0 {
		return "📌 ยังไม่ได้ปักอะไรไว้ครับ พิมพ์บอกได้เลย เช่น \"ปักลิสต์ซื้อของไว้\""
	}
	lines := []string{fmt.Sprintf("📌 ตอนนี้ปักไว้ %d รายการครับ", len(views))}
	for i, v := range views {
		if i == maxPinnedReplyLines {
			lines = append(lines, fmt.Sprintf("และอีก %d รายการ", len(views)-i))
			break
		}
		lines = append(lines, "- "+pinnedLine(v))
	}
	return strings.Join(lines, "\n")
}

func pinnedLine(v noteView) string {
	switch {
	case v.Type == "reminder" && v.RemindAt != "":
		if at, err := time.Parse(time.RFC3339, v.RemindAt); err == nil {
			return fmt.Sprintf("⏰ %s (%s)", v.Content, thaiDateTime(at))
		}
	case v.Type == "checklist":
		var left []string
		for _, it := range v.Items {
			if text, ok := strings.CutPrefix(it, "☐ "); ok {
				left = append(left, text)
			}
		}
		if len(v.Items) > 0 && len(left) == 0 {
			return v.Content + " (ครบแล้ว)"
		}
		if len(left) > 0 {
			return fmt.Sprintf("%s (เหลือ %s)", v.Content, strings.Join(left, ", "))
		}
	}
	return v.Content
}
