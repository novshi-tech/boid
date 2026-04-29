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

駆動シグナル: payload に `artifact` または `tasks` が現れたか (両者は対称な「実行完了」シグナル)、 `executing` 由来の finding。

- 実行完了 + `executing` 由来の open finding あり → `reworking`
- 実行完了 + open finding なし → `verifying`
- 実行完了でない + `lifecycle.executed` が true → `done` (作業不要だった)

`artifact` と `tasks` は対称です。 plan タスクが `tasks` を、 dev タスクが `artifact` を書く、という違いがあるだけで、いずれも「executing 終了、レビューへ進む」を意味します。

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

reviewer の hook / gate が payload patch で finding を書き、次の dispatch サイクルで自動遷移ルールが発火します。

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
boid task show <id>              # 現在の status と payload
boid task watch <id>             # status の変化をライブ表示
boid job list --task <id>        # このタスクで動いた job 一覧
boid job show <id>               # 1 ジョブの stdout / stderr / 終了コード等
```

状態と payload で「いま何が起きているか」、 job で「kit が何をしたか」が分かります。

---

次: [Web UI](web-ui.md)
