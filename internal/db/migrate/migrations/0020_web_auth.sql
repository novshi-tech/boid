CREATE TABLE IF NOT EXISTS web_devices (
  id           TEXT PRIMARY KEY,
  label        TEXT,
  cookie_hash  BLOB NOT NULL,
  created_at   TIMESTAMP NOT NULL,
  last_seen_at TIMESTAMP NOT NULL,
  revoked_at   TIMESTAMP
);
CREATE TABLE IF NOT EXISTS web_pairing_codes (
  code_hash   BLOB PRIMARY KEY,
  label       TEXT,
  created_at  TIMESTAMP NOT NULL,
  expires_at  TIMESTAMP NOT NULL,
  consumed_at TIMESTAMP
);
