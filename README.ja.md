# boid

**パーソナル AI オーケストレータ (Linux 専用)。** 複数の AI コーディングエージェントを並行で走らせるとき、人間がボトルネックにならないようにするためのツールです。エージェントに自律的に進める余地を与え、書き込み範囲を絞れるサンドボックスで安全性を担保し、進行中のタスクを Web UI と TUI でひと目で把握できます。

[English README](README.md)

## 特徴

- **エージェントに自律性を与える。** 「依頼 → 実行 → 検証 → 修正 → 完了」というタスクの流れと、各段階で記録するデータ形式があらかじめ決まっているので、修正ループのたびに人間が状況を再投入しなくても先に進みます。Claude Code, Codex, git, gh, エディタ、言語ツールチェーンなど、手元のコマンドをそのままエージェントに渡せます
- **自律性と安全性をサンドボックスで両立。** エージェントは普段のディレクトリを直接読みますが、書き込みは指定したスコープ (典型的には git worktree) に閉じ込められます。暴走したエージェントもホームディレクトリや他プロジェクトには触れません。タスクごとに別 worktree を切れば、複数の依頼を別ブランチ・別ディレクトリで同時に走らせても作業が衝突しません
- **全タスクの進行をひと目で把握。** すべてのタスクが状態付きで一覧化され、 TUI / CLI / Web UI のどれからでも横断的に確認できます。 Web UI を Cloudflare Tunnel で公開すればスマホからも進捗を確認・操作できます
- **個人のローカル環境で完結。** `go install` でバイナリを入れて `boid start` するだけ。設定ファイルの作成も、サーバ準備も、サインアップも要りません。クラウド側のサンドボックスでは届かない、手元の実環境に直接作用させられます
- **拡張は別パッケージとして差し替え可能。** AI エージェントの種類 (Claude Code / Codex)、 CI 連携、 PR 作成、自動マージなど、用途別の拡張パッケージ ([boid-kits](https://github.com/novshi-tech/boid-kits)) を組み合わせて構成します

## インストール

```bash
go install github.com/novshi-tech/boid@latest
```

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
- **[概念](docs/ja/guide/concepts.md)** — 用語解説
- **[状態機械](docs/ja/guide/state-machine.md)** — C2 Q&A の `awaiting` 状態を含む
- **[C2 フロー](docs/ja/architecture/c2-flow.md)** — 非対話セッション + Q&A アーキテクチャ
- **[Web UI](docs/ja/guide/web-ui.md)** — Cloudflare Tunnel 構成を含む
- **[トラブルシューティング](docs/ja/guide/troubleshooting.md)**

ドキュメント全体の目次は [docs/ja/](docs/ja/README.md)、英語版は [docs/en/](docs/en/README.md) にあります。

## ステータス

現在は作者が直接サポートできる範囲で評価中です。広い公開はその後を予定しています。

## ライセンス

[MIT](LICENSE)
