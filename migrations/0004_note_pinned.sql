-- ปักหมุดโน้ต: widget บน iPhone แสดงเฉพาะโน้ตที่ปักหมุดไว้ (NULL = ไม่ได้ปัก)
BEGIN;

ALTER TABLE notes ADD COLUMN IF NOT EXISTS pinned_at timestamptz;

CREATE INDEX IF NOT EXISTS notes_user_pinned_idx
  ON notes (user_id, pinned_at DESC)
  WHERE pinned_at IS NOT NULL;

COMMIT;
