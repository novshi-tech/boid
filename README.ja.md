# boid

**パーソナル AI オーケストレータ。** 成果物の生成からリリースまでのワークフロー全体を、構造化されたタスクとして追跡・自動化します。すべての処理はサンドボックス内で安全に実行され、ローカルで完結します。

[English README](README.md)

## 特徴

- **ローカル / 単一ユーザ。** SQLite ベースの自己完結型 daemon。クラウドアカウント不要、チーム前提の機能なし、共有状態なし
- **構造化されたタスクライフサイクル。** すべてのタスクは `executing → verifying → reworking → done` を通過します。修正ループは payload 上の検証結果で駆動され、場当たり的な再依頼ではありません
- **サンドボックスファースト。** Hook とエージェント実行は既定でサンドボックス内。`host_commands` として宣言されたものだけが host に渡り、ポリシーは kit 単位で管理されます
- **Worktree + PR ワークフロー。** git worktree で並列に開発タスクを動かします。auto-merge と CI 検証は kit として提供され、core は環境非依存のまま
- **Pluggable kits。** Claude Code / Codex / GitHub PR・auto-merge などを [boid-kits](https://github.com/novshi-tech/boid-kits) から再利用、あるいは自作の kit を追加可能
- **TUI / CLI / Web UI。** ターミナルから操作するほか、Cloudflare Tunnel 経由でスマホからもペアリングして使えます

## インストール

```bash
go install github.com/novshi-tech/boid@latest
```

Linux のみ対応。

## クイックスタート

```bash
boid start              # daemon 起動 (自動デタッチ)
boid task list          # タスク一覧
boid task show <id>     # タスク詳細
boid stop               # daemon 停止
```

詳しい手順は [docs/ja/getting-started/01-install.md](docs/ja/getting-started/01-install.md) を参照してください。

## ドキュメント

- **[インストールとクイックスタート](docs/ja/getting-started/01-install.md)**
- **[概念](docs/ja/guide/concepts.md)** — 語彙
- **[状態機械](docs/ja/guide/state-machine.md)**
- **[Web UI](docs/ja/guide/web-ui.md)** — Cloudflare Tunnel 構成を含む
- **[トラブルシューティング](docs/ja/guide/troubleshooting.md)**

ドキュメント全体の目次は [docs/ja/](docs/ja/README.md)、英語版は [docs/en/](docs/en/README.md) にあります。

## ステータス

現在は作者が直接サポートできる範囲で評価中です。広い公開はその後を予定しています。

## ライセンス

[MIT](LICENSE)
