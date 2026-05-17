# Payload trait リファレンス

タスクの payload に置けるキー (trait) と、それぞれが状態遷移に与える影響をまとめたリファレンスです。

[概念 / payload と trait](../guide/concepts.md#payload-と-trait) で短く紹介していますが、 このページでは正規仕様 (定義済み trait の一覧、 マージモード、 状態機械が見ている条件) を網羅します。

## 前提

- payload はタスクが進む過程で蓄積される JSON ドキュメント
- payload のトップレベルキーが trait
- どの trait を hook が読み書きしてよいかは [`kit.yaml`](../kit-authoring/overview.md) の `traits.consumes` / `traits.produces` で宣言する
- 値の更新は hook が出力する payload patch (`payload_patch` ラッパ) を通じて行う ([Hook スクリプトプロトコル](hook-contract.md))

## 定義済みの trait

`boid` の payload で扱える trait は `artifact` のみです。 状態機械の自動遷移は payload trait を直接見ず、 hook の終了 (`boid job done`) だけで駆動されます。

| Trait | 書き込み可能 | 内容 |
|---|---|---|
| `artifact` | hook が produce | 実装系タスクが残す成果物 (commit / PR URL / 変更ファイル等) を格納する自由形マップ |

### `artifact`

実行 hook が成果物を書く先。 構造はプロジェクト / kit によって自由ですが、 `artifact.children.*` は `boid` 本体が予約しており、 hook が書こうとするとエラーになります (親タスクから子タスクの状態を参照するためのビュー)。

### サブタスクの生成

統括系の behavior (`supervisor`) は payload trait に書く形ではなく、 hook から `boid task create` builtin を直接呼び出してサブタスクを登録します。 詳細は [`/boid-supervisor` SKILL](../../../internal/skills/data/boid-supervisor/SKILL.md) を参照してください。

## 自動算出される値

### `lifecycle`

タスクの履歴から自動的に算出される値で、 状態遷移の判定にだけ使われます。 **payload には保存されず**、 状態機械の評価時にだけ仮想的に注入されます。

参照可能なフィールド:

| フィールド | 型 | 内容 |
|---|---|---|
| `lifecycle.abort.code` | string | abort 時の理由コード |
| `lifecycle.abort.message` | string | abort 時の人間可読メッセージ |

hook から `lifecycle` を書き込む payload patch を出すと、 自動算出に上書きされて意味を成しません。 hook の `traits.produces` に `lifecycle` を含める意味はありません。

### 予約キー

- **`artifact.children.*`** — 親タスクが子タスクの状態を参照するためのビュー領域。 `boid` 本体が computed に解決するため、 hook が直接書き込もうとするとエラーになります

## payload trait ではないもの

### `instructions`

instructions は payload の trait ではなく、 タスクの top-level フィールド (`Task.Instructions` 配列) に保持されます。 配列の最後の要素が active な指示で、 `boid task reopen <id> --message "..."` で append されます。

instructions の構造は [`project.yaml` リファレンス / Instruction](project-yaml.md#instruction) を参照してください。

## マージモード

hook が出した payload patch をどう merge するかは trait ごとに決まっています。

| Trait | モード | 意味 |
|---|---|---|
| `artifact` / 任意のキー | **exclusive** | 後勝ち。 hook が書いた値で base の同一キーを置き換える |

複数 hook が並走する場合、 `artifact.<my-hook-id>` のように hook ごとに独立した sub-key を使うことで衝突を避けます。

## hook 側の宣言

[`kit.yaml`](../kit-authoring/overview.md) の `traits` で、 hook が読み書きする trait を宣言します。

```yaml
hooks:
  - id: my-hook
    traits:
      consumes: [artifact?]   # 読みたい値 (TaskJSON 経由で渡される)
      produces: [artifact]    # 書きたい値 (これ以外を payload patch に書くと drop される)
```

### `?` サフィックスによる optional 宣言

`consumes` の末尾に `?` を付けると、 その trait が無くてもエラーにせず hook を動かせます。

```yaml
traits:
  consumes: [artifact?]
```

`?` は `consumes` のみで意味を持ちます (`produces` には付けません)。

### produces にない trait は drop

hook が `produces` に宣言していない trait を payload patch に書いても、 `boid` 本体は警告ログを出して **その trait だけ捨てます**。 patch 全体は drop されません。

## 関連ドキュメント

- [概念 / payload と trait](../guide/concepts.md#payload-と-trait) — 短い紹介
- [状態機械](../guide/state-machine.md) — hook 完了が遷移をどう駆動するか
- [Hook スクリプトプロトコル](hook-contract.md) — payload patch の出し方
- [Kit 作者向け 概要](../kit-authoring/overview.md) — `traits.consumes` / `produces` の宣言
- [`project.yaml` リファレンス / Instruction](project-yaml.md#instruction) — `instructions` の構造
