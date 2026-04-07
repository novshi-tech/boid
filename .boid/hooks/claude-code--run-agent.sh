#!/bin/bash
set -e

# ~/.claude/skills/boid-sandbox が存在しない場合はシンボリックリンクを作成する
SKILLS_SRC="${HOME}/.local/share/boid/skills/boid-sandbox"
SKILLS_LINK="${HOME}/.claude/skills/boid-sandbox"
if [ ! -e "$SKILLS_LINK" ] && [ ! -L "$SKILLS_LINK" ]; then
    mkdir -p "$(dirname "$SKILLS_LINK")"
    ln -s "$SKILLS_SRC" "$SKILLS_LINK"
fi

if [ "${BOID_INTERACTIVE:-0}" = "1" ]; then
    exec claude --dangerously-skip-permissions "/boid-sandbox"
else
    exec claude --dangerously-skip-permissions --verbose --output-format=stream-json --include-partial-messages -p "/boid-sandbox"
fi
