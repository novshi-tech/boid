# 状態機械

`boid` のすべてのタスクは、共通の状態機械を通過します。状態機械は 1 つだけで、 behavior ごとに切り替わるわけではありません。 behavior ごとに変わるのは、各状態で発火する hook / gate の中身です。

このページでは状態・遷移・遷移条件を網羅します。語彙が分からない場合は先に [概念](concepts.md) を読んでください。

## 状態

```
                 +--------+    abort / job_failed / fatal finding
                 |aborted | <-----------------------------------+
                 +--------+                                     |
                                                                |
   start                                                        |
pending -----> executing -----> verifying -----> done           |
                  ^   |             |  ^                        |
                  |   |             |  |                        |
                  |   v             v  |                        |
                  +- reworking <----+  |                        |
                       |               |                        |
                       +---------------+                        |
                                                                |
                                       reopen                   |
                                       <-----                   |
                                       (done -> reworking)      |
```

| 状態 | 意味 |
|---|---|
| `pending` | 作成済み、未開始 |
| `executing` | hook が主作業中 |
| `verifying` | reviewer hook / gate が結果を検証中 |
| `reworking` | finding を直す必要がある状態。 executing 側の hook が再実行される |
| `done` | 成功で終端 |
| `aborted` | 失敗で終端 (手動 abort、 fatal finding、 rework 上限超過、 job 失敗) |

## 手動遷移

ユーザまたは handler が action として送信します (`boid action send --task <id> --type <action>`)。

| Action | From | To |
|---|---|---|
| `start` | `pending` | `executing` |
| `done` | `executing` / `verifying` / `reworking` | `done` |
| `reopen` | `done` | `reworking` |
| `abort` | 終端でない任意の状態 | `aborted` |
| `job_failed` (system) | 終端でない任意の状態 | `aborted` |

## 自動遷移

自動遷移は payload の変更で発火します。 payload が更新されるたびに、状態機械はすべてのルールを優先度順に評価し、最初にマッチしたものでタスクを進めます。

### Abort (最優先)

任意の状態から自動発火します。

- いずれかの finding が `severity=fatal` かつ `status=open` → `aborted`
- `reworking` 中に `lifecycle.rework_count` が設定上限を超える → `aborted` (上限は `~/.config/boid/config.yaml` の `state_machine.rework_limit` で変更可、既定 5)

### `executing` から

判定に使う材料は次の 2 つです:

- payload に `artifact` または `tasks` のいずれかが書かれているか — どちらも「executing で出すべき成果が出揃った」ことを表します
- 状態 `executing` で書かれた open finding が残っているか

これらの組み合わせで遷移が決まります。

- 成果が出揃った + `executing` で書かれた open finding あり → `reworking`
- 成果が出揃った + open finding なし → `verifying`
- 成果は出ていないが `lifecycle.executed` が true → `done` (実行はしたが直すべき成果物がなかった)

`artifact` と `tasks` は役割が対称で、 plan 系のタスクが `tasks` を、 dev 系のタスクが `artifact` を書きます。書く trait が違うだけで、いずれも「executing が終わったので検証へ進める」という同じ意味を持ちます。

### `verifying` から

- `verifying` 由来の open finding あり → `reworking`
- open finding なし → `done` (verification gate がなければそのまま素通り)

### `reworking` から

- `reworking` 由来の finding がすべて resolved → `verifying` (検証に再入場)
- `reworking` 由来の open finding が残っている → `reworking` のまま (解消まで自己ループ)

reworking の退場判定は `reworking` 由来の finding のみを見ます。 `verifying` 由来の open finding (例: `mergeable-check`) は reworking 退場をブロックせず、 verifying 再入場時に同じ gate が再実行されて評価し直されます。

## finding がループを駆動する

finding は `verification.findings` 内のオブジェクトで、以下を持ちます。

- `state` — 発生した状態 (`executing` / `verifying` / `reworking`)
- `status` — `open` または `resolved`
- `severity` — `info` (既定) / `warning` / `error` / `fatal` (open の `fatal` があれば即 abort)
- `message` — rework hook が読む自由形式テキスト

レビュー系の hook / gate が payload patch で finding を書き込み、それを契機に daemon が状態遷移ルールを再評価して自動遷移が発火します。

## rework 上限と abort

`reworking → aborted` は `lifecycle.rework_count` が設定上限を超えると発火します。既定は 5。 `~/.config/boid/config.yaml` で上書き:

```yaml
state_machine:
  rework_limit: 10
```

暴走 rework ループに対する安全装置です。 abort 理由には `code=rework_limit_exceeded` が記録されるので、他の失敗と区別できます。

## 動作モード: one-shot と feedback-loop

behavior の `transition` フィールドが rework の使われ方を切り替えます。

- **one-shot** — `executing → verifying → done`。 verifier が finding を書けば 1 度だけ `reworking` に戻って再試行。 「これ 1 つやる」型に向く
- **feedback-loop** — 同じ状態機械だが、 `reworking ↔ verifying` を複数回まわす想定。 PR レビューと CI を通す変更に向く

状態機械自体は同一です。違いはどの kit と handler を behavior に紐付けるか。

## CLI からの観察

```bash
boid task show <id>              # 現在の status と payload を表示
boid task watch <id>             # status の変化をリアルタイムに追う
boid job list --task <id>        # このタスクで実行された全ジョブを列挙
boid job show <id>               # 1 ジョブの stdout / stderr / 終了コード
```

状態と payload を見れば「いま何が起きているか」が、ジョブを見れば「拡張パッケージのスクリプトが何をしたか」が分かります。

---

次: [Web UI](web-ui.md)
