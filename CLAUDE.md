# boid

汎用パーソナル AI オーケストレータ。

## ビルド & テスト

```bash
go build ./...          # ビルド
go test ./...           # ユニットテスト
go test -race ./...     # レースコンディション検出
go vet ./...            # 静的解析
```

E2E テスト（`e2e/scenarios/` 配下）は `./e2e/run.sh [scenario]` で実行する。
サンドボックス内から呼んだ場合は `host_commands.run-e2e` の path match で host 側 broker に自動 dispatch されるため、 サンドボックス内 claude code からも普通に実行可能（実体は host で動く）。

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
| `~/.claude/projects/-home-...-worktrees-boid-<taskid>/` | Claude Code | **boid 管轄外**・手動管理が必要 |

`~/.claude/projects/` 配下の `*.jsonl` は Claude Code 本体が書き込むセッションログであり、boid が手を出すのは越権行為となる。
手動で削除する場合は `rm -rf ~/.claude/projects/-home-*-worktrees-boid-*` を実行すること（他のプロジェクトのログを巻き込まないよう注意）。

### 自動 GC

boid daemon 起動時に GC goroutine が立ち上がり、起動 **10 秒後**に初回実行、以降 **24 時間ごと**に **30 日より古いデータ**を自動削除する。
削除対象は `runtimes/<runtime_id>/` ディレクトリに加え、終端状態の tasks/actions/jobs（DB レコード）・worktree ディレクトリ・`/tmp/boid-*` ・失効済みデバイスも含む。
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

## サンドボックス内での Web アクセス

サンドボックス内では `WebFetch` ツールは無効化されている。web ページを読む場合は
`/boid-web` スキル経由で行う（haiku サブエージェントが `boid fetch <url>` を実行して要約を返す）。

## コーディング規約

- Go モジュールパスは `github.com/novshi-tech/boid` を使う
- TDD: テストを先に書き、失敗を確認してから実装する
- コミットプレフィックス: `feat:`, `fix:`, `refactor:`, `test:`
- 外部ライブラリは最小限。標準ライブラリで実現できるものは追加しない
- Linux のみ対応
