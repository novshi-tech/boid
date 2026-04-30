# Payload trait リファレンス

タスクの payload に置けるキー (trait) と、それぞれが状態遷移に与える影響をまとめたリファレンスです。

[概念 / payload と trait](../guide/concepts.md#payload-と-trait) で短く紹介していますが、このページでは正規仕様 (定義済み trait の一覧、マージモード、状態機械が見ている条件) を網羅します。

## 前提

- payload はタスクが進む過程で蓄積される JSON ドキュメント
- payload のトップレベルキーが trait
- どの trait を hook / gate が読み書きしてよいかは [`kit.yaml`](../kit-authoring/overview.md) の `traits.consumes` / `traits.produces` で宣言する
- 値の更新は handler が出力する payload patch (`payload_patch` ラッパ) を通じて行う ([Handler スクリプトプロトコル](handler-contract.md))

## 定義済みの trait

`boid` 本体が状態遷移ルールで参照する trait は次の 3 つです。

| Trait | 書き込み可能 | 状態機械が見るもの |
|---|---|---|
| `artifact` | hook / gate が produce | キーの存在 (executing 完了の合図) |
| `tasks` | hook / gate が produce | 非空配列か (executing 完了の合図、 plan 系) |
| `verification` | hook / gate が produce (shared モード) | 各サブキーの `findings` と `source_state` |

`artifact` と `tasks` は対称で、 「`executing` で出すべき成果が出揃った」 を表します。

### `artifact`

実行 hook が成果物を書く先。 構造はプロジェクト / kit によって自由ですが、 `artifact.children.*` は `boid` 本体が予約しており、handler が書こうとするとエラーになります。

状態機械側の判定:

- `artifact` が payload に存在する (= null でない、キーがある) → 「executing で生成すべき成果が出揃った」 判定材料
- 結合: `artifact` または `tasks` のいずれかが立てば executing 完了

### `tasks`

`artifact` と対称な役割を持ち、計画系のタスク (例: `plan` behavior) が使います。値は配列で、空でなければ完了とみなされます。

状態機械側の判定:

- `tasks` が **非空配列** であれば完了シグナル
- 空配列 / null は完了とみなさない (`artifact` は null でなければ存在判定なのに対し、 `tasks` は要素数を見る)

### `verification`

レビュー系の hook / gate が指摘 (finding) を書く先。マージモードが **shared** で、 hook / gate が出した payload patch は handler ID をキーにして自動的にラップされます。

最終的な payload 上の構造:

```json
{
  "verification": {
    "<handler-id>": {
      "source_state": "executing",
      "findings": [
        {
          "message": "...",
          "status": "open",
          "severity": "fatal"
        }
      ]
    },
    "<another-handler-id>": {
      "source_state": "verifying",
      "findings": [...]
    }
  }
}
```

各サブキーが 1 つの handler の書き込み区画です。 handler が同じパッチを次に出したときは、自分のサブキーだけが上書きされ、他 handler の書き込みは保持されます (これが shared モードの意味)。

handler が patch として書き出すときの形は次のとおりです (handler ID によるラップは内部で自動付与されるため、 handler 側は意識しません):

```json
{
  "payload_patch": {
    "verification": {
      "findings": [...]
    }
  }
}
```

`source_state` も同様に内部で自動付与されます。 `boid` の coordinator が、 patch を適用するときの **タスクの現在の status** (例: `executing` / `verifying` / `reworking`) を `source_state` フィールドとして書き加えます。

#### finding の構造

`findings` 配列の各要素は次のフィールドを持ちます。

| キー | 型 | 役割 |
|---|---|---|
| `message` | string | 自由形式の指摘内容。修正系 hook が読む |
| `status` | string | `open` (未解決) または `resolved` (解決済み) |
| `severity` | string | `normal` (既定) または `fatal`。 open の `fatal` があると即時 `aborted` |

#### 状態機械の判定

verifying / reworking の自動遷移は、 `verification` 全体ではなく **`source_state` が一致するサブキーだけ** を見て判定します。具体的には:

- `executing` 由来の open finding → `executing → reworking`
- `verifying` 由来の open finding → `verifying → reworking`
- `reworking` 由来の open finding がすべて resolved → `reworking → verifying`
- 任意の状態で `severity=fatal` の open finding → 即 `aborted`

これにより、 `verifying` で書かれた gate finding (例: `mergeable-check`) と、 `reworking` で書き直された agent finding が混在しても、 reworking 退場判定はあくまで `reworking` 由来分だけを見るので、 verifying 側の finding がデッドロックを生むことはありません。

## 自動算出される値

### `lifecycle`

タスクの履歴から自動的に算出される値で、状態遷移の判定にだけ使われます。 **payload には保存されず**、状態機械の評価時にだけ仮想的に注入されます。

参照可能なフィールド:

| フィールド | 型 | 内容 |
|---|---|---|
| `lifecycle.rework_count` | int | `reworking` に入った回数。 abort 上限の判定に使用 |
| `lifecycle.executed` | bool | `executing` で hook が 1 度でも実行されたか |

handler から `lifecycle` を書き込む payload patch を出すと、自動算出に上書きされて意味を成しません。 hook の `traits.produces` に `lifecycle` を含める意味はありません。

### 予約キー

- **`artifact.children.*`** — 親タスクが子タスクの状態を参照するためのビュー領域。 `boid` 本体が computed に解決するため、 handler が直接書き込もうとするとエラーになります

## payload trait ではないもの

### `instructions`

過去には payload の中に instructions を入れる構成でしたが、現在は **タスクの top-level フィールド** (`Task.instructions`) として独立しており、 payload には含まれません。 payload 内に `instructions` キーを書こうとするとエラーになります。

instructions の構造は [`project.yaml` リファレンス / Instruction](project-yaml.md#instruction) を参照してください。

## マージモード

handler が出した payload patch をどう merge するかは trait ごとに決まっています。

| Trait | モード | 意味 |
|---|---|---|
| `verification` | **shared** | handler ID をキーにしてラップ。複数 handler の書き込みが各々のサブキーに残る |
| `artifact` / `tasks` / 任意のキー | **exclusive** | 後勝ち。 handler が書いた値で base の同一キーを置き換える |

shared モードは、 同じ trait を複数 handler が並行して書いて互いに上書きしないようにするための仕組みです。 verification はレビュアー系 handler が複数になることを想定して shared にしてあります。

## handler 側の宣言

[`kit.yaml`](../kit-authoring/overview.md) の `traits` で、 hook / gate が読み書きする trait を宣言します。

```yaml
hooks:
  - id: my-hook
    on: [executing]
    traits:
      consumes: [instructions]      # 読みたい値 (TaskJSON 経由で渡される)
      produces: [artifact]          # 書きたい値 (これ以外を payload patch に書くと drop される)
```

### `?` サフィックスによる optional 宣言

`consumes` の末尾に `?` を付けると、その trait が無くてもエラーにせず handler を動かせます。

```yaml
traits:
  consumes: [artifact?, verification?]
```

`?` は `consumes` のみで意味を持ちます (`produces` には付けません)。

### produces にない trait は drop

handler が `produces` に宣言していない trait を payload patch に書いても、 `boid` 本体は警告ログを出して **その trait だけ捨てます**。 patch 全体は drop されません。

## 関連ドキュメント

- [概念 / payload と trait](../guide/concepts.md#payload-と-trait) — 短い紹介
- [状態機械](../guide/state-machine.md) — どの trait がどの遷移を駆動するか
- [Handler スクリプトプロトコル](handler-contract.md) — payload patch の出し方
- [Kit 作者向け 概要](../kit-authoring/overview.md) — `traits.consumes` / `produces` の宣言
- [`project.yaml` リファレンス / Instruction](project-yaml.md#instruction) — `instructions` の構造
