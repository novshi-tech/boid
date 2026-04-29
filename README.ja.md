# boid

**パーソナル AI オーケストレータ (Linux 専用)。** 依頼から成果物のリリースまでを 1 つのタスクとして追跡し、 AI エージェントに任せます。エージェントはローカルマシンのファイルシステムを直接読み書きできるので、普段使っているコマンドや開発環境をそのまま扱えます。書き込みはサンドボックスで指定範囲に閉じ込められるため、自分のファイルが壊れる心配なく動かせます。

[English README](README.md)

## 特徴

- **手元のツールをそのまま使える。** Claude Code, Codex, git, gh, エディタ、言語ツールチェーンなど、すでにインストール済みのコマンドをエージェントに渡せます。クラウド側のサンドボックスでは届かない、自分の実環境への作用が可能です
- **インストール後すぐに使い始められる。** `go install` でバイナリを入れて `boid start` するだけ。設定ファイルの作成も、サーバ準備も、サインアップも要りません
- **タスクのモデルが事前に定義されている。** 「依頼 → 実行 → 検証 → 修正 → 完了」の流れと、各段階で記録するデータ形式があらかじめ決まっています。修正ループのたびに人間が状況を再投入しなくても進みます
- **ローカルファイルを書き換える範囲をサンドボックスで絞れる。** エージェントは普段のディレクトリを直接読みますが、書き込みは git worktree など指定したスコープに閉じ込められます。逆走したエージェントもホームディレクトリや他プロジェクトには触れられません
- **複数の依頼を並行して進められる。** タスクごとに専用の git worktree を切るので、同時に動いても互いに干渉しません
- **拡張は別パッケージとして差し替え可能。** AI エージェントの種類 (Claude Code / Codex)、 CI 連携、 PR 作成、自動マージなど、用途別の拡張パッケージ ([boid-kits](https://github.com/novshi-tech/boid-kits)) を組み合わせて構成します
- **ターミナルとブラウザの両方から操作できる。** TUI / CLI に加え、 Web UI を Cloudflare Tunnel で公開すればスマホからも操作できます

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
- **[状態機械](docs/ja/guide/state-machine.md)**
- **[Web UI](docs/ja/guide/web-ui.md)** — Cloudflare Tunnel 構成を含む
- **[トラブルシューティング](docs/ja/guide/troubleshooting.md)**

ドキュメント全体の目次は [docs/ja/](docs/ja/README.md)、英語版は [docs/en/](docs/en/README.md) にあります。

## ステータス

現在は作者が直接サポートできる範囲で評価中です。広い公開はその後を予定しています。

## ライセンス

[MIT](LICENSE)
