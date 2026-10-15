-- AI assistant Phase 1: reminder บนตาราง notes + log ของ action ที่ AI ทำ (ใช้ undo/วัดต้นทุน/eval)
BEGIN;

-- ── notes: reminder ─────────────────────────────────────────────
ALTER TABLE notes
  ADD COLUMN IF NOT EXISTS remind_at   timestamptz,
  ADD COLUMN IF NOT EXISTS reminded_at timestamptz,
  ADD COLUMN IF NOT EXISTS source      text NOT NULL DEFAULT 'line';

ALTER TABLE notes DROP CONSTRAINT IF EXISTS notes_type_check;
ALTER TABLE notes ADD CONSTRAINT notes_type_check
  CHECK (type = ANY (ARRAY['note', 'todo', 'checklist', 'reminder']));

ALTER TABLE notes DROP CONSTRAINT IF EXISTS notes_source_check;
ALTER TABLE notes ADD CONSTRAINT notes_source_check
  CHECK (source = ANY (ARRAY['line', 'web', 'ai']));

-- scheduler ดึง reminder ที่ถึงเวลาแต่ยังไม่ได้ส่ง
CREATE INDEX IF NOT EXISTS idx_notes_pending_reminders
  ON notes (remind_at)
  WHERE remind_at IS NOT NULL AND reminded_at IS NULL;

-- ── ai_actions ──────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS ai_actions (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id           uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  webhook_event_id  text,
  input_text        text NOT NULL,
  model             text NOT NULL,
  tool_calls        jsonb NOT NULL DEFAULT '[]',  -- [{name, input, result, is_error}]
  reply_text        text,
  created_note_ids  uuid[] NOT NULL DEFAULT '{}',
  touched_item_ids  uuid[] NOT NULL DEFAULT '{}',
  input_tokens      int,
  output_tokens     int,
  cache_read_tokens int,
  undone_at         timestamptz,
  created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_ai_actions_user_created
  ON ai_actions (user_id, created_at DESC);

COMMIT;
