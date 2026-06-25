#!/usr/bin/env bash
set -euo pipefail
PEER_DIR="${E2E_PEER_PROJECT_DIR:?}"
CLONE_DEST="$PWD/peer-clone"
git clone --local "$PEER_DIR" "$CLONE_DEST"
(cd "$CLONE_DEST" && git switch feature)
[[ -f "$CLONE_DEST/feature.txt" ]] || { printf 'FAIL: feature.txt not found after switching to feature branch\n' >&2; exit 1; }
peer_head_ref="$(cat "$PEER_DIR/.git/HEAD" 2>/dev/null || printf '')"
peer_branch="${peer_head_ref#ref: refs/heads/}"
[[ "$peer_branch" == "main" ]] || { printf 'FAIL: peer HEAD changed (was "main", now "%s")\n' "$peer_branch" >&2; exit 1; }
mkdir -p "$HOME/.boid/output"
cat > "$HOME/.boid/output/payload_patch.json" <<'EOF'
{"payload_patch":{"artifact":{"clone_local_succeeded":true}}}
EOF
