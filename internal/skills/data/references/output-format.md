# Output Format

## Contents

- [基本](#基本)
- [artifact](#artifact)
- [verification](#verification)
- [tasks](#tasks)
- [ルール](#ルール)

## 基本

結果は `~/.boid/output/payload_patch.yaml` に書き出す。

```yaml
payload_patch:
  artifact: { ... }
```

トップレベルに `payload_patch` キーを置き、その配下に trait 名をキーとして書く。

出力が不要な場合（no-op）はファイルを作成しなくてよい。

## artifact

実装成果物。任意のマッピング。

```yaml
payload_patch:
  artifact:
    summary: "OAuth2 ログイン機能を実装"
    files_changed:
      - auth.go
      - auth_test.go
    commit: abc1234
```

スキーマは自由。実装内容を記述する。
status が `executing` または `collecting_feedback` のとき出力する。

## verification

検証結果。findings 配列を含む。

```yaml
payload_patch:
  verification:
    findings:
      - message: "テストが全て通過"
        status: resolved
      - message: "エラーハンドリング不足"
        status: open
```

- `message`: 指摘内容
- `status`: `open`（要対応）または `resolved`（問題なし）
- status が `verifying` または `in_review` のとき出力する

`source_state` はシステムが自動注入する。含めないこと。

複数エージェントが同時に verification を出力できる（shared trait）。
システムがエージェント ID をキーにして自動的に分離する。

## tasks

サブタスク配列。triage / planning 用途。

```yaml
payload_patch:
  tasks:
    - title: "認証モジュール"
      behavior: dev
      description: "OAuth2 実装。..."
    - title: "テスト追加"
      behavior: dev
      description: "認証のユニットテスト。..."
```

- `title`: タスクのタイトル（必須）
- `behavior`: タスクの実行モデル名（必須）。プロジェクトの `task_behaviors` に定義されたキーを指定する
- `description`: このタスクを実行するエージェントへの指示。何を・どのように実装するかを詳細に記述する（必須）

## ルール

- `instructions` trait は出力に含めない（読み取り専用）
- 一度の実行で出力する trait は通常 1 つ
- artifact と tasks は排他的 trait — 複数エージェントが同時に出力するとエラー
- verification は共有 trait — 複数エージェントが同時に出力可能
