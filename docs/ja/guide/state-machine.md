# 状態機械

`boid` のすべてのタスクは、共通の状態機械を通過します。 状態機械は 1 つだけで、 behavior ごとに切り替わるわけではありません。 behavior ごとに変わるのは、 各状態で発火する hook の中身です。

このページでは状態・ 遷移・ 遷移条件を網羅します。 語彙が分からない場合は先に [概念](concepts.md) を読んでください。

## 状態

```
                 +--------+    abort / job_failed
                 |aborted | <--------------------+
                 +--------+                      |
                                                 |
   start                                         |
pending -----> executing -----> done             |
                  ^    ^                         |
                  |    | ask                     |
                  |    +------+                  |
                  |           v                  |
                  |       awaiting               |
                  |           |                  |
                  +-- answer -+                  |
```

| 状態 | 意味 |
|---|---|
| `pending` | 作成済み、 未開始 |
| `executing` | hook が主作業中 |
| `awaiting` | エージェントがユーザの回答を待機中 (C2 Q&A モード) |
| `done` | 成功で終端 |
| `aborted` | 失敗で終端 (手動 abort、 job 失敗) |

## 手動遷移

ユーザまたは hook が action として送信します (`boid action send --task <id> --type <action>`)。

| Action | From | To | 備考 |
|---|---|---|---|
| `start` | `pending` | `executing` | |
| `done` | `executing` | `done` | 強制完了 (通常は自動遷移にまかせる) |
| `reopen` | `done` | `executing` | 新しい instruction を append して再開 (`--message` で渡す) |
| `ask` | `executing` | `awaiting` | `boid task notify --ask` が発行。 `answer` が届くまでタスクを停止する |
| `answer` | `awaiting` | `executing` | `boid task answer` または Web UI が発行。 hook が再起動される |
| `abort` | 終端でない任意の状態 | `aborted` | |
| `job_failed` (system) | 終端でない任意の状態 | `aborted` | |

## 自動遷移

自動遷移は payload の変更で発火します。 payload が更新されるたびに、 状態機械はすべてのルールを優先度順に評価し、 最初にマッチしたものでタスクを進めます。

### `executing` から

- `lifecycle.executed` が `true` (= 直近の hook が `boid job done` で正常終了した) → `done`

`lifecycle.executed` は履歴から自動算出される transient な値ではなく、 hook の終了をフックして state machine が評価するだけのフラグです。 一度 done に遷移するとリセットされ、 reopen で executing に戻った場合は再度 hook の完了を待ちます。

## reopen で instruction を追加する

`boid task reopen <id> --message "..."` は done のタスクを再 executing に戻し、 新しい `Instruction` を `Task.Instructions` 配列に append します。 配列の最後の要素が active として扱われ、 agent / model / interactive は前回 active の値を継承します。

```bash
# done のタスクを再開して 「conflict を解消して再 push」 を依頼する
boid task reopen abc-123 --message "merge origin/main で conflict を解消して再 push"
```

reopen で append された instruction は履歴として残り、 過去の active instruction も `Task.Instructions[..]` から参照できます。

## hook

- **hook**: サンドボックス内で実行される behavior の実体。 `executing` 中にのみ発火する。 `boid job done` の終了が `lifecycle.executed = true` を立て、 自動遷移を駆動する

## 動作モード

`boid` の状態機械は behavior に関わらず 1 種類だけです。 タスクの動作の違いは、

- どの hook を behavior に紐付けるか
- 失敗時に reopen を挟むか / 別タスクとして再投入するか

で表現されます。 ハーネス側に「検証ループ」を組み込むのではなく、 失敗の検知と修正方針は agent instruction に書く方針です。

## CLI からの観察

```bash
boid task show <id>              # 現在の status と payload を表示
boid task watch <id>             # status の変化をリアルタイムに追う
boid job list --task <id>        # このタスクで実行された全ジョブを列挙
boid job show <id>               # 1 ジョブの stdout / stderr / 終了コード
```

状態と payload を見れば「いま何が起きているか」が、 ジョブを見れば「拡張パッケージのスクリプトが何をしたか」が分かります。

---

次: [Web UI](web-ui.md)
