---
name: boid-web
description: >
  Web ページを読む / URL の中身を取得する。WebFetch の代替。
  サンドボックス内では WebFetch が無効なため、web ページの取得・閲覧・
  URL の内容確認はこのスキル経由で行う。
  「〜のページを読んで」「URL を確認して」「ドキュメントを調べて」
  「web で〜を調べて」など web 取得が必要なときに使用。
---

# boid-web — Web ページ取得スキル

サンドボックス内では `WebFetch` ツールが無効化されています。
Web ページを読む正式な代替は `boid fetch <url>` コマンド（ホスト仲介 GET → Markdown 変換）です。

## なぜサブエージェントを使うか

生ページをそのまま主エージェントのコンテキストに流すとトークンを大量消費します。
`WebFetch` が低コストで動いていたのは小モデルによる要約のおかげです。
このスキルではその設計を再現します：

- **安いモデル（haiku）のサブエージェント**を spawn する
- サブエージェントが `boid fetch <url>` を実行し、生コンテンツを**自分のコンテキストで消費**する
- 主エージェントには**要約・必要情報だけ**を返す
- 結果：主エージェントのコンテキストに生ページが載らない

## 手順

### 1. サブエージェントを spawn する

`Agent` ツールを使い、`model: "haiku"` を指定します。

```
Agent({
  description: "Fetch and summarize <url>",
  model: "haiku",
  prompt: `
    Run the following command and return a summary of the result:
      boid fetch <url>

    Instructions:
    - Run the Bash command: boid fetch <url>
    - Read the output (Markdown-formatted page content)
    - Extract and return: <what the main agent needs — e.g. "the value of X", "the list of endpoints", "a 3-sentence summary">
    - Do not return the raw page verbatim; summarize or extract only.
  `
})
```

### 2. サブエージェントのプロンプトのポイント

| 項目 | 内容 |
|---|---|
| コマンド | `boid fetch <url>` (Bash ツールで実行) |
| モデル | `haiku`（安い・速い） |
| 返却内容 | 要約 or 必要な情報のみ（生ページ全文は不要） |
| エラー処理 | fetch 失敗時はエラーメッセージをそのまま返す |

### 3. 複数 URL を並行取得する場合

```
parallel([
  () => Agent({ model: "haiku", prompt: "boid fetch <url1> して概要を返せ" }),
  () => Agent({ model: "haiku", prompt: "boid fetch <url2> して概要を返せ" }),
])
```

## 注意

- `boid fetch` はホスト側で実行されます。サンドボックス制約の影響を受けません。
- `WebFetch` ツールは**サンドボックス内で無効**です。直接呼ばないでください。
- 要約精度が重要な場合は `sonnet` を指定しても構いませんが、通常は `haiku` で十分です。
