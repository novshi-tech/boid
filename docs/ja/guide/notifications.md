# 通知

`boid` は通知の発火ロジックを自身は持ちません。agent が `boid task notify <id> --message "..."` を呼んだときだけ、`~/.config/boid/config.yaml` の `notify.command` を exec する仕組みです。

主な使い方は **supervisor agent (旧 plan agent) による判断分岐 / 事前承認**です。agent が「ユーザの判断なしに進められない」状態になったとき、スマホへのプッシュ通知でユーザに知らせます。通知を受けたユーザは Web UI のセッションビューアで状況を確認し、返答します。

## 設定

`~/.config/boid/config.yaml` に `notify.command` を追加します。

```yaml
notify:
  command:
    - /home/you/bin/boid-notify.sh
```

`command` は `[]string` で、1 番目の要素が実行ファイル、残りは追加引数として渡されます。シェル経由ではなく `exec.CommandContext` で直接 spawn されるため、シェル展開 (`~` の展開等) は効きません。

`notify.command` が空または未設定の場合、通知はスキップされます。タスクの実行には影響しません。 (HTTP 501 経路はデーモンが常に notify サービスを wire するため、通常運用では実質的に到達できません。)

## スクリプトに渡される環境変数

通知スクリプトは以下の環境変数を受け取ります。

| 変数 | 内容 |
|---|---|
| `BOID_TASK_ID` | タスク UUID |
| `BOID_TASK_TITLE` | タスクのタイトル |
| `BOID_PROJECT_ID` | プロジェクト UUID |
| `BOID_PROJECT_NAME` | プロジェクト名 (ベストエフォート、未設定なら空) |
| `BOID_MESSAGE` | agent が `--message` で渡したテキスト |
| `BOID_TASK_URL` | 呼び出しモードに依存する URL (後述)。 `web.public_url` 未設定なら空文字 |

ソースは `internal/notify/notify.go` の `Notify` 関数です。

### notify モード別の `BOID_TASK_URL`

`BOID_TASK_URL` の値は通知の呼び出しモードによって変わります。

| notify モード | `BOID_TASK_URL` の値 |
|---|---|
| `--ask` | `<public_url>/tasks/{id}/questions/{question_id}` |
| `--done` / `--fail` | `<public_url>/tasks/{id}` |
| FYI (ライフサイクルフラグなし) | 実行中 interactive job があれば `<public_url>/jobs/{job_id}`、なければ `<public_url>/tasks/{id}` |

## agent 側からの呼び方

agent は以下のようにコマンドを呼び出します。

```bash
boid task notify ${BOID_TASK_ID} --message "PR #42 のレビュー反映方針を判断してほしい"
```

hook 経由のエージェントセッションは常に PTY 上で対話的に起動されるため、 `BOID_INTERACTIVE` による自律 / 対話の場合分けは不要です。 `boid task notify --ask` を呼べば boid daemon が task を `awaiting` に遷移させ、 ランタイムに **SIGUSR1** を送って停止を要求します。 SIGTERM ではなく SIGUSR1 を使うことで EXIT trap が生きたまま `boid job done` の正常完了を維持できます。 ユーザの回答が届いた時点で daemon が新しいセッションを spawn し、 `$BOID_USER_ANSWER` を介して回答を引き渡します。

notify 直後、agent はセッション内に質問本文 (選択肢・判断材料・context) を出力します。 ユーザは Web UI のセッションビューアで確認し、返答します。

呼び出しポリシーの詳細は [`/boid-task` SKILL.md](../../../internal/skills/data/boid-task/SKILL.md) の Supervisor Mode 内「When to ask (plan approval)」節を参照してください。

## `notify --done` のガード

`boid task notify --done` は `done_request` を記録する前に `verifyDoneClaim` チェックを通ります。 以下の条件のどちらかに該当するとデーモンがリクエストを拒否します (HTTP 409)。

1. **未完了の子タスクがある**: 非終端状態の子タスクが 1 つ以上残っている
2. **リリースコミットが見つからない**: エージェントが報告したコミット SHA がリポジトリに存在しない

これらは幻覚 (confabulation) 防止ガードであり、 実際の作業が完了していないのにタスクを done にしてしまうのを防ぎます。

## スクリプト例 1: ntfy.sh

[ntfy](https://ntfy.sh) はセルフホスト / パブリック双方に対応したシンプルなプッシュ通知サービスです。

```sh
#!/usr/bin/env bash
# boid-notify-ntfy.sh — ntfy.sh への通知
# topic は推測しづらい長い文字列に変更すること
set -euo pipefail
TOPIC="boid-XXXXXXXX-replace-me"
curl -fsS \
  -H "Title: ${BOID_TASK_TITLE:-boid task}" \
  -H "Click: ${BOID_TASK_URL:-https://ntfy.sh}" \
  -d "${BOID_MESSAGE}" \
  "https://ntfy.sh/${TOPIC}" >/dev/null
```

スクリプトを `/home/you/bin/boid-notify-ntfy.sh` に置いて実行権限を付与し、`config.yaml` で指定します。

```yaml
notify:
  command:
    - /home/you/bin/boid-notify-ntfy.sh
```

`Click` ヘッダに `BOID_TASK_URL` を渡すと、通知をタップしたときに Web UI のタスク詳細へ直接飛べます。`BOID_TASK_URL` を使うには [Web UI](web-ui.md) で `boid web set-url` を済ませておく必要があります。

ntfy アプリはスマホ (iOS / Android) から `https://ntfy.sh/<topic>` を subscribe して受け取ります。パブリックサーバを使う場合は topic 名を長いランダム文字列にしてください。

## スクリプト例 2: Pushover

[Pushover](https://pushover.net) はリッチなプッシュ通知を送れる有料サービスです (1 ユーザ $5 の買い切り)。User Key と Application Token が必要です。

```sh
#!/usr/bin/env bash
# boid-notify-pushover.sh — Pushover への通知
set -euo pipefail
: "${PUSHOVER_USER:?PUSHOVER_USER not set}"
: "${PUSHOVER_TOKEN:?PUSHOVER_TOKEN not set}"

curl -fsS https://api.pushover.net/1/messages.json \
  --form-string "token=${PUSHOVER_TOKEN}" \
  --form-string "user=${PUSHOVER_USER}" \
  --form-string "title=${BOID_TASK_TITLE:-boid task}" \
  --form-string "message=${BOID_MESSAGE}" \
  --form-string "url=${BOID_TASK_URL}" \
  --form-string "url_title=Open in boid" >/dev/null
```

`PUSHOVER_USER` / `PUSHOVER_TOKEN` は `notify.command` の引数として渡せないため、ラッパースクリプト内でハードコードするか、boid daemon の実行環境に環境変数として乗せる必要があります。systemd の `EnvironmentFile=` や `.profile` の export で設定するのが一般的です。

## マジックリンクとの連動

`boid web set-url https://boid.example.com` を設定しておくと、`BOID_TASK_URL` が `https://boid.example.com/tasks/<id>` 形式になります。通知からワンタップでタスク詳細に飛べるようになるため、スマホ運用するなら必ず設定してください。

公開 URL の設定手順は [Web UI](web-ui.md#他デバイスから) を参照してください。

---

次: [トラブルシューティング](troubleshooting.md)
