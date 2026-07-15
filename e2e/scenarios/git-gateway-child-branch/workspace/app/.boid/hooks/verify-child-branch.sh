#!/usr/bin/env bash
set -euo pipefail

# branch-policy-simplification Phase 1 (v0.0.11) 向けの child 側 hook。
# 子 task の sandbox は BuildCloneDeclaration の CheckoutOnly 経路で
# 起動される — root task と全く同じ扱いで、per-task branch も fork point
# も存在しない:
#   - Branch          = BOID_BASE_BRANCH ("main"、 parent から継承)
#   - CheckoutOnly    = true  (runner が checkout -B main origin/main)
#
# よってこの hook が観測すべき事実:
#   (a) 現在の HEAD branch 名が BOID_BASE_BRANCH ("main") そのものである
#       (別ブランチを新規作成しない)
#   (b) parent-marker.txt が clone に含まれている
#       (parent の push が origin/main に届いていて、child の fresh clone が
#        それを直接 fetch していることの proof)
#   (c) HEAD の親コミット (== origin/main tip) の rev が parent の push
#       と一致していれば理想だが、artifact 経由の受渡しは複雑なので
#       (b) のファイル存在で代替する (retired worktree scenario と同じ
#        「観測可能な副作用で pin する」路線)

current_branch="$(git rev-parse --abbrev-ref HEAD)"
expected_branch="${BOID_BASE_BRANCH:-main}"

branch_matches_base_branch=false
if [[ "$current_branch" == "$expected_branch" ]]; then
  branch_matches_base_branch=true
fi

parent_marker_present=false
if [[ -f parent-marker.txt ]]; then
  parent_marker_present=true
fi

# HEAD is "main" itself (no separate branch); git-log against just HEAD
# should include parent's commit since child directly cloned the same branch
# parent already pushed to.
parent_commit_in_log=false
if git log --format='%H' HEAD | grep -q .; then
  # find the commit that touched parent-marker.txt
  parent_marker_commit="$(git log --format='%H' HEAD -- parent-marker.txt | head -1)"
  if [[ -n "$parent_marker_commit" ]]; then
    parent_commit_in_log=true
  fi
fi

if [[ "$branch_matches_base_branch" != "true" ]] \
   || [[ "$parent_marker_present" != "true" ]] \
   || [[ "$parent_commit_in_log" != "true" ]]; then
  printf 'FAIL: child branch resolution did not behave as expected\n' >&2
  printf '  current_branch=%s expected_branch=%s\n' "$current_branch" "$expected_branch" >&2
  printf '  branch_matches_base_branch=%s parent_marker_present=%s parent_commit_in_log=%s\n' \
    "$branch_matches_base_branch" "$parent_marker_present" "$parent_commit_in_log" >&2
  git log --oneline -5 HEAD >&2 || true
  ls -la >&2 || true
  exit 1
fi

mkdir -p "$HOME/.boid/output"
cat > "$HOME/.boid/output/payload_patch.json" <<EOF
{"payload_patch":{"artifact":{"source":"verify-child-branch","current_branch":"${current_branch}","branch_matches_base_branch":true,"parent_marker_present":true,"parent_commit_in_log":true}}}
EOF
