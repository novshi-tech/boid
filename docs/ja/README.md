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
- [4. フィードバックループ](getting-started/04-feedback-loop.md)

### ガイド

概念別の how-to。

- [概念](guide/concepts.md) — task / job / hook / gate / kit / payload / trait など内部用語の解説
- [状態機械](guide/state-machine.md) — `executing → verifying → reworking → done`
- [Web UI](guide/web-ui.md) — デバイスのペアリング・失効、 Cloudflare Tunnel での公開
- [トラブルシューティング](guide/troubleshooting.md)

### リファレンス

安定インタフェースの仕様。整備予定。

- CLI: `boid start`, `boid task`, `boid job`
- `project.yaml` schema

### Kit 作者向け

整備予定。現時点では [boid-kits](https://github.com/novshi-tech/boid-kits) リポジトリが事実上のリファレンスです。

### アーキテクチャ

整備予定。内部実装は [`internal/`](https://github.com/novshi-tech/boid/tree/main/internal) を参照。

### Contributing

整備予定。要点は TDD・最小依存・コミットプレフィックス `feat:` / `fix:` / `refactor:` / `test:`。現在の運用規約は [`CLAUDE.md`](https://github.com/novshi-tech/boid/blob/main/CLAUDE.md) を参照。
