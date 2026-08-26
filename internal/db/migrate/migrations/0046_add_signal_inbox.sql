-- docs/plans/signal-ingest-detailed-design.md §2 (PR-1): signal inbox の
-- 永続化 2 テーブル。signal-driven-review.md §8.1 の inbox 不変条件
-- (dedup は idempotency key で no-op / source cursor は処理済み Signal 自身
-- を越えて前進する / 取り込み中のまま残る終着状態を持たない) をこの schema
-- で支える。
--
-- signals: Connector が生成した Signal 1 件 1 行。
--   - dedup は PRIMARY KEY そのもの (workspace_id, service, connector, id)
--     — `INSERT OR IGNORE` で同一 4 つ組の再投入は no-op になる (採点表
--     Q10)。id は connector が生成する安定 event key で、workspace 内
--     一意ではない (同じ id が別 service/connector から来ることがある —
--     AckSignals の照合規則は internal/orchestrator/signal_store.go 参照)。
--   - workspace_id は workspaces.slug。project_workspaces の前例と同じく
--     FK は張らない (workspace の削除/再構成と signal 行のライフサイクルを
--     独立させる — 30 日 GC が別途面倒を見る)。
--   - occurred_at / received_at / acked_at は RFC3339 の TEXT。DATETIME 型
--     ではなく TEXT にしているのは、保存前に必ず UTC へ正規化した上で
--     **固定幅の** RFC3339 ナノ秒表記 (末尾ゼロを trim しない) で書き込む
--     制御を store 層自身に持たせるため — DATETIME 型で driver のシリアライズ
--     に委ねると末尾ゼロが trim され、レキシコグラフィックな
--     `ORDER BY occurred_at` が実時刻の順序と食い違いうる (M2, Opus review
--     2026-08-26 — internal/orchestrator/signal_store.go の
--     signalTimeLayout のコメント参照)。connector から届く occurred_at の
--     tz 表記 (jira は `+09:00` 等) はワイヤ上の入力としては受け付けるが、
--     **保存値は常に UTC 正規化済み** であり生の offset は残らない。cursor
--     との比較も文字列比較ではなく store 層が明示的に time.Parse して行う
--     (internal/orchestrator/signal_store.go の IngestSignals のコメント
--     参照 — 文字列比較は offset 混在で壊れる)。
--   - attempts / acked_at が「取り込み中のまま残る終着状態を持たない」の
--     実装: attempts は connector の取り込み失敗**ではなく**、判断側への
--     配信試行回数 — ClaimSignals が呼ばれるたび (scan script が signal を
--     掴んで判断を試みるたび) に加算される。判断が終わらないまま (crash 等
--     で) 放置された signal は pending のまま残り、次の ClaimSignals で
--     再配送される。上限 (MaxSignalAttempts) 超過は dead として可視化される
--     が行は消えない — 判断側が ack するか GC (30 日、GCSignals) が刈るまで
--     残る。connector 自体の取り込み失敗 (非ゼロ exit) は別の機構 (導出
--     trigger の failStreak、design doc §5.1) で可視化され、signals.attempts
--     とは無関係 (L5, Opus review 2026-08-26)。
CREATE TABLE IF NOT EXISTS signals (
  workspace_id TEXT NOT NULL,           -- workspaces.slug (FK は張らない: project_workspaces の前例)
  service      TEXT NOT NULL,           -- service instance 名
  connector    TEXT NOT NULL,           -- "<pack>/<connector>"
  id           TEXT NOT NULL,           -- envelope id (connector が生成する安定 event key)
  occurred_at  TEXT NOT NULL,           -- RFC3339 (tz-aware)
  identity     TEXT NOT NULL,
  url          TEXT NOT NULL DEFAULT '',
  author       TEXT NOT NULL DEFAULT '',
  title        TEXT NOT NULL DEFAULT '',
  received_at  TEXT NOT NULL,           -- daemon 時計
  attempts     INTEGER NOT NULL DEFAULT 0,
  acked_at     TEXT,
  PRIMARY KEY (workspace_id, service, connector, id)
);

-- ClaimSignals ("pending を occurred_at 昇順に limit 件") と
-- HasPendingSignals ("workspace に pending が 1 件でもあるか") の両方を
-- カバーする部分 index。acked_at IS NULL の行だけが対象で、ack 済みの
-- 大多数 (30 日で GC される until) をインデックスに含めない。
CREATE INDEX IF NOT EXISTS idx_signals_pending
  ON signals(workspace_id, occurred_at) WHERE acked_at IS NULL;

-- signal_cursors: source (workspace, service, connector) ごとの栞。
-- IngestSignals が同一 tx 内で `max(rows.occurred_at)` へ単調にのみ前進
-- させる (現在値より小さい値には絶対に戻さない — signal-driven-review.md
-- §8.1 の「source cursor は処理済み Signal 自身を越えて前進する」の実装)。
-- cursor は空文字列で「最初から」を表す (行が無い = GetSignalCursor が
-- 空文字列を返す)。
CREATE TABLE IF NOT EXISTS signal_cursors (
  workspace_id TEXT NOT NULL,
  service      TEXT NOT NULL,
  connector    TEXT NOT NULL,
  cursor       TEXT NOT NULL DEFAULT '',
  updated_at   TEXT NOT NULL,
  PRIMARY KEY (workspace_id, service, connector)
);
