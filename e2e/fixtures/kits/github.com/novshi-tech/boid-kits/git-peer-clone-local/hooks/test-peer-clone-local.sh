#!/usr/bin/env bash
set -euo pipefail

PEER_DIR="${E2E_PEER_PROJECT_DIR:?}"
# Dest must be within the worktree root. For worktree tasks, the sandbox cwd
# IS the worktree dir, so $PWD/peer-clone is guaranteed to be within it.
CLONE_DEST="$PWD/peer-clone"

# Clone the peer project using the brokered git clone --local op.
# The broker validates that source is a workspace peer and dest is in the worktree.
git clone --local "$PEER_DIR" "$CLONE_DEST"

# Switch to the feature branch in the clone (peer itself is untouched).
# Use a subshell + cd to avoid the denied -C global option.
(cd "$CLONE_DEST" && git switch feature)

# Verify the feature-branch file exists.
[[ -f "$CLONE_DEST/feature.txt" ]] || {
    printf 'FAIL: feature.txt not found after switching to feature branch\n' >&2
    exit 1
}

# Verify the peer's HEAD is still on main (peer is read-only and unmodified).
# Read the HEAD file directly to avoid needing git -C on the peer dir.
peer_head_ref="$(cat "$PEER_DIR/.git/HEAD" 2>/dev/null || printf '')"
peer_branch="${peer_head_ref#ref: refs/heads/}"
[[ "$peer_branch" == "main" ]] || {
    printf 'FAIL: peer HEAD changed (was "main", now "%s")\n' "$peer_branch" >&2
    exit 1
}

mkdir -p "$HOME/.boid/output"
cat > "$HOME/.boid/output/payload_patch.json" <<'EOF'
{"payload_patch":{"artifact":{"clone_local_succeeded":true}}}
EOF
