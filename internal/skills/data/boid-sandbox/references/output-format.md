# Output Format

## Contents

- [基本](#基本)
- [artifact](#artifact)
- [tasks](#tasks)
- [ルール](#ルール)

## 基本

結果は `~/.boid/output/payload_patch.json` に **JSON** で書き出す。

```json
{
  "payload_patch": {
    "artifact": { }
  }
}
```

トップレベルに `payload_patch` キーを置き、その配下に trait 名をキーとして書く。

出力が不要な場合（no-op）はファイルを作成しなくてよい。

JSON 限定: YAML 1.1 implicit type 変換 (`on:`/`yes:` が boolean に化ける等) や非 string キー
による parse 失敗を避けるため、payload_patch は JSON で書くこと。

## artifact

実装成果物。任意のマッピング。

```json
{
  "payload_patch": {
    "artifact": {
      "summary": "OAuth2 ログイン機能を実装",
      "files_changed": ["auth.go", "auth_test.go"],
      "commit": "abc1234",
      "pr_url": "https://github.com/owner/repo/pull/123"
    }
  }
}
```

スキーマは自由。実装内容を記述する。
status が `executing` のときに出力する。

## tasks

サブタスク配列。triage / planning 用途。

```json
{
  "payload_patch": {
    "tasks": [
      {
        "title": "認証モジュール",
        "behavior": "dev",
        "description": "OAuth2 実装。...",
        "auto_start": true
      },
      {
        "title": "テスト追加",
        "behavior": "dev",
        "description": "認証のユニットテスト。..."
      }
    ]
  }
}
```

- `title`: タスクのタイトル（必須）
- `behavior`: タスクの実行モデル名（必須）。プロジェクトの `task_behaviors` に定義されたキーを指定する
- `description`: このタスクを実行するエージェントへの指示。何を・どのように実装するかを詳細に記述する（必須）
- `auto_start`: bool（デフォルト: false）。true にするとタスク作成直後に自動で `start` アクションが発火され、即座に実行が開始される

## ルール

- `instructions` は payload の trait ではないので payload_patch に書かない (読み取り専用、 task root の `instructions.yaml` から取得)
- 一度の実行で出力する trait は通常 1 つ
- artifact と tasks は exclusive trait — 後勝ちで上書きされる。 並列 hook で衝突するなら `artifact.<my-handler-id>` のように handler ごとに sub-key を切る
- 修正不可能なエラーは payload に残すのではなく `boid task abort <task_id> --code <reason> --message "<summary>"` で打ち切る
