# boid を Heddleway のトンネルに乗せる

ステータス: **段階 1・2 を実装 (2026-11-24)**。

関連: [split-and-shrink.md](split-and-shrink.md) §4.2・§8、Heddleway の `docs/adr/0003-reverse-connection.md` と `docs/tunnel-protocol.md`。

## 段階 1: トンネルクライアントと呼び返しの中継口

- `internal/tunnel`: ペアリング (Ed25519、標準モード)、challenge に署名した JWT で WebSocket、`hello` で説明を送る、`request` を daemon の中のハンドラで答える、`call` で呼び返す、30 秒ごとの `ping`、切れたら 1〜60 秒の倍々でつなぎ直す。`4003` (登録の削除) と `4001` (同じ鍵の別の接続に置き換わった) では止まる
- 鍵は data home の `tunnel.json` (0600)。形は Heddleway の `tools/tunnel-client.mjs` と同じなので、どちらで作った鍵も読める
- CLI: `boid tunnel pair <URL> [--name]`・`status`・`forget`。ペアリングは daemon が行う (鍵が daemon の volume から出ない)。管理の API は `/api/tunnel`
- **トンネルに出す口は job の読み取りだけ** (`GET /jobs`・`/jobs/{id}`・`/jobs/{id}/log`)。今の daemon の API は secret の値・config・workspace まで触れるので、丸ごとは出さない。トンネルからの要求はゲートウェイが所有者のものに限っているので、boid の側ではそれ以上の認証をしない
- **呼び返しの中継口**: API gateway と同じ listener・同じ job トークンで `/heddleway/<token>/<service>/...`。`tunnel.callback_workspaces` の workspace だけ通し (初期は空)、readonly の job は GET / HEAD だけ。`Authorization`・`Cookie`・`Origin` は流さず、`Origin` 付きの要求は断る。job の識別に `x-boid-job` (task ID) を付ける。env は `BOID_HEDDLEWAY_BASE`
- 厳しめのモードは入れていない。boid は所有者の PC で動き、鍵を daemon の volume に置く前提 (ADR 0003 決定 4 の標準)

- 実ゲートウェイでの疎通 (2026-11-24): `GET /jobs` が全件を古い順に返し 7 MB あったので、新しい順・既定 20 件・`output` 500 文字、ログは末尾 256 KiB にした

## 段階 2: トンネルから job を起動する

split-and-shrink §3.2 の job を、今ある session job の上に作る。

- `POST /jobs` `{"project", "instruction", "harness" (既定 claude), "readonly", "model", "name"}` → `202` と `{"id", "result_url"}`。ADR 0003 決定 6 の非同期の約束
- 起動は session job (`sessionDispatcherAdapter.StartSession`) をそのまま使う。指示の末尾に**結果の約束**を足す: 結果を JSON (`summary`・`questions`・`artifacts`) で `/tmp/boid-result.json` に書き、`boid job done "$BOID_JOB_ID" --output-file /tmp/boid-result.json` で終える。質問は待たずに結果に書いて終える (「ask は書いて終わる」)。成果は終える前に push する。ハーネスに求めるのは「指示を読む」「コマンドを打てる」だけなので、claude / codex / opencode で同じ
- `boid job done` は job の `output` に結果を残して runtime を止める (既存の仕組み)。`GET /jobs/{id}/result` は `output` が JSON なら `result`、そうでなければ `result_text` で返す
- `POST /jobs/{id}/stop` (既存の agent-stop)、`GET /projects` (id・名前・workspace・repository だけ)
- repo は今は project の ID か名前で指す。§3.3 の「repo の URL とブランチを渡す」は、プロジェクトの退役 (段階 4) と一緒に
- workspace での絞り込みはまだしない。呼べるのは所有者だけなので。顧客の workspace の結果がメインのアカウントに入る問題 (§6) は、結果の約束で「要約にとどめる」と頼むところまで
- 未決: 終わるまで待つ口 (long-poll) は、オーケストレータのスキルを作るときに要るか決める。ブランチの指定 (`BOID_BASE_BRANCH`) は今の session job が受けないので、要るなら足す

## 次の段階 (予定)

1. **オーケストレータのスキル** (§4.1): 起動 → 結果のポーリング → judge → 人に質問、のループ。ゲートウェイの boid を呼ぶスキルとして
2. **API gateway の退役**: 中継口が `BOID_API_BASE` の代わりになるので、`internal/apigateway`・`gwtransport` と `services` の設定を外す。スキルの向き先を `BOID_HEDDLEWAY_BASE` に移してから
3. **git gateway の退役**: Heddleway の git の接続先 (ADR 0002) を中継口経由で使い、`internal/gitgateway` とプロジェクトを外す
4. **task・card・signal・orchestrator の退役** (Murmur へ)。Web UI は job の一覧・ログ・attach だけに絞る
