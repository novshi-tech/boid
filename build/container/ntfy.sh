#!/bin/bash
# build/container/ntfy.sh
#
# ntfy.sh 通知スクリプト。build/container/Dockerfile が
# /usr/local/bin/ntfy.sh に焼き込み、operator は config.yaml の
# notify.command でこれを指す (例:
# `boid config set notify.command '["/usr/local/bin/ntfy.sh","https://ntfy.sh/<topic>"]'`)。
#
# container 移行前は operator のホーム配下 (~/.local/bin/ntfy.sh) に置かれた
# 個人スクリプトで、daemon プロセス自身が PATH 上のそれを exec していた
# (internal/notify/notify.go の Service.Notify)。Phase 6 で daemon が
# compose コンテナ内で動くようになって以降、notify.command のコマンド解決は
# コンテナのファイルシステム上で行われるため、ホスト専用パスのスクリプトは
# 見えなくなり通知が無言で失敗する (exec: file not found)。このスクリプトは
# その移植先: イメージに焼き込むことでコンテナ内 PATH からも解決できる。
#
# ntfy のトピック URL は操作者ごとに異なる秘匿情報寄りの値なのでスクリプト
# 本体には焼き込まず、notify.command の第1引数として config.yaml (daemon
# 側の boid_state named volume に永続化される — イメージにもリポジトリにも
# 残らない) から渡す。
#
# internal/notify.Service.Notify が渡す環境変数 (BOID_TASK_ID / BOID_MESSAGE
# / BOID_TASK_URL / BOID_PROJECT_NAME / BOID_TASK_TITLE) はホスト版と不変。
set -euo pipefail

NTFY_URL="${1:?usage: ntfy.sh <ntfy-topic-url>}"

curl -fsS -H "Title: boid: ${BOID_TASK_ID:0:8}" \
	-H "Click: ${BOID_TASK_URL:-}" \
	-d "${BOID_MESSAGE:-}" \
	"$NTFY_URL"
