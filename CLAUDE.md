# boid

汎用パーソナル AI オーケストレータ。

## ビルド & テスト

```bash
go build ./...          # ビルド
go test ./...           # ユニットテスト
go test -race ./...     # レースコンディション検出
go vet ./...            # 静的解析
```

E2E テスト（`e2e/run-container.sh`、real docker/podman engine が必要）はホスト側では `./e2e/run-container.sh` で実行する。 PR-4（`docs/plans/volume-only-daemon.md` §論点e）で userns backend を撤去したのに伴い、それに依存していた per-scenario black-box harness（`e2e/run.sh` + `e2e/scenarios/*`）は撤去済み — container backend の compose sibling-connectivity / workspace network isolation / reap 系の固定 flow のみを検証する単一スクリプトになった。
サンドボックス内 (Claude Code 等) からは `run-e2e` (declared short name、scenario 引数は撤去済み) を PATH 経由で呼ぶ — `/run/boid/bin/run-e2e` symlink 経由で host 側 broker に dispatch される (実体は host で動く)。 サンドボックス内で `./e2e/run-container.sh` を直接叩くと、 サンドボックス内から docker/podman engine にアクセスできないため失敗する。

## プロジェクト構成

- `main.go` — エントリポイント
- `cmd/` — CLI コマンド定義（cobra）
- `internal/` — 内部パッケージ
  - `api/` — HTTP ハンドラ
  - `client/` — UNIX ソケット HTTP クライアント
  - `config/` — 設定ファイル読み込み・デフォルト値
  - `daemon/` — デーモン起動・停止・PID 管理
  - `db/` — SQLite 永続化
  - `dispatcher/` — ジョブ dispatch・サンドボックスビルド
  - `initwizard/` — 初回セットアップウィザード
  - `kit/` — キット（再利用可能な拡張パッケージ）
  - `logrotate/` — ログローテーション
  - `notify/` — 通知サービス
  - `orchestrator/` — 状態機械・遷移モデル・project.yaml パース・hook 評価
  - `qrterm/` — ターミナル QR コード表示
  - `sandbox/` — サンドボックス実行環境
  - `server/` — UNIX ソケット + TCP サーバ・ルーティング
  - `skills/` — 組み込みスキル管理
  - `timeline/` — タイムライン記録
  - `tui/` — TUI（テキスト UI）
- `web/` — Templ テンプレート + 静的ファイル
- `testutil/` — テストヘルパー
- `e2e/` — E2E テスト

## ディスク使用量の管理

boid が管理するディスクデータは2種類ある:

| ディレクトリ | 管理主体 | GC 対象 |
|---|---|---|
| `~/.local/share/boid/runtimes/<runtime_id>/` | boid | daemon が自動削除（24h 毎）|
| `~/.claude/projects/-workspace-<project-name>-.../`（project ごと） | Claude Code | **boid 管轄外**・手動管理が必要 |

`~/.claude/projects/` 配下の `*.jsonl` は Claude Code 本体が書き込むセッションログであり、boid が手を出すのは越権行為となる。
git gateway cutover (2026-07) 後はジョブの cwd が sandbox 内の `/workspace/<project-name>`（project 名ベース、task ID を含まない）になったため、同一 project の複数タスクが同じログディレクトリに集約される。手動で削除する場合は実際に一度 dispatch した上で `~/.claude/projects/` 配下の該当ディレクトリを確認し、他プロジェクトのログを巻き込まないよう注意して削除すること。

### 自動 GC

boid daemon 起動時に GC goroutine が立ち上がり、起動 **10 秒後**に初回実行、以降 **24 時間ごと**に **30 日より古いデータ**を自動削除する。
削除対象は `runtimes/<runtime_id>/` ディレクトリに加え、終端状態の tasks/actions/jobs（DB レコード）・`/tmp/boid-*` ・失効済みデバイスも含む。
手動実行は引き続き `boid gc` で可能。設定は `docs/ja/reference/config-yaml.md` を参照。

## Web UI

- `boid start` のデフォルトで Web UI は有効 (`http://localhost:8080`、 listen アドレスは `boid web set-addr <addr>` で変更可)
- 初回は `boid web pair` でペアリングコード (5 分有効、単回) を発行、コード / URL / QR で登録
- デバイス管理: `boid web devices` / `boid web revoke <id>` / `boid web revoke-all`
- loopback (127.0.0.1/::1) からはペアリング不要、外部公開 (Cloudflare Tunnel 等) からは必須
- public URL は `web.public_url` に設定 (マジックリンク用、詳細は `docs/ja/reference/config-yaml.md`)
- 署名鍵は `~/.local/share/boid/web_secret` に自動生成 (0600)

Cloudflare Tunnel 公開手順は docs/ja/guide/web-ui.md を参照。

## セキュリティモデル

Hook と Exec はサンドボックス実行。Gate 機構は廃止済みで、現行 dispatch は hook のみ（`JobKind` は `hook`/`exec`）。サンドボックスの書き込み可否は `task.readonly` および `command.readonly`（Exec の場合）のみで決まる。role による差分はない。

hook の定義経路は `hooks[].command`（inline shell command、`sh -c` 経由で実行）または `kind: agent`（virtual agent hook 合成）の 2 つのみ。`.boid/hooks/*.sh` のような外部 script ファイルを参照する経路（旧 `ScriptPath`）は 2026-07 に撤廃済み（`docs/plans/script-hook-removal.md`）。理由は dead code 化に加え、sandbox 内 clone は tracked file しか持ってこないため `.boid/` を gitignore している project で script hook が silent に ENOENT 落ちする契約問題があり、外部 script 参照経路自体を無くすことで根絶した。詳細は `docs/ja/reference/project-yaml.md` の `hooks` 節を参照。

## サンドボックス実行バックエンド

サンドボックスの実装は `SandboxBackend` interface (`internal/sandbox/backend/`) で抽象化されている。 **container backend が唯一の backend**（Phase 6 `docs/plans/phase6-container-backend.md` で実装、PR-4 `docs/plans/volume-only-daemon.md` §論点e で userns backend を完全撤去し単一化、2026-07-25）: docker コンテナを job ごとに使い捨てで生成する docker-out-of-docker 方式。job は workspace 単位で分離された docker network に閉じ込められる。daemon 自体は `docker compose`（`build/container/compose.yml`、`scripts/deploy-container.sh`）でコンテナ化して運用するのが推奨だが、bare `boid start`（host process 直接起動）から動かす場合も同じ container backend で dispatch する（docker/podman socket へのアクセスが必須）。

`sandbox.backend` config option は撤去済み（container 以外の選択肢が無いため）。旧 config.yaml に残っている場合は起動時に warning を出して無視する（`KindOpaque`、`internal/config/schema.go`）。userns backend（rootless userns + pivot_root + 5 段 mount）のコードは完全に削除済み — 撤去計画は `docs/plans/phase6-cutover-followups.md`（歴史記録）を参照。

## サンドボックス内での Web アクセス

サンドボックス内では `WebFetch` ツールは無効化されている。web ページを読む場合は
`/boid-web` スキル経由で行う（haiku サブエージェントが `boid fetch <url>` を実行して要約を返す）。

## コーディング規約

- Go モジュールパスは `github.com/novshi-tech/boid` を使う
- TDD: テストを先に書き、失敗を確認してから実装する
- コミットプレフィックス: `feat:`, `fix:`, `refactor:`, `test:`
- 外部ライブラリは最小限。標準ライブラリで実現できるものは追加しない
- Linux のみ対応
