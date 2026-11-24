# boid を Heddleway のトンネルに乗せる

ステータス: **段階 1 を実装 (2026-11-24)**。

関連: [split-and-shrink.md](split-and-shrink.md) §4.2・§8、Heddleway の `docs/adr/0003-reverse-connection.md` と `docs/tunnel-protocol.md`。

## 段階 1: トンネルクライアントと呼び返しの中継口

- `internal/tunnel`: ペアリング (Ed25519、標準モード)、challenge に署名した JWT で WebSocket、`hello` で説明を送る、`request` を daemon の中のハンドラで答える、`call` で呼び返す、30 秒ごとの `ping`、切れたら 1〜60 秒の倍々でつなぎ直す。`4003` (登録の削除) と `4001` (同じ鍵の別の接続に置き換わった) では止まる
- 鍵は data home の `tunnel.json` (0600)。形は Heddleway の `tools/tunnel-client.mjs` と同じなので、どちらで作った鍵も読める
- CLI: `boid tunnel pair <URL> [--name]`・`status`・`forget`。ペアリングは daemon が行う (鍵が daemon の volume から出ない)。管理の API は `/api/tunnel`
- **トンネルに出す口は job の読み取りだけ** (`GET /jobs`・`/jobs/{id}`・`/jobs/{id}/log`)。今の daemon の API は secret の値・config・workspace まで触れるので、丸ごとは出さない。トンネルからの要求はゲートウェイが所有者のものに限っているので、boid の側ではそれ以上の認証をしない
- **呼び返しの中継口**: API gateway と同じ listener・同じ job トークンで `/heddleway/<token>/<service>/...`。`tunnel.callback_workspaces` の workspace だけ通し (初期は空)、readonly の job は GET / HEAD だけ。`Authorization`・`Cookie`・`Origin` は流さず、`Origin` 付きの要求は断る。job の識別に `x-boid-job` (task ID) を付ける。env は `BOID_HEDDLEWAY_BASE`
- 厳しめのモードは入れていない。boid は所有者の PC で動き、鍵を daemon の volume に置く前提 (ADR 0003 決定 4 の標準)

## 次の段階 (予定)

1. **job の API をトンネルに出す**: split-and-shrink §3.2 の job (workspace × ハーネス × 指示 × repo とブランチ、状態は running / completed / failed、結果は JSON) を起動する口と、結果を取る口。非同期 (`202` と ID) の約束に合わせる。オーケストレータのスキル (§4.1) と一緒に作る
2. **API gateway の退役**: 中継口が `BOID_API_BASE` の代わりになるので、`internal/apigateway`・`gwtransport` と `services` の設定を外す。スキルの向き先を `BOID_HEDDLEWAY_BASE` に移してから
3. **git gateway の退役**: Heddleway の git の接続先 (ADR 0002) を中継口経由で使い、`internal/gitgateway` とプロジェクトを外す
4. **task・card・signal・orchestrator の退役** (Murmur へ)。Web UI は job の一覧・ログ・attach だけに絞る
