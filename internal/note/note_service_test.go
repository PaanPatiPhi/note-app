package note

import "testing"

func TestIsCommand(t *testing.T) {
	cases := map[string]bool{
		// คำสั่งเดิม → fast path
		"ลบ":            true,
		"  ดู  ":        true,
		"ดูวันนี้":      true,
		"ดู 25/08":      true,
		"ดู 25/08/2026": true,
		"ดูลิส":         true,
		"ดูลิส 1/9":     true,
		"ดูโน้ตวันนี้":  true,
		"ดูโน้ต 3/10":   true,
		"แก้ ซื้อนมสองขวด": true,
		"หา หมอ":           true,
		"เสร็จ 2":          true,
		"นม เสร็จ":         true,
		"น้ำหกแพ็ค ยกเลิก": true,

		// ข้อความอิสระ → AI
		"จดว่าอย่าลืมซื้อของ":          false,
		"ซื้อนม, ไข่, ขนมปัง":          false,
		"พรุ่งนี้ 9 โมงเตือนโทรหาหมอ":  false,
		"ซื้อนมแล้ว":                   false,
		"ดู หนังกับแฟนพรุ่งนี้ 2 ทุ่ม": false, // ขึ้นต้นด้วย "ดู " แต่ไม่ใช่วันที่
		"ดูลิส ซื้อของ":                false,
		"เสร็จ งานบ้าน":                false, // ไม่ใช่เลขลำดับ
		"ดูแลแมวด้วย":                  false,
		"หาหมอฟันพรุ่งนี้":             false, // ไม่มีช่องว่างหลัง "หา"
		"เสร็จ": false,
		"ลบโน้ตเรื่องประชุมเมื่อวาน": false,
	}
	for text, want := range cases {
		if got := IsCommand(text); got != want {
			t.Errorf("IsCommand(%q) = %v, want %v", text, got, want)
		}
	}
}
