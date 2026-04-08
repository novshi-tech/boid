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
