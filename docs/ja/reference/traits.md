# Payload trait リファレンス

タスクの payload に置けるキー (trait) と、それぞれが状態遷移に与える影響をまとめたリファレンスです。

[概念 / payload と trait](../guide/concepts.md#payload-と-trait) で短く紹介していますが、 このページでは正規仕様 (定義済み trait の一覧、 マージモード、 状態機械が見ている条件) を網羅します。

## 前提

- payload はタスクが進む過程で蓄積される JSON ドキュメント
- payload のトップレベルキーが trait
- どの trait を hook が読み書きしてよいかは [`project.yaml`](project-yaml.md) の `task_behaviors.<name>.hooks[].traits` (`consumes` / `produces`) で宣言する。 hook は常に `project.yaml` が権威であり、 kit は hook を提供しない (`kit.yaml` に同等の宣言はない)
- 値の更新は hook が出力する payload patch (`payload_patch` ラッパ) を通じて行う ([Hook スクリプトプロトコル](hook-contract.md))

## 定義済みの trait

以下の trait が定義されています。 状態機械の自動遷移は payload trait を直接見ず、 hook の終了 (`boid job done`) だけで駆動されます。

| Trait | 書き込み | マージモード | 内容 |
|---|---|---|---|
| `artifact` | hook が produce | exclusive | 実装系タスクが残す成果物 (commit / PR URL / 変更ファイル等) を格納する自由形マップ |
| `verification` | hook が produce | shared | 検証系 hook の結果。 handler ID 配下の sub-key にマージされる |
| `awaiting` | boid コアが管理 | exclusive | `boid task ask` (blocking RPC) または `boid task notify --ask` で設定される Q&A 状態。 [awaiting trait](#awaiting-trait) を参照 |

### `artifact`

実行 hook が成果物を書く先。 構造はプロジェクト / kit によって自由ですが、 `artifact.children.*` は `boid` 本体が予約しており、 hook が書こうとするとエラーになります (親タスクから子タスクの状態を参照するためのビュー)。

### `verification`

検証ステップを実行する hook が書く先。 `artifact` と異なり、 マージモードは **shared** です。 各 hook が handler-ID sub-key の下に書くことで、 複数の検証 hook が並走しても互いの結果を上書きしません。

### awaiting trait

`boid task ask` (blocking RPC) または `boid task notify --ask` が呼ばれたときに `boid` コアが自動的に設定します。 `boid task ask` 経路では agent が broker 接続を握ったまま回答を待ち、 daemon の in-memory レジストリ経由で回答が直接 agent に届きます。 `notify --ask` 経路は agent が exit した上で `awaiting` に遷移するだけで、 daemon は resume hook を dispatch しません (session-id resume は廃止済)。 実用の Q&A は `boid task ask` を使ってください。

フィールド:

| フィールド | 型 | 設定者 | 役割 |
|---|---|---|---|
| `question` | string | boid コア | ユーザに表示する質問テキスト |
| `question_id` | string | boid コア | この Q&A ターンを識別する UUID |
| `pending_answer` | string | boid コア | レガシー `notify --ask` 経路でユーザの回答を保持していたフィールド。 `boid task ask` 経路では使われません (回答は in-memory で直接配送される) |

`awaiting` トレイトは boid コアと `ApplyAction("ask"/"answer")` のみが管理します。 hook から直接書き込んではいけません。 過去レコードに残っている `session_id` / `mode` フィールドは互換のためデシリアライズは silently 無視されます (構造体からは削除済)。

### サブタスクの生成

統括系の behavior (`supervisor`) は payload trait に書く形ではなく、 hook から `boid task create` builtin を直接呼び出してサブタスクを登録します。 詳細は [`/boid-task` SKILL — Supervisor Mode](../../../internal/skills/data/boid-task/SKILL.md) を参照してください。

## 自動算出される値

### `lifecycle`

タスクの履歴から自動的に算出される値で、 状態遷移の判定にだけ使われます。 **payload には保存されず**、 状態機械の評価時にだけ仮想的に注入されます。

参照可能なフィールド:

| フィールド | 型 | 内容 |
|---|---|---|
| `lifecycle.executed` | bool | 現在のディスパッチサイクルで hook job が正常完了した場合 `true`。 自動遷移ルールの主トリガー |
| `lifecycle.done` | object | 現在の executing サイクルで `boid task notify --done` が呼ばれた場合に設定される。 `message` フィールドを持つ。 `lifecycle.executed` と合わせて `executing→done` 自動遷移を駆動する |
| `lifecycle.fail` | object | 現在の executing サイクルで `boid task notify --fail` が呼ばれた場合に設定される。 `message` フィールドを持つ。 `executing→aborted` 自動遷移を駆動する (`lifecycle.done` より優先) |
| `lifecycle.abort.code` | string | abort 時の理由コード |
| `lifecycle.abort.message` | string | abort 時の人間可読メッセージ |

hook 完了時に評価される自動遷移ルール:

1. `executing→aborted` (`lifecycle.executed && lifecycle.fail` が成立する場合)
2. `executing→done` (`lifecycle.executed && lifecycle.done` が成立する場合)
3. `executing→done` (`lifecycle.executed` のみ、 explicit な notify がないレガシー hook 経路)

hook から `lifecycle` を書き込む payload patch を出すと、 自動算出に上書きされて意味を成しません。 hook の `traits.produces` に `lifecycle` を含める意味はありません。

### 予約キー

- **`artifact.children.*`** — 親タスクが子タスクの状態を参照するためのビュー領域。 `boid` 本体が computed に解決するため、 hook が直接書き込もうとするとエラーになります

## payload trait ではないもの

### `instructions`

instructions は payload の trait ではなく、 タスクの top-level フィールド (`Task.Instructions` 配列) に保持されます。 配列の最後の要素が active な指示で、 `boid task reopen <id> --message "..."` で append されます。

instructions の構造は [`project.yaml` リファレンス / Instruction](project-yaml.md#instruction) を参照してください。

## マージモード

hook が出した payload patch をどう merge するかは trait ごとに決まっています。 3 つのマージモードがあります:

| モード | 意味 |
|---|---|
| **exclusive** | 後勝ち。 hook が書いた値で base の同一キーを置き換える |
| **shared** | handler-ID sub-key 単位でマージする。 複数 hook が並走しても互いを上書きしない |
| **default** | 明示的な上書きがなければ **exclusive** にフォールバック |

trait ごとのマージモード:

| Trait | モード |
|---|---|
| `verification` | **shared** (handler ID sub-key にマージ) |
| `artifact` / 任意のキー | **exclusive** |

複数 hook が並走する場合、 `artifact.<my-hook-id>` のように hook ごとに独立した sub-key を使うことで衝突を避けます。

## hook 側の宣言

[`project.yaml`](project-yaml.md#hooks) の `task_behaviors.<name>.hooks[].traits` で、 hook が読み書きする trait を宣言します。

```yaml
task_behaviors:
  executor:
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
- [`project.yaml` リファレンス / hooks](project-yaml.md#hooks) — `traits.consumes` / `produces` の宣言
- [`project.yaml` リファレンス / Instruction](project-yaml.md#instruction) — `instructions` の構造
