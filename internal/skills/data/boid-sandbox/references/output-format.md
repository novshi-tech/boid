# Output Format

## Contents

- [基本](#基本)
- [artifact](#artifact)
- [サブタスクの生成](#サブタスクの生成)
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

## サブタスクの生成

サブタスクは payload には書かず、`boid task create` builtin を直接呼び出して登録する。
詳細は `/boid-plan` skill を参照。

```bash
boid task create <<EOF
title: 認証モジュール
behavior: dev
description: OAuth2 実装。...
auto_start: true
EOF
```

## ルール

- `instructions` は payload の trait ではないので payload_patch に書かない (読み取り専用、 task root の `instructions.yaml` から取得)
- 一度の実行で出力する trait は通常 1 つ
- artifact は exclusive trait — 後勝ちで上書きされる。 並列 hook で衝突するなら `artifact.<my-handler-id>` のように handler ごとに sub-key を切る
- 修正不可能なエラーは payload に残すのではなく `boid task abort <task_id> --code <reason> --message "<summary>"` で打ち切る
