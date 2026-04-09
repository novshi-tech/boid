---
name: boid-e2e
description: boid プロジェクトで新機能開発時に E2E テストをセットで作成するためのガイド。
  シナリオ作成手順・テンプレート・テスト設計ガイドラインを提供する。
  新機能実装タスクで「e2eテストも作りたい」「シナリオを追加したい」場合に使用する。
---

# boid E2E テスト作成スキル

新機能に E2E テストをセットで作成するための手順書。
既存シナリオのサンプルとして `e2e/scenarios/project-smoke`（最もシンプル）を参照すること。

## クイックスタート

```
1. e2e/scenarios/<scenario-name>/ ディレクトリを作成する
2. workspace/app/.boid/project.yaml を配置する
3. scenario.sh を作成する
4. （サンドボックス必要な場合）requires-sandbox を配置する
5. ./e2e/run.sh <scenario-name> で動作確認（ホスト上のみ実行可能）
```

## 詳細リファレンス

- [E2E インフラ概要](references/infrastructure.md) — run.sh の実行フロー・ヘルパー関数・環境変数
- [シナリオ作成テンプレート](references/scenario-template.md) — ディレクトリ構成・project.yaml・scenario.sh のテンプレート
- [テスト設計ガイドライン](references/design-guidelines.md) — 何をテストするか・アサーション・待機パターン・fake コマンド

## チェックリスト

新しいシナリオを作成したら以下を確認する:

- [ ] `e2e/scenarios/<name>/scenario.sh` が作成されているか
- [ ] `e2e/scenarios/<name>/workspace/app/.boid/project.yaml` が正しい構成か
- [ ] 正常系のアサーション（`e2e_assert_contains`）が少なくとも 1 つあるか
- [ ] サンドボックスが必要な場合 `requires-sandbox` ファイルがあるか
- [ ] fixture kit（`e2e/fixtures/kits/`）が必要な場合追加されているか
- [ ] ユニットテストが壊れていないか（`go test ./...` で確認）
- [ ] 既存シナリオを壊していないか（CI に任せる、または `./e2e/run.sh` で全シナリオ実行）

## インストール

このスキルを Claude Code で使えるようにするには:

```bash
cp -r docs/skills/boid-e2e ~/.claude/skills/
```

インストール後、`/boid-e2e` コマンドで呼び出せるようになる。
