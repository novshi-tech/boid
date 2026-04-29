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
  - `server/` — UNIX ソケット + TCP サーバ
  - `client/` — UNIX ソケット HTTP クライアント
  - `db/` — SQLite 永続化
  - `api/` — HTTP ハンドラ
  - `reducer/` — StateMachine + 遷移モデル
  - `hook/` — フック評価・発火
  - `project/` — プロジェクト管理・project.yaml パース
  - `sandbox/` — サンドボックス
  - `hostcmd/` — ホストコマンドブローカー
  - `job/` — ジョブ実行
  - `tmux/` — tmux セッション管理
  - `kit/` — キット（再利用可能な拡張パッケージ）
  - `model/` — ドメインモデル型定義
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

boid daemon 起動時に GC goroutine が立ち上がり、**24 時間ごと**・**30 日より古いデータ**を自動削除する。
手動実行は引き続き `boid gc` で可能。

設定は `~/.config/boid/config.yaml`（XDG 準拠）で変更できる:

```yaml
gc:
  enabled: true       # false にすると自動 GC を無効化
  interval: 24h       # GC の実行間隔（デフォルト: 24h）
  older_than: 720h    # この期間より古いデータを削除（デフォルト: 720h = 30日）
```

ファイルが存在しない場合はデフォルト値で動作する（エラーにならない）。

## Web UI

- `boid start` のデフォルトで Web UI は有効 (`http://localhost:8080`、 listen は `--http-addr` で変更可)
- 初回は `boid web pair` でペアリングコード (5 分有効、単回) を発行、コード / URL / QR で登録
- デバイス管理: `boid web devices` / `boid web revoke <id>` / `boid web revoke-all`
- loopback (127.0.0.1/::1) からはペアリング不要、外部公開 (Cloudflare Tunnel 等) からは必須
- public URL は `~/.config/boid/config.yaml` の `web.public_url` に設定 (マジックリンク用)
- 署名鍵は `~/.local/share/boid/web_secret` に自動生成 (0600)

Cloudflare Tunnel 公開手順は docs/plans/web-ui-rebuild.md を参照。

## セキュリティモデル

Gate はホスト直実行、Hook と Exec はサンドボックス実行。サンドボックスの書き込み可否は `task.readonly`（Hook の場合は `task.Status == verifying` でも readonly）および `command.readonly`（Exec の場合）のみで決まる。role による差分はない。

## コーディング規約

- Go モジュールパスは `github.com/novshi-tech/boid` を使う
- TDD: テストを先に書き、失敗を確認してから実装する
- コミットプレフィックス: `feat:`, `fix:`, `refactor:`, `test:`
- 外部ライブラリは最小限。標準ライブラリで実現できるものは追加しない
- Linux のみ対応

## 並列 dev タスクとコンフリクト

### 予防ガイド

- 同じファイルを編集する可能性のある dev タスク同士は `depends_on` + `depends_on_payload: "artifact.auto-merge.merged"` で直列化すること
- プランニング段階で「ファイル触る範囲の overlap」を見積もり、overlap があれば順序依存を入れる
- 順序に意味がない場合でも、同じディレクトリを深く触るタスクは直列化を検討する
- テストファイルや config ファイルなど、共有度が高いファイルは特に注意

### コンフリクト発生後の復旧手順

auto-merge は 2 つの gate で構成される:
- `mergeable-check` (exit gate on verifying): PR の mergeable 状態を検証し、conflict があれば finding を書き込む
- `auto-merge` (entry gate on done): done 入場時に `gh pr merge` を実行する

conflict 発生時のフロー (`verifying → reworking → verifying → done`):

1. `mergeable-check` exit gate が verifying 退場時に PR の mergeable 状態を確認し、conflict があれば finding (open) を書き込む
2. state machine が unresolved finding を検出し、自動的に reworking に遷移する
3. 該当 task の worktree で Claude が `git merge origin/main` を実行してコンフリクトを解消する（rebase ではなく merge を使うこと）
4. agent が commit + `git push` + (既存 PR が無ければ) `gh pr create` + `gh pr checks --watch` で CI 完了確認
5. agent が CI 結果を `payload_patch.verification.findings` に書き込んで exit (`boid task update --payload-file -`)
6. findings が resolved になると state machine が reworking → verifying に自動遷移する
7. `mergeable-check` が再実行され、conflict が解消されていれば verifying → done に進む
8. `auto-merge` entry gate が done 入場時に `gh pr merge` を実行する

重要な注意点:

- `git rebase` は使わない。merge で fast-forward 互換な履歴を作ることで force push を不要にしている
- エージェントは手動で `git fetch` しようとしないこと。必要な fetch は worktree 再構築時に `WorktreeManager.Recreate` が自動で実行する（base branch も fetch 済み）
