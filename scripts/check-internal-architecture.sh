#!/usr/bin/env bash

set -euo pipefail

MODE="${1:-current}"
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INTERNAL_DIR="$ROOT_DIR/internal"

current_allowed=(
  api
  client
  db
  dispatcher
  hostcmd
  kit
  orchestrator
  project
  projectspec
  sandbox
  secret
  server
)

target_allowed=(
  api
  client
  dispatcher
  orchestrator
  sandbox
  server
)

list_top_level_packages() {
  find "$INTERNAL_DIR" -mindepth 1 -maxdepth 1 -type d -printf '%f\n' | sort
}

contains() {
  local needle="$1"
  shift
  local item
  for item in "$@"; do
    if [[ "$item" == "$needle" ]]; then
      return 0
    fi
  done
  return 1
}

check_allowed_set() {
  local -n allowed_ref=$1
  local failed=0
  local packages=()
  mapfile -t packages < <(list_top_level_packages)

  echo "mode: $MODE"
  echo "internal packages:"
  printf '  %s\n' "${packages[@]}"

  local pkg
  for pkg in "${packages[@]}"; do
    if ! contains "$pkg" "${allowed_ref[@]}"; then
      echo "unexpected package: internal/$pkg" >&2
      failed=1
    fi
  done

  if [[ "$MODE" == "target" ]]; then
    local required
    for required in "${allowed_ref[@]}"; do
      if ! contains "$required" "${packages[@]}"; then
        echo "missing target package: internal/$required" >&2
        failed=1
      fi
    done
  fi

  return "$failed"
}

check_empty_project_dir() {
  local project_dir="$INTERNAL_DIR/project"
  if [[ ! -d "$project_dir" ]]; then
    echo "missing directory: internal/project" >&2
    return 1
  fi

  if find "$project_dir" -mindepth 1 -print -quit | grep -q .; then
    echo "expected empty directory: internal/project" >&2
    return 1
  fi

  echo "confirmed empty directory: internal/project"
}

check_import_graph() {
  echo "go list internal imports:"
  go list -f '{{.ImportPath}}|{{join .Imports ","}}' ./internal/... | sort
}

case "$MODE" in
  current)
    check_allowed_set current_allowed
    check_empty_project_dir
    check_import_graph
    ;;
  target)
    check_allowed_set target_allowed
    check_import_graph
    ;;
  imports)
    check_import_graph
    ;;
  *)
    echo "usage: $0 [current|target|imports]" >&2
    exit 2
    ;;
esac
