# boid

汎用パーソナル AI オーケストレータ。

## ビルド & テスト

```bash
go build ./...          # ビルド
go test ./...           # ユニットテスト
go test -race ./...     # レースコンディション検出
go vet ./...            # 静的解析
```

E2E テスト（`e2e/scenarios/` 配下）はサンドボックス内では実行できない（nft/unshare 等の特権が必要）。
E2E の検証は CI（GitHub Actions）に任せること。ホスト上で直接実行する場合は `./e2e/run.sh [scenario]`。

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

## コーディング規約

- Go モジュールパスは `github.com/novshi-tech/boid` を使う
- TDD: テストを先に書き、失敗を確認してから実装する
- コミットプレフィックス: `feat:`, `fix:`, `refactor:`, `test:`
- 外部ライブラリは最小限。標準ライブラリで実現できるものは追加しない
- Linux のみ対応

## 並列 dev タスクとコンフリクト

### 予防ガイド

- 同じファイルを編集する可能性のある dev タスク同士は `depends_on` + `depends_on_payload: "artifact.pr.merged"` で直列化すること
- プランニング段階で「ファイル触る範囲の overlap」を見積もり、overlap があれば順序依存を入れる
- 順序に意味がない場合でも、同じディレクトリを深く触るタスクは直列化を検討する
- テストファイルや config ファイルなど、共有度が高いファイルは特に注意

### コンフリクト発生後の復旧手順

1. `boid script run github-auto-merge/detect-conflicts` を手動で実行
2. detect-conflicts が対象 task を自動的に reworking に戻し、finding を書き込む
3. 該当 task の worktree で Claude が `git merge origin/main` を実行してコンフリクトを解消する（rebase ではなく merge を使うこと）
4. commit のみ。push は pr-verify gate が実行する
5. pr-verify が通常 push し、再度 CI が回る
6. 緑になれば auto-merge が動き、task が done に戻る

重要な注意点:

- `git rebase` は使わない。merge で fast-forward 互換な履歴を作ることで force push を不要にしている
- hook role からは git push/fetch が禁止されているので、手動で push しようとしない（broker が reject する）
