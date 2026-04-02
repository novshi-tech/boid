#!/usr/bin/env bash
set -euo pipefail

e2e_log() {
  printf '[e2e] %s\n' "$*"
}

e2e_fail() {
  printf '[e2e] ERROR: %s\n' "$*" >&2
  exit 1
}

e2e_require_cmd() {
  local cmd="$1"
  command -v "$cmd" >/dev/null 2>&1 || e2e_fail "required command not found: $cmd"
}

e2e_require_sandbox_prereqs() {
  e2e_require_cmd tmux
  e2e_require_cmd pasta
  e2e_require_cmd unshare
  e2e_require_cmd nft

  if ! unshare --user --mount --map-root-user -- true >/dev/null 2>&1; then
    e2e_fail "sandbox prerequisite check failed: unshare --user --mount --map-root-user"
  fi
}

e2e_assert_contains() {
  local haystack="$1"
  local needle="$2"
  if [[ "$haystack" != *"$needle"* ]]; then
    printf '[e2e] ERROR: expected output to contain %q\n' "$needle" >&2
    printf '%s\n' "$haystack" >&2
    exit 1
  fi
}

e2e_run() {
  e2e_log "run: $*"
  "$@"
}
