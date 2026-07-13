#!/usr/bin/env bash
set -euo pipefail

# docs/plans/git-gateway-cutover.md PR7b — 新規シナリオ 5/5:
# 「許可外 repo への push/fetch が gateway で 403」の検証。
#
# gateway の authorization は internal/gitgateway/server.go の
# ServeHTTP に集中している:
#   - token 無効: 401
#   - token 有効だが repo/operation が許可集合に無い: 403
# このシナリオでは self (registered) の job token を使って、boid に
# 未登録の別 repo (workspace/unauthorized/ dir — harness は bare
# upstream を seed するが、scenario.sh は `boid project add` しない)
# にアクセスすると 403 になることを、fetch (ls-remote) と push の
# 両方で pin する。
#
# self の origin URL を組み替えて unauthorized の gateway URL を作る:
#   http://10.0.2.2:PORT/j/TOKEN/HOST/OWNER/app.git
#                             ↑ この部分を                ↓ 差し替え
#                             `unauthorized.git`
# fixture upstream の owner segment は e2e_setup_fixture_upstream の
# E2E_FIXTURE_UPSTREAM_OWNER (= "e2e-fixture") で固定。

self_origin_url="$(git remote get-url origin)"
if [[ -z "$self_origin_url" ]]; then
  printf 'FAIL: self clone has no origin URL — clone wiring broken\n' >&2
  exit 1
fi

# repo tail should be "app.git"; replace it with "unauthorized.git" while
# keeping the /j/<token>/<host>/<owner>/ prefix intact.
unauthorized_url="${self_origin_url%/app.git}/unauthorized.git"
if [[ "$unauthorized_url" == "$self_origin_url" ]]; then
  printf 'FAIL: could not derive unauthorized URL from self origin: %s\n' "$self_origin_url" >&2
  exit 1
fi

# Redact the token for logging (the URL literally embeds the job token).
redacted_url="$(printf '%s' "$unauthorized_url" | sed -E 's#/j/[^/]+/#/j/<redacted>/#')"

# --- 1. fetch attempt via ls-remote (GET /info/refs?service=git-upload-pack)
fetch_stderr_file="$(mktemp)"
if git ls-remote "$unauthorized_url" 2>"$fetch_stderr_file"; then
  printf 'FAIL: ls-remote against unauthorized repo unexpectedly succeeded (%s)\n' "$redacted_url" >&2
  cat "$fetch_stderr_file" >&2
  exit 1
fi
fetch_stderr="$(cat "$fetch_stderr_file")"
rm -f "$fetch_stderr_file"
if ! printf '%s' "$fetch_stderr" | grep -q '403'; then
  printf 'FAIL: fetch was denied but not with HTTP 403 — probable wiring bug\n' >&2
  printf '%s\n' "$fetch_stderr" >&2
  exit 1
fi

# --- 2. push attempt (GET /info/refs?service=git-receive-pack then
#       POST /git-receive-pack). git push fails on the very first HTTP
#       call for the same reason — the token has no permission entry for
#       this repo key at all, so both endpoints are denied.
git config user.name  "Boid E2E Forbidden"
git config user.email "e2e-forbidden@boid.test"
printf 'noop for push attempt\n' > forbidden-attempt.txt
git add forbidden-attempt.txt
git commit -q -m "forbidden repo push attempt"

push_stderr_file="$(mktemp)"
if git push "$unauthorized_url" HEAD:refs/heads/main 2>"$push_stderr_file"; then
  printf 'FAIL: push to unauthorized repo unexpectedly succeeded (%s)\n' "$redacted_url" >&2
  cat "$push_stderr_file" >&2
  exit 1
fi
push_stderr="$(cat "$push_stderr_file")"
rm -f "$push_stderr_file"
if ! printf '%s' "$push_stderr" | grep -q '403'; then
  printf 'FAIL: push was denied but not with HTTP 403 — probable wiring bug\n' >&2
  printf '%s\n' "$push_stderr" >&2
  exit 1
fi

# --- 3. Control: fetch on the authorized (self) origin still works, to
#       distinguish 403 (repo-specific denial) from a general breakage
#       (e.g. gateway crashed, egress blocked, token expired).
if ! git fetch -q origin main; then
  printf 'FAIL: control git fetch origin main failed — probable general gateway wiring bug rather than 403 policy\n' >&2
  exit 1
fi

mkdir -p "$HOME/.boid/output"
cat > "$HOME/.boid/output/payload_patch.json" <<EOF
{"payload_patch":{"artifact":{"source":"forbidden-repo","fetch_denied":true,"push_denied":true,"control_fetch_ok":true}}}
EOF
