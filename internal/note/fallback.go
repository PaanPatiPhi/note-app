package note

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ส่วนนี้ทำงานได้โดยไม่ต้องใช้ AI: ใช้ทั้งตอนปิด AI (AI_PROVIDER=off) และตอน AI ล่ม/ช้าเกินไป
// ทั้งใน LINE และหน้าแชทบนเว็บ

// AIDownHint ต่อท้ายคำตอบเมื่อ AI ใช้ไม่ได้แล้วข้อความถูกจดเป็นโน้ตธรรมดาแทน
// ผู้ใช้จะได้รู้ว่ายังไม่ได้ตั้งเตือนหรือลงนัดให้
const AIDownHint = "(ตอนนี้พี่จ๊อดอ่านข้อความไม่ได้ เลยจดไว้ตามที่พิมพ์มาก่อน ถ้าอยากให้ตั้งเตือนหรือลงนัด ลองพิมพ์ใหม่อีกครั้งภายหลังครับ)"

// ── คำถาม "ปักอะไรไว้บ้าง" ────────────────────────────────────

// คำถามนี้อ่านอย่างเดียวและคำตอบตายตัว จึงตอบจาก DB ตรงๆ ไม่ผ่านโมเดลเสมอ
// (โมเดลเล็กอย่าง qwen3:4b มักตอบโดยไม่เรียก tool หรือแต่งคำตอบจากชื่อลิสต์)
var (
	pinQuestionTopics = []string{"ปัก", "หมุด", "widget", "วิดเจ็ต", "วิดเจต"}
	pinQuestionWords  = []string{"อะไร", "บ้าง"}
	// คำสั่งเปลี่ยนสถานะ เช่น "เอาอะไรออกจาก widget บ้าง" ไม่ใช่คำถาม
	pinCommandWords = []string{"เลิก", "ถอด", "เอาออก", "ออกจาก", "ลบ"}
)

const maxPinnedReplyLines = 10

