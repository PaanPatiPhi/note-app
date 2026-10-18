package assistant

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseThaiClock(t *testing.T) {
	cases := map[string]string{ // ข้อความ → "HH:MM" หรือ "-" (อ่านไม่ได้/กำกวม)
		"พรุ่งนี้สองทุ่มเตือนจ่ายค่าเน็ต": "20:00",
		"เตือนจ่ายค่าไฟพรุ่งนี้ทุ่มนึง":   "19:00",
		"ทุ่มหนึ่งไปวิ่ง":                 "19:00",
		"สามทุ่มครึ่งโทรหาแม่":            "21:30",
		"5 ทุ่มปิดไฟ":                     "23:00",
		"พรุ่งนี้บ่ายสองเตือนไปรับลูก":    "14:00",
		"บ่ายโมงประชุม":                   "13:00",
		"บ่าย 3 โมงครึ่งนัดหมอ":           "15:30",
		"ห้าโมงเย็นรดน้ำต้นไม้":           "17:00",
		"พรุ่งนี้ 9 โมงเตือนโทรหาหมอ":     "09:00",
		"แปดโมงเช้าส่งงาน":                "08:00",
		"สิบโมงตรงเข้าประชุม":             "10:00",
		"ตีห้าตื่นไปวิ่ง":                 "05:00",
		"เที่ยงกินข้าวกับทีม":             "12:00",
		"เที่ยงคืนส่งงาน":                 "00:00",
		"ประชุม 14:30 ห้อง 3":             "14:30",
		"นัดหมอ ๙ โมง":                    "09:00",
		"20 นาฬิกา ดูบอล":                 "20:00",
		// อ่านไม่ได้หรือกำกวม → ไม่แก้
		"เตือนให้ซื้อพัดลมใหม่หน่อย": "-",
		"ตีแบดกับเพื่อนพรุ่งนี้":     "-", // "ตี" ไม่ตามด้วยตัวเลข
		"9 โมงกับบ่ายสามมีประชุม":    "-", // สองเวลาต่างกัน
		"บ่ายนี้ไปธนาคาร":            "-",
		"ซื้อของ 3 อย่าง":            "-",
		"สองโมงประชุม":               "-", // 2 โมงไม่มีคำบอกช่วง → กำกวม
	}
	for text, want := range cases {
		h, m, ok := parseThaiClock(text)
		got := "-"
		if ok {
			got = fmt.Sprintf("%02d:%02d", h, m)
		}
		if got != want {
			t.Errorf("parseThaiClock(%q) = %s, want %s", text, got, want)
		}
	}
}

func TestCorrectClockKeepsDate(t *testing.T) {
	modelSaid := time.Date(2026, 10, 19, 22, 0, 0, 0, bangkok) // โมเดลแปล "สองทุ่ม" เป็น 22:00
	got := correctClock(modelSaid, "พรุ่งนี้สองทุ่มเตือนจ่ายค่าเน็ต")
	want := time.Date(2026, 10, 19, 20, 0, 0, 0, bangkok)
	if !got.Equal(want) {
		t.Errorf("correctClock = %v, want %v", got, want)
	}
	// อ่านเวลาไม่ได้ → ใช้ของโมเดลตามเดิม
	if got := correctClock(modelSaid, "อีก 30 นาทีเตือนปิดเตา"); !got.Equal(modelSaid) {
		t.Errorf("unparseable text changed the time: %v", got)
	}
}

func TestCreateReminderFixesModelHour(t *testing.T) {
	store := newFakeStore()
	llm := &fakeLLM{responses: []string{
		toolUseMsg("t1", "create_reminder", map[string]any{"text": "จ่ายค่าเน็ต", "remind_at": "2026-10-16T22:00:00+07:00"}),
		textMsg("⏰ ตั้งเตือนแล้วครับ"),
	}}
	a, _ := newTestAssistant(llm, store)
	handle(t, a, "พรุ่งนี้สองทุ่มเตือนจ่ายค่าเน็ต")

	want := time.Date(2026, 10, 16, 20, 0, 0, 0, bangkok)
	if len(store.notes) != 1 || !store.notes[0].RemindAt.Equal(want) {
		t.Fatalf("saved remind_at = %v, want %v", store.notes[0].RemindAt, want)
	}
	// โมเดลได้เวลาที่แก้แล้วกลับไป จะได้ตอบผู้ใช้ด้วยเวลาที่ถูก
	if !strings.Contains(llm.requests[1], "20:00") {
		t.Errorf("tool result should carry the corrected time: %s", llm.requests[1])
	}
}

func TestCorrectDay(t *testing.T) {
	now := time.Date(2026, 10, 18, 11, 0, 0, 0, bangkok)
	modelSaid := time.Date(2026, 10, 20, 20, 0, 0, 0, bangkok) // โมเดลนับ "พรุ่งนี้" เป็นอีกสองวัน
	cases := []struct {
		text string
		want time.Time
	}{
		{"พรุ่งนี้สองทุ่มเตือนจ่ายค่าเน็ต", time.Date(2026, 10, 19, 20, 0, 0, 0, bangkok)},
		{"คืนนี้สองทุ่มดูบอล", time.Date(2026, 10, 18, 20, 0, 0, 0, bangkok)},
		{"มะรืนนี้สองทุ่มประชุม", modelSaid},          // ถูกอยู่แล้ว
		{"วันศุกร์สองทุ่มประชุม", modelSaid},          // ไม่ใช่คำสัมพัทธ์ → ไม่แก้
		{"วันนี้หรือพรุ่งนี้สองทุ่มก็ได้", modelSaid}, // ขัดกัน → ไม่แก้
	}
	for _, tc := range cases {
		if got := correctDay(modelSaid, tc.text, now); !got.Equal(tc.want) {
			t.Errorf("correctDay(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
	// ข้ามเดือน
	endOfMonth := time.Date(2026, 10, 31, 9, 0, 0, 0, bangkok)
	if got := correctDay(endOfMonth, "พรุ่งนี้ 9 โมง", endOfMonth); !got.Equal(time.Date(2026, 11, 1, 9, 0, 0, 0, bangkok)) {
		t.Errorf("month rollover = %v", got)
	}
}
