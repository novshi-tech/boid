# boid ドキュメント

`boid` はパーソナル AI オーケストレータです。複数の AI コーディングエージェントを並行で走らせるとき、人間がボトルネックにならないことを目標としています。エージェントに自律的に進める余地を与え、書き込み範囲を絞れるサンドボックスで安全性を担保し、進行中の全タスクを CLI / Web UI で横断的に把握できます。すべて手元のマシン上で完結し、サーバもサインアップも不要です。

アーキテクチャ自体は特定の AI エージェントに依存しない設計ですが、 現時点で実用的に動作確認が取れているのは **Claude Code** のみです。 チュートリアル類はこの前提で書かれています。

このページはエントリポイントです。ドキュメントは段階的に整備中で、未着手のページはリンクなしで予定として記載しています。

[English version](../en/README.md)

## 目的別エントリ

- **はじめて使う。** → [インストール](getting-started/01-install.md)
- **モデル (概念) を理解したい。** → [概念](guide/concepts.md) と [状態機械](guide/state-machine.md)
- **スマホから操作したい。** → [Web UI](guide/web-ui.md)
- **通知でスマホに知らせたい。** → [通知](guide/notifications.md)
- **詰まっている挙動を直したい。** → [トラブルシューティング](guide/troubleshooting.md)

## セクション

### Getting started

順を追って手を動かすチュートリアル。

- [1. インストール](getting-started/01-install.md)
- [2. プロジェクトを初期化する](getting-started/02-init-project.md)
- [3. Web UI をセットアップする](getting-started/03-web-ui.md)
- [4. 最初のタスク](getting-started/04-first-task.md)
- [ワークフロー](../workflows.md) — 3 つのワークフロー形 (ローカル merge / 1 executor 1 PR / 1 supervisor 1 PR) を project.yaml テンプレート付きで紹介

### ガイド

概念別の how-to。

- [概念](guide/concepts.md) — task / job / hook / kit / payload / trait など内部用語の解説
- [状態機械](guide/state-machine.md) — `pending → executing → done` (+ `aborted`)
- [Web UI](guide/web-ui.md) — デバイスのペアリング・失効、 Cloudflare Tunnel での公開
- [通知](guide/notifications.md) — `notify.command` の設定、 ntfy / Pushover スクリプト例
- [トラブルシューティング](guide/troubleshooting.md)

### リファレンス

- [`project.yaml` リファレンス](reference/project-yaml.md) — プロジェクト定義ファイルの全フィールド
- [Hook スクリプトプロトコル](reference/hook-contract.md) — hook の入出力契約 (stdin、 環境変数、 `payload_patch.json`、 終了コード等)
- [Payload trait リファレンス](reference/traits.md) — `artifact` / `lifecycle` の構造、状態機械が見ている条件、マージモード
- [CLI リファレンス](reference/cli.md) — 全サブコマンドの役割別一覧 (詳細フラグは `boid <subcommand> --help`)
- [HTTP API リファレンス](reference/http-api.md) — daemon が公開する `/api/*` エンドポイント (UNIX socket / HTTP listener)、 SSE、 エラーフォーマット

### Kit 作者向け

- [概要](kit-authoring/overview.md) — kit のディスク上のレイアウト、 `kit.yaml` 主要フィールド、 hook スクリプトのプロトコル
- 公式 kit 群: [boid-kits](https://github.com/novshi-tech/boid-kits)

### アーキテクチャ

- [概要](architecture/overview.md) — プロセス構成、レイヤと依存方向、主要コンポーネント、 1 アクションのデータの流れ
- [永続化レイヤ](architecture/persistence.md) — SQLite のテーブル一覧、 主要カラム、 JSON カラムの中身、 マイグレーションの扱い
- [サンドボックス内部実装](architecture/sandbox-internals.md) — outer / setup / inner の 3 段スクリプト、 mount / user namespace、 pasta + nftables、 broker と shim の連携、後片付けの安全弁
- [Web UI 内部実装](architecture/web-internals.md) — 認証ミドルウェア、 セッション cookie (HMAC)、ペアリングフロー、CSRF、 Server-Sent Events

### Contributing

- [貢献ガイド](contributing/README.md) — 開発環境、コーディング規約、 PR の出し方、バグ報告
