-- docs/plans/egress-proxy-stable-port.md: egress プロキシのポートを daemon
-- 再起動をまたいで不変にするための永続化テーブル。
--
-- 背景: Proxy.Start は `net.Listen(host+":0")` でエフェメラル採番していたため、
-- daemon を再起動するたびに workspace ごとのプロキシポートが変わっていた。
-- env の HTTPS_PROXY を読むツール (curl / git / boid 自身) は影響を受けないが、
-- プロキシ URL を設定ファイルに焼くツールは壊れる。実際、永続 HOME volume 上の
-- ~/.npmrc に古いポートが焼き込まれ、npm/pnpm が ECONNREFUSED を 1 分
-- バックオフでリトライし続ける ("ハングする") 事故が起きた。
--
-- 設計上の判断が 2 つある。
--
-- 1. workspaces テーブルの列にしない。
--    workspace meta は `boid workspace export` / `apply` でユーザが往復させる
--    面であり、daemon が採番した実装都合の値がそこに現れると、export した
--    yaml を別環境に apply した時に無意味な値を持ち込む。ユーザが編集しては
--    いけない値なので面を分ける (0036_add_workspace_watchdog.sql と同じ流儀)。
--
-- 2. workspaces(slug) への FK を張らない。かつ列名を workspace_id にしない。
--    キーは workspace slug とは限らない。dispatcher.NoWorkspaceProxyKey
--    ("__no_workspace__" — workspace に属さない `boid exec` / ProfileInit 用の
--    listener) という予約キーがあり、これは ValidWorkspaceSlug が決して許さない
--    "_" を含むため、実在の slug には決してならない。FK を張るとこのキーが入らない。
--    proxy_key という名前で「これは workspace とは限らない allocator のキーである」
--    ことを表す。
--
-- port の UNIQUE は二重割当の防止。ポート採番自体は Go 側 (ProxyManager) が
-- 実際に bind して確かめるので DB は権威ではないが、論理的に壊れた状態を
-- 書けなくしておく。
CREATE TABLE IF NOT EXISTS workspace_egress_port (
    proxy_key   TEXT PRIMARY KEY,
    port        INTEGER NOT NULL UNIQUE,
    updated_at  DATETIME NOT NULL
);
