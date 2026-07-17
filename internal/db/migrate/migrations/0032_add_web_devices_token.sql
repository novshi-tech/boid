-- docs/plans/cli-remote-connection.md Phase 3 PR0: Bearer device token 発行
-- (POST /api/auth/device) のため web_devices を拡張する。
--   - cookie_hash を nullable 化: Bearer-only device (CLI 経由でペアリング
--     された device) は cookie を一切持たないため、既存の NOT NULL 制約は
--     そのままでは insert できない。
--   - token_hash BLOB を追加: raw Bearer token の SHA-256 hash。raw token
--     自体は DB に一切残さない (発行レスポンスで一度だけ返す)。
--   - token_created_at DATETIME を追加: audit 用、cookie 側に対応物はない。
-- SQLite に列単位の ALTER COLUMN (NOT NULL 制約緩和) は無いため、
-- 0021_jobs_nullable_task_id.sql の precedent に倣いテーブル再作成で行う。
CREATE TABLE web_devices_new (
  id               TEXT PRIMARY KEY,
  label            TEXT,
  cookie_hash      BLOB,
  token_hash       BLOB,
  token_created_at TIMESTAMP,
  created_at       TIMESTAMP NOT NULL,
  last_seen_at     TIMESTAMP NOT NULL,
  revoked_at       TIMESTAMP
);
INSERT INTO web_devices_new (id, label, cookie_hash, created_at, last_seen_at, revoked_at)
SELECT id, label, cookie_hash, created_at, last_seen_at, revoked_at
FROM web_devices;
DROP TABLE web_devices;
ALTER TABLE web_devices_new RENAME TO web_devices;

-- token_hash is unique when present (two devices must never share a raw
-- token); SQLite treats every NULL as distinct for UNIQUE indexes, so
-- cookie-only devices (token_hash IS NULL) are unaffected.
CREATE UNIQUE INDEX idx_web_devices_token_hash ON web_devices(token_hash) WHERE token_hash IS NOT NULL;
