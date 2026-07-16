-- Phase 2.5 (workspace DB 一元化) の schema 先置き。この migration は
-- workspaces テーブルを作成するのみで、read/write の権威は引き続き
-- ~/.config/boid/workspaces/*.yaml のまま (DB は空)。詳細は
-- docs/plans/workspace-db-consolidation.md を参照。
CREATE TABLE IF NOT EXISTS workspaces (
  slug                TEXT PRIMARY KEY,
  container_image     TEXT,                              -- nullable, Phase 6 まで無視
  host_commands       TEXT NOT NULL DEFAULT '[]',        -- JSON: []string of names
  env                 TEXT NOT NULL DEFAULT '{}',        -- JSON: map[string]string
  allowed_domains     TEXT NOT NULL DEFAULT '[]',        -- JSON: []string
  extra_repos         TEXT NOT NULL DEFAULT '[]',        -- JSON: []string
  capabilities        TEXT NOT NULL DEFAULT '{}',        -- JSON: Capabilities struct
  additional_bindings TEXT NOT NULL DEFAULT '[]',        -- JSON: []BindMount, Phase 4 で退役
  created_at          DATETIME NOT NULL DEFAULT (datetime('now')),
  updated_at          DATETIME NOT NULL DEFAULT (datetime('now'))
);
