#!/usr/bin/env bash
set -euo pipefail

# docs/plans/git-gateway-cutover.md PR7b — smoke test for the post-cutover
# "clone → commit → push が gateway 経由で通る" contract. The hook runs
# inside a fresh sandbox-internal clone under /workspace/<name> (workspace
# 親化リファクタリング). Its origin remote was set at clone time to the
# gateway URL that embeds this job's token, so `git push origin HEAD` here
# exercises the full gateway → upstream authenticate-and-forward path.
#
# We can't rely on a repo-wide user.name/user.email because the sandbox
# is a fresh clone with no host-side ~/.gitconfig — set it locally.
git config user.name  "Boid E2E Push"
git config user.email "e2e-push@boid.test"

printf 'pushed from sandbox at %s\n' "$(date -Ins)" > gateway-push.txt
git add gateway-push.txt
git commit -q -m "gateway push smoke test"

# HEAD is on the task's branch which the runner already checked out
# (BuildCloneDeclaration: root task ⇒ CheckoutOnly on base_branch, i.e.
# "main" here). `origin HEAD` pushes to the same-named branch upstream.
git push -q origin HEAD

pushed_commit="$(git rev-parse HEAD)"
pushed_branch="$(git rev-parse --abbrev-ref HEAD)"

mkdir -p "$HOME/.boid/output"
cat > "$HOME/.boid/output/payload_patch.json" <<EOF
{"payload_patch":{"artifact":{"source":"gateway-push","pushed_commit":"${pushed_commit}","pushed_branch":"${pushed_branch}"}}}
EOF
