# Notifications

`boid` does not have its own notification-firing logic. It only invokes the `notify.command` configured in `~/.config/boid/config.yaml` when an agent explicitly calls `boid task notify <id> --message "..."`.

The main use case is **supervisor agent decision branching and up-front approval**: when the agent reaches a point where it cannot proceed without a human judgment call, it sends a push notification to let you know. You check the session in the Web UI and reply.

## Configuration

Add `notify.command` to `~/.config/boid/config.yaml`:

```yaml
notify:
  command:
    - /home/you/bin/boid-notify.sh
```

`command` is a `[]string`. The first element is the executable and the rest are additional arguments. It is spawned directly via `exec.CommandContext`, not through a shell, so shell expansions (e.g. `~`) do not apply.

If the setting is empty or `config.yaml` does not exist, `boid task notify` returns HTTP 501 and the notification is silently skipped. Task execution is unaffected.

## Environment variables passed to the script

The notification script receives the following environment variables:

| Variable | Contents |
|---|---|
| `BOID_TASK_ID` | Task UUID |
| `BOID_TASK_TITLE` | Task title |
| `BOID_PROJECT_ID` | Project UUID |
| `BOID_PROJECT_NAME` | Project name (best-effort; empty if unavailable) |
| `BOID_MESSAGE` | Text the agent passed with `--message` |
| `BOID_TASK_URL` | Task detail URL (`<public_url>/tasks/<id>` if `web.public_url` is set, otherwise empty) |

The authoritative source is the `Notify` function in `internal/notify/notify.go`.

## How agents call it

An agent calls the command like this:

```bash
boid task notify ${BOID_TASK_ID} --message "Need a decision on how to apply PR #42 review feedback"
```

Notify is only called in **interactive mode** (`BOID_INTERACTIVE=1`). In autonomous mode the agent writes the situation to the task artifact and exits instead.

Immediately after calling notify, the agent outputs the question body (options, context, decision criteria) to the session and waits for your reply. You read it in the Web UI session viewer and respond there.

For the full calling policy, see the "いつ notify を呼ぶか" (when to call notify) section in [`/boid-supervisor` SKILL.md](../../../internal/skills/data/boid-supervisor/SKILL.md).

## Script example 1: ntfy.sh

[ntfy](https://ntfy.sh) is a simple push notification service that supports both self-hosted and public instances.

```sh
#!/usr/bin/env bash
# boid-notify-ntfy.sh — send a notification via ntfy.sh
# Use a long random string as the topic — do not keep the placeholder below.
set -euo pipefail
TOPIC="boid-XXXXXXXX-replace-me"
curl -fsS \
  -H "Title: ${BOID_TASK_TITLE:-boid task}" \
  -H "Click: ${BOID_TASK_URL:-https://ntfy.sh}" \
  -d "${BOID_MESSAGE}" \
  "https://ntfy.sh/${TOPIC}" >/dev/null
```

Place the script at `/home/you/bin/boid-notify-ntfy.sh`, make it executable, and wire it in `config.yaml`:

```yaml
notify:
  command:
    - /home/you/bin/boid-notify-ntfy.sh
```

The `Click` header carries `BOID_TASK_URL`, so tapping the notification opens the task detail in the Web UI directly. `BOID_TASK_URL` is only populated after you run `boid web set-url` — see [Web UI](web-ui.md).

Subscribe to `https://ntfy.sh/<topic>` in the ntfy app on your phone (iOS / Android). When using the public server, pick a long random topic name that is hard to guess.

## Script example 2: Pushover

[Pushover](https://pushover.net) delivers rich push notifications and requires a User Key and an Application Token (one-time $5 per user after a free trial).

```sh
#!/usr/bin/env bash
# boid-notify-pushover.sh — send a notification via Pushover
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

`PUSHOVER_USER` and `PUSHOVER_TOKEN` cannot be passed through `notify.command`, so they must be available in the environment where the `boid` daemon runs. Common approaches are an `EnvironmentFile=` in the systemd unit, or an `export` in your shell profile that is sourced before `boid start`.

## Integration with the magic link

Once you run `boid web set-url https://boid.example.com`, `BOID_TASK_URL` becomes `https://boid.example.com/tasks/<id>`. Tapping a notification then takes you straight to the task detail in the Web UI. If you plan to drive `boid` from a phone, set the public URL first.

See [Web UI](web-ui.md#access-from-another-device) for the setup steps.

---

Next: [Troubleshooting](troubleshooting.md)
