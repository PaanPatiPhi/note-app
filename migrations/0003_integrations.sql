-- บริการภายนอกที่ผู้ใช้เชื่อมไว้ (เริ่มที่ Google Calendar) — token เก็บแบบเข้ารหัส (AES-GCM, key จาก INTEGRATIONS_KEY)
BEGIN;

CREATE TABLE IF NOT EXISTS integrations (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id          uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider         text NOT NULL,           -- 'google'
  account_email    text,
  scopes           text NOT NULL DEFAULT '',
  access_token     text NOT NULL,           -- เข้ารหัสแล้ว (base64)
  refresh_token    text NOT NULL,           -- เข้ารหัสแล้ว (base64)
  token_expiry     timestamptz,
  calendar_id      text NOT NULL DEFAULT 'primary',
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, provider)
);

COMMIT;