// IsPinnedQuestion บอกว่าข้อความถามว่าตอนนี้ปักอะไรไว้บน widget
func IsPinnedQuestion(text string) bool {
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

// FormatPinned จัดรูปคำตอบรายการที่ปักไว้ (ลิสต์บอกของที่ยังไม่ติ๊ก, เตือนบอกเวลา)
func FormatPinned(notes []*Note, items map[string][]*NoteItem) string {
	if len(notes) == 0 {
		return "📌 ยังไม่ได้ปักอะไรไว้ครับ พิมพ์บอกได้เลย เช่น \"ปักลิสต์ซื้อของไว้\""
	}
	lines := []string{fmt.Sprintf("📌 ตอนนี้ปักไว้ %d รายการครับ", len(notes))}
	for i, n := range notes {
		if i == maxPinnedReplyLines {
			lines = append(lines, fmt.Sprintf("และอีก %d รายการ", len(notes)-i))
			break
		}
		lines = append(lines, "- "+pinnedLine(n, items[n.ID]))
	}
	return strings.Join(lines, "\n")
}

func pinnedLine(n *Note, items []*NoteItem) string {
	content := contentOf(n)
	switch {
	case n.Type == NoteTypeReminder && n.RemindAt != nil:
		return fmt.Sprintf("⏰ %s (%s)", content, ThaiDateTime(*n.RemindAt))
	case n.Type == NoteTypeChecklist && len(items) > 0:
		var left []string
		for _, it := range items {
			if !it.IsDone {
				left = append(left, it.Text)
			}
		}
		if len(left) == 0 {
			return content + " (ครบแล้ว)"
		}
		return fmt.Sprintf("%s (เหลือ %s)", content, strings.Join(left, ", "))
	}
	return content
}

func contentOf(n *Note) string {
	if n.Content == nil {
		return ""
	}
	return *n.Content
}

var (
	thaiShortWeekdays = [...]string{"อา.", "จ.", "อ.", "พ.", "พฤ.", "ศ.", "ส."}
	thaiShortMonths   = [...]string{"ม.ค.", "ก.พ.", "มี.ค.", "เม.ย.", "พ.ค.", "มิ.ย.", "ก.ค.", "ส.ค.", "ก.ย.", "ต.ค.", "พ.ย.", "ธ.ค."}
)

// ThaiDateTime จัดรูปเวลาแบบที่ใช้ตอบใน LINE เช่น "อา. 18 ต.ค. 09:00" (เวลาไทย)
func ThaiDateTime(t time.Time) string {
	t = t.In(bangkok)
	return fmt.Sprintf("%s %d %s %s", thaiShortWeekdays[t.Weekday()], t.Day(), thaiShortMonths[t.Month()-1], t.Format("15:04"))
}

// ── คำสั่งปัก/ถอดหมุดแบบไม่ใช้ AI ──────────────────────────────

var (
	unpinPrefixes = []string{"เลิกปักหมุด", "เลิกปัก", "ถอดหมุด"}
	pinPrefixes   = []string{"ปักหมุด", "ปัก"}
	// คำที่ไม่ใช่ชื่อโน้ต ตัดออกจากหัว/ท้ายก่อนค้นหา เช่น "ปักลิสต์ซื้อของไว้หน่อย" → "ซื้อของ"
	pinLeadingFillers  = []string{"ลิสต์", "ลิส", "โน้ต", "เตือน", "อัน"}
	pinTrailingFillers = []string{
		"ไว้หน้าจอ", "หน้าจอ", "ไว้บน widget", "บน widget", "ออกจาก widget", "จาก widget", "widget",
		"ไว้ด้วย", "ไว้หน่อย", "หน่อย", "ด้วย", "ไว้", "ออก", "ครับ", "ค่ะ", "คะ", "นะ",
	}
)

// parsePinCommand แยก "ปัก <ชื่อ>" / "เลิกปัก <ชื่อ>" เป็นคำค้นกับสถานะที่ต้องการ
func parsePinCommand(text string) (query string, pinned bool, ok bool) {
	t := strings.TrimSpace(text)
	rest, matched := "", false
	for _, p := range unpinPrefixes {
		if strings.HasPrefix(t, p) {
			rest, pinned, matched = strings.TrimPrefix(t, p), false, true
			break
		}
	}
	if !matched {
		for _, p := range pinPrefixes {
			if strings.HasPrefix(t, p) {
				rest, pinned, matched = strings.TrimPrefix(t, p), true, true
				break
			}
		}
	}
	if !matched {
		return "", false, false
	}
	query = trimFillers(strings.TrimSpace(rest))
	return query, pinned, query != ""
}

func trimFillers(s string) string {
	for changed := true; changed; {
		changed = false
		for _, f := range pinLeadingFillers {
			if rest := strings.TrimSpace(strings.TrimPrefix(s, f)); rest != s {
				s, changed = rest, true
			}
		}
		for _, f := range pinTrailingFillers {
			if rest := strings.TrimSpace(strings.TrimSuffix(s, f)); rest != s {
				s, changed = rest, true
			}
		}
	}
	return s
}

// PickPinTarget เลือกโน้ตที่แน่ใจจากผลค้นหา (เรียงใหม่ → เก่า):
// เจอตัวเดียว, ชื่อตรงเป๊ะตัวเดียว, หรือมีตัวเดียวที่ยังไม่อยู่ในสถานะที่ต้องการ
// (ตอนปักจะข้ามโน้ตที่ทำเสร็จแล้ว) — ใช้ร่วมกับ tool pin_notes ของ AI
func PickPinTarget(matches []*Note, query string, pinned bool) (*Note, bool) {
	if len(matches) == 0 {
		return nil, false
	}
	if len(matches) == 1 {
		return matches[0], true
	}
	var exact []*Note
	for _, m := range matches {
		if strings.EqualFold(strings.TrimSpace(contentOf(m)), query) {
			exact = append(exact, m)
		}
	}
	if len(exact) == 1 {
		return exact[0], true
	}
	pool := matches
	if len(exact) > 1 {
		pool = exact
	}
	var pending []*Note
	for _, m := range pool {
		if (m.PinnedAt != nil) != pinned && !(pinned && m.IsDone) {
			pending = append(pending, m)
		}
	}
	if len(pending) == 1 {
		return pending[0], true
	}
	return nil, false
}

// pinByQuery ปัก/ถอดหมุดโน้ตจากชื่อ; ok=false = หาไม่เจอเลย (ให้ผู้เรียกตัดสินใจต่อ)
func (s *Service) pinByQuery(ctx context.Context, userID, query string, pinned bool) (*CommandResult, bool, error) {
	matches, err := s.noteRepo.SearchByUser(ctx, userID, query)
	if err != nil {
		return nil, false, err
	}
	if len(matches) == 0 {
		return nil, false, nil
	}
	target, ok := PickPinTarget(matches, query, pinned)
	if !ok {
		if len(matches) > fuzzyMaxChoices {
			matches = matches[:fuzzyMaxChoices]
		}
		return &CommandResult{Action: "pin_ambiguous", Notes: matches, Query: query}, true, nil
	}
	if (target.PinnedAt != nil) != pinned {
		if err := s.noteRepo.SetPinned(ctx, userID, target.ID, pinned); err != nil {
			return nil, false, err
		}
	}
	action := "pinned"
	if !pinned {
		action = "unpinned"
	}
	return &CommandResult{Action: action, Note: target}, true, nil
}

func (s *Service) listPinned(ctx context.Context, userID string) (*CommandResult, error) {
	notes, err := s.noteRepo.FindFeed(ctx, FeedQuery{UserID: userID, Desc: true, Limit: recentFeedLimit, PinnedOnly: true})
	if err != nil {
		return nil, fmt.Errorf("list pinned: %w", err)
	}
	return &CommandResult{Action: "pinned_list", Notes: notes, NoteItems: s.attachItems(ctx, notes)}, nil
}

// ── ใช้จากหน้าแชทบนเว็บตอน AI ใช้ไม่ได้ ───────────────────────

// CaptureForUser คือ CaptureFromLine สำหรับ user ที่รู้ id แล้ว (หน้าแชทบนเว็บ)
func (s *Service) CaptureForUser(ctx context.Context, userID, text string) (*CommandResult, error) {
	return s.captureFor(ctx, userID, text, NoteSourceWeb)
}

// ReplyText คือข้อความตอบกลับของผลคำสั่ง แบบข้อความล้วน (เว็บไม่มีปุ่ม quick reply แบบ LINE)
func ReplyText(result *CommandResult) string {
	if result.Action == "item_choice" {
		names := make([]string, 0, len(result.ItemMatches))
		for _, m := range result.ItemMatches {
			names = append(names, m.Item.Text)
		}
		return fmt.Sprintf("เจอหลายรายการที่คล้ายกันครับ: %s\nพิมพ์ชื่อให้ตรงขึ้นอีกนิดนะครับ", strings.Join(names, ", "))
	}
	return replyTextFor(result)
}

// Wrote บอกว่าคำสั่งนี้สร้างหรือแก้ข้อมูล (เว็บจะได้โหลดรายการใหม่)
func (r *CommandResult) Wrote() bool {
	switch r.Action {
	case "created", "deleted", "updated", "item_toggled", "pinned", "unpinned":
		return true
	}
	return false
}
