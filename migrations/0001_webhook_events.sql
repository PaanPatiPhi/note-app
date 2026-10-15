-- เก็บ webhookEventId ของ LINE ที่ประมวลผลแล้ว เพื่อกันประมวลผลซ้ำ
-- ใช้โดย note.Repository.TryMarkEventProcessed (INSERT ... ON CONFLICT DO NOTHING)
CREATE TABLE IF NOT EXISTS webhook_events (
  webhook_event_id text PRIMARY KEY,
  created_at       timestamptz NOT NULL DEFAULT now()
);

-- ใช้ลบแถวเก่าได้ภายหลัง (LINE redeliver ภายในช่วงเวลาสั้นๆ เท่านั้น)
CREATE INDEX IF NOT EXISTS webhook_events_created_at_idx ON webhook_events (created_at);
