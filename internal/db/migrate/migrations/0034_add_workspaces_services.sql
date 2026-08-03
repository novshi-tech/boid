-- docs/plans/api-gateway.md §3: workspace 単位の API gateway service 有効化
-- (allowed_domains の floor + workspace 加算方式を鏡写し)。この migration の
-- 時点ではまだ WorkspaceMeta.Services が読み書きするだけ — service registry
-- 自体は config.yaml `services:` 側にある。
ALTER TABLE workspaces ADD COLUMN services TEXT NOT NULL DEFAULT '[]';
