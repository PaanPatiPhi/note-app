package assistant

import (
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// อ่านเวลาแบบไทย ("สองทุ่ม", "บ่ายสาม", "9 โมงครึ่ง", "ตีห้า", "เที่ยง", "14:30") ในโค้ด
// เพราะโมเดลเล็กแปลผิดได้ (เช่น "สองทุ่ม" → 22:00) — ใช้แก้ชั่วโมง/นาทีของ reminder ให้ตรงกับที่ผู้ใช้พิมพ์

// ตัวเลขไทยแบบคำ เรียงคำยาวก่อน ("สิบสอง" ต้องมาก่อน "สิบ" และ "สอง")
const thaiNum = `(สิบสอง|สิบเอ็ด|สิบ|หนึ่ง|นึ่ง|นึง|สอง|สาม|สี่|ห้า|หก|เจ็ด|แปด|เก้า|\d{1,2})`

var thaiNumWords = map[string]int{
	"หนึ่ง": 1, "นึ่ง": 1, "นึง": 1, "สอง": 2, "สาม": 3, "สี่": 4, "ห้า": 5, "หก": 6,
	"เจ็ด": 7, "แปด": 8, "เก้า": 9, "สิบ": 10, "สิบเอ็ด": 11, "สิบสอง": 12,
}

func thaiNumber(s string) (int, bool) {
	if n, ok := thaiNumWords[s]; ok {
		return n, true
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

type clockRule struct {
	re *regexp.Regexp
	// คืนชั่วโมง/นาที จาก submatch; ok=false ถ้าค่าไม่สมเหตุผล
	hm func(m []string) (h, min int, ok bool)
}

// กฎเรียงจากเฉพาะเจาะจงที่สุดไปกว้างที่สุด; ข้อความถูกตัดช่องว่างออกก่อน ("9 โมง" → "9โมง")
// กลุ่มสุดท้ายของทุกกฎคือ "ครึ่ง" (= +30 นาที) ถ้ามี
var clockRules = []clockRule{
	// 14:30, 9.15น., 20:00น.
	{regexp.MustCompile(`(\d{1,2})[:.](\d{2})(?:น\.?)?()`), func(m []string) (int, int, bool) {
		h, _ := strconv.Atoi(m[1])
		mi, _ := strconv.Atoi(m[2])
		return h, mi, h <= 23 && mi <= 59
	}},
	{regexp.MustCompile(`เที่ยงคืน(ครึ่ง)?`), func(m []string) (int, int, bool) { return 0, 0, true }},
	{regexp.MustCompile(`เที่ยง(?:วัน|ตรง)?(ครึ่ง)?`), func(m []string) (int, int, bool) { return 12, 0, true }},
	// ตีหนึ่ง..ตีห้า
	{regexp.MustCompile(`ตี` + thaiNum + `(ครึ่ง)?`), func(m []string) (int, int, bool) {
		n, ok := thaiNumber(m[1])
		return n, 0, ok && n >= 1 && n <= 5
	}},
	// ทุ่มนึง / ทุ่มหนึ่ง = 19:00
	{regexp.MustCompile(`ทุ่ม(?:นึง|หนึ่ง|นึ่ง)(ครึ่ง)?`), func(m []string) (int, int, bool) { return 19, 0, true }},
	// หนึ่งทุ่ม..ห้าทุ่ม = 19..23
	{regexp.MustCompile(thaiNum + `ทุ่ม(ครึ่ง)?`), func(m []string) (int, int, bool) {
		n, ok := thaiNumber(m[1])
		return 18 + n, 0, ok && n >= 1 && n <= 5
	}},
	// บ่ายโมง = 13:00
	{regexp.MustCompile(`บ่ายโมง(ครึ่ง)?`), func(m []string) (int, int, bool) { return 13, 0, true }},
	// บ่ายสอง(โมง) .. บ่ายห้า(โมง) = 14..17
	{regexp.MustCompile(`บ่าย` + thaiNum + `(?:โมง)?(ครึ่ง)?`), func(m []string) (int, int, bool) {
		n, ok := thaiNumber(m[1])
		return 12 + n, 0, ok && n >= 1 && n <= 5
	}},
	// สี่โมงเย็น..หกโมงเย็น = 16..18
	{regexp.MustCompile(thaiNum + `โมง(?:เย็น|ค่ำ)(ครึ่ง)?`), func(m []string) (int, int, bool) {
		n, ok := thaiNumber(m[1])
		if ok && n >= 4 && n <= 6 {
			return 12 + n, 0, true
		}
		return 0, 0, false
	}},
	// หกโมงเช้า..สิบเอ็ดโมงเช้า, 9 โมง (ไม่มีคำบอกช่วง = ช่วงเช้า 6..12)
	{regexp.MustCompile(thaiNum + `โมง(?:เช้า|ตรง)?(ครึ่ง)?`), func(m []string) (int, int, bool) {
		n, ok := thaiNumber(m[1])
		return n, 0, ok && n >= 6 && n <= 12
	}},
	// 20 นาฬิกา
	{regexp.MustCompile(`(\d{1,2})นาฬิกา(ครึ่ง)?`), func(m []string) (int, int, bool) {
		h, _ := strconv.Atoi(m[1])
		return h, 0, h <= 23
	}},
}

type clockTime struct{ hour, minute int }

// parseThaiClock คืนเวลาของวัน ถ้าข้อความระบุเวลาไว้ชัดเจนเพียงเวลาเดียว
// ไม่เจอเวลา หรือเจอหลายเวลาที่ต่างกัน (เช่น "9 โมงกับบ่ายสาม") → ok=false
func parseThaiClock(text string) (hour, minute int, ok bool) {
	s := normalizeThaiDigits(strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, text))

	var (
		used  [][2]int
		found []clockTime
	)
	overlaps := func(a, b int) bool {
		for _, u := range used {
			if a < u[1] && b > u[0] {
				return true
			}
		}
		return false
	}
	for _, rule := range clockRules {
		for _, idx := range rule.re.FindAllStringSubmatchIndex(s, -1) {
			if overlaps(idx[0], idx[1]) {
				continue
			}
			sub := make([]string, len(idx)/2)
			for i := range sub {
				if idx[2*i] >= 0 {
					sub[i] = s[idx[2*i]:idx[2*i+1]]
				}
			}
			h, mi, valid := rule.hm(sub)
			if !valid {
				continue
			}
			if sub[len(sub)-1] == "ครึ่ง" {
				mi = 30
			}
			used = append(used, [2]int{idx[0], idx[1]})
			found = append(found, clockTime{h, mi})
		}
	}

	if len(found) == 0 {
		return 0, 0, false
	}
	for _, f := range found[1:] {
		if f != found[0] {
			return 0, 0, false
		}
	}
	return found[0].hour, found[0].minute, true
}

// normalizeThaiDigits แปลงเลขไทย ๐-๙ เป็นเลขอารบิก
func normalizeThaiDigits(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '๐' && r <= '๙' {
			return '0' + (r - '๐')
		}
		return r
	}, s)
}

