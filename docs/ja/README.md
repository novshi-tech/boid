# boid ドキュメント

`boid` はパーソナル AI オーケストレータです。成果物の生成・検証・リリースまでの一連のワークフローを、サンドボックス内で安全に実行できる構造化タスクとして扱います。

このページはエントリポイントです。ドキュメントは段階的に整備中で、未着手のページはリンクなしで予定として記載しています。

[English version](../en/README.md)

## 目的別エントリ

- **はじめて使う。** → [インストール](getting-started/01-install.md)
- **モデル (概念) を理解したい。** → [概念](guide/concepts.md) と [状態機械](guide/state-machine.md)
- **スマホから操作したい。** → [Web UI](guide/web-ui.md)
- **詰まっている挙動を直したい。** → [トラブルシューティング](guide/troubleshooting.md)
- **過去の設計メモを読みたい。** → [Archive](archive/)

## セクション

### Getting started

順を追って手を動かすチュートリアル。

- [1. インストール](getting-started/01-install.md)
- 2. 最初のタスク — *予定*
- 3. プロジェクトと kit — *予定*
- 4. フィードバックループ — *予定*

### ガイド

概念別の how-to。

- [概念](guide/concepts.md) — task / job / hook / gate / kit / payload / trait
- [状態機械](guide/state-machine.md) — `executing → verifying → reworking → done`
- [Web UI](guide/web-ui.md) — ペアリング、デバイス管理、Cloudflare Tunnel
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

### Archive

- [過去の設計メモと実装計画](archive/)
