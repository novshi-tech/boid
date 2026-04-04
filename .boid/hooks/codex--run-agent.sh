#!/bin/bash
set -euo pipefail

# ~/.codex/skills/boid-sandbox が存在しない場合はシンボリックリンクを作成する
SKILLS_SRC="${HOME}/.local/share/boid/skills/boid-sandbox"
SKILLS_LINK="${HOME}/.codex/skills/boid-sandbox"
if [ ! -e "$SKILLS_LINK" ] && [ ! -L "$SKILLS_LINK" ]; then
    mkdir -p "$(dirname "$SKILLS_LINK")"
    ln -s "$SKILLS_SRC" "$SKILLS_LINK"
fi

# Keep boid hook stdout machine-readable; send Codex output to stderr.
echo '/boid-sandbox' | codex exec --sandbox danger-full-access --skip-git-repo-check - 1>&2