// correctClock แก้ชั่วโมง/นาทีของเวลาที่โมเดลให้มา ให้ตรงกับเวลาที่ผู้ใช้พิมพ์ (ถ้าอ่านได้ชัดเจน)
// คงวันที่ที่โมเดลเลือกไว้ (วันที่แก้แยกใน correctDay)
func correctClock(at time.Time, userText string) time.Time {
	h, m, ok := parseThaiClock(userText)
	if !ok {
		return at
	}
	local := at.In(bangkok)
	if local.Hour() == h && local.Minute() == m {
		return at
	}
	fixed := time.Date(local.Year(), local.Month(), local.Day(), h, m, 0, 0, bangkok)
	log.Printf("แก้เวลาเตือนจาก %s เป็น %s ตามข้อความผู้ใช้", local.Format("15:04"), fixed.Format("15:04"))
	return fixed
}

// คำบอกวันแบบสัมพัทธ์ที่ชัดเจน → จำนวนวันจากวันนี้ ("มะรืน" ต้องเช็คก่อนเพราะไม่ชนกับคำอื่น)
var relativeDays = []struct {
	word   string
	offset int
}{
	{"มะรืน", 2},
	{"พรุ่งนี้", 1},
	{"วันนี้", 0},
	{"คืนนี้", 0},
	{"tomorrow", 1},
	{"today", 0},
	{"tonight", 0},
}

// parseRelativeDay คืนจำนวนวันจากวันนี้ ถ้าข้อความบอกวันแบบสัมพัทธ์ไว้แบบเดียว
// ไม่มี หรือมีหลายแบบที่ขัดกัน (เช่น "วันนี้หรือพรุ่งนี้") → ok=false
func parseRelativeDay(text string) (offset int, ok bool) {
	lower := strings.ToLower(text)
	found := -1
	for _, d := range relativeDays {
		if !strings.Contains(lower, d.word) {
			continue
		}
		if found >= 0 && found != d.offset {
			return 0, false
		}
		found = d.offset
	}
	return found, found >= 0
}

// correctDay แก้วันที่ของเวลาที่โมเดลให้มา ให้ตรงกับ "วันนี้/พรุ่งนี้/มะรืน" ที่ผู้ใช้พิมพ์ (คงเวลาไว้)
// โมเดลเล็กบางทีนับวันเกิน เช่น "พรุ่งนี้" → อีกสองวัน
func correctDay(at time.Time, userText string, now time.Time) time.Time {
	offset, ok := parseRelativeDay(userText)
	if !ok {
		return at
	}
	local := at.In(bangkok)
	today := now.In(bangkok)
	want := time.Date(today.Year(), today.Month(), today.Day()+offset, local.Hour(), local.Minute(), 0, 0, bangkok)
	if want.Year() == local.Year() && want.YearDay() == local.YearDay() {
		return at
	}
	log.Printf("แก้วันเตือนจาก %s เป็น %s ตามข้อความผู้ใช้", local.Format("2006-01-02"), want.Format("2006-01-02"))
	return want
}
