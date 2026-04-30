# boid ドキュメント

`boid` はパーソナル AI オーケストレータです。依頼から成果物のリリースまでを、事前に定義されたタスクのモデルに沿って AI エージェントに任せ、エージェントの書き込みはサンドボックスで指定範囲に閉じ込めます。

このページはエントリポイントです。ドキュメントは段階的に整備中で、未着手のページはリンクなしで予定として記載しています。

[English version](../en/README.md)

## 目的別エントリ

- **はじめて使う。** → [インストール](getting-started/01-install.md)
- **モデル (概念) を理解したい。** → [概念](guide/concepts.md) と [状態機械](guide/state-machine.md)
- **スマホから操作したい。** → [Web UI](guide/web-ui.md)
- **詰まっている挙動を直したい。** → [トラブルシューティング](guide/troubleshooting.md)

## セクション

### Getting started

順を追って手を動かすチュートリアル。

- [1. インストール](getting-started/01-install.md)
- [2. 最初のタスク](getting-started/02-first-task.md)
- [3. プロジェクトと拡張パッケージ (kit)](getting-started/03-projects-and-kits.md)
- [4. GitHub PR ベースの開発ワークフロー](getting-started/04-dev-workflow.md)

### ガイド

概念別の how-to。

- [概念](guide/concepts.md) — task / job / hook / gate / kit / payload / trait など内部用語の解説
- [状態機械](guide/state-machine.md) — `executing → verifying → reworking → done`
- [Web UI](guide/web-ui.md) — デバイスのペアリング・失効、 Cloudflare Tunnel での公開
- [トラブルシューティング](guide/troubleshooting.md)

### リファレンス

- [`project.yaml` リファレンス](reference/project-yaml.md) — プロジェクト定義ファイルの全フィールド
- [Handler スクリプトプロトコル](reference/handler-contract.md) — hook / gate の入出力契約 (stdin、 環境変数、 `payload_patch.json`、 終了コード等)
- [Payload trait リファレンス](reference/traits.md) — `artifact` / `tasks` / `verification` / `lifecycle` の構造、状態機械が見ている条件、マージモード
- CLI (`boid start`, `boid task`, `boid job`) — 整備予定。 `boid <subcommand> --help` を当面の参考に

### Kit 作者向け

- [概要](kit-authoring/overview.md) — kit のディスク上のレイアウト、 `kit.yaml` 主要フィールド、 hook / gate スクリプトのプロトコル
- 公式 kit 群: [boid-kits](https://github.com/novshi-tech/boid-kits)

### アーキテクチャ

- [概要](architecture/overview.md) — プロセス構成、レイヤと依存方向、主要コンポーネント、 1 アクションのデータの流れ

### Contributing

- [貢献ガイド](contributing/README.md) — 開発環境、コーディング規約、 PR の出し方、バグ報告
