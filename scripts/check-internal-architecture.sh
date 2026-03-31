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
  orchestrator
  sandbox
  server
)

target_allowed=(
  api
  client
  db
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
    echo "confirmed missing directory: internal/project"
    return 0
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

check_forbidden_imports() {
  local failed=0
  local line path imports

  while IFS='|' read -r path imports; do
    case "$path" in
      github.com/novshi-tech/boid/internal/api)
        if [[ "$imports" == *"github.com/novshi-tech/boid/internal/db"* ]]; then
          echo "forbidden import: internal/api -> internal/db" >&2
          failed=1
        fi
        if [[ "$imports" == *"github.com/novshi-tech/boid/internal/server"* ]]; then
          echo "forbidden import: internal/api -> internal/server" >&2
          failed=1
        fi
        ;;
      github.com/novshi-tech/boid/internal/orchestrator)
        if [[ "$imports" == *"github.com/novshi-tech/boid/internal/api"* ]]; then
          echo "forbidden import: internal/orchestrator -> internal/api" >&2
          failed=1
        fi
        if [[ "$imports" == *"github.com/novshi-tech/boid/internal/server"* ]]; then
          echo "forbidden import: internal/orchestrator -> internal/server" >&2
          failed=1
        fi
        ;;
      github.com/novshi-tech/boid/internal/dispatcher)
        if [[ "$imports" == *"github.com/novshi-tech/boid/internal/api"* ]]; then
          echo "forbidden import: internal/dispatcher -> internal/api" >&2
          failed=1
        fi
        if [[ "$imports" == *"github.com/novshi-tech/boid/internal/server"* ]]; then
          echo "forbidden import: internal/dispatcher -> internal/server" >&2
          failed=1
        fi
        ;;
      github.com/novshi-tech/boid/internal/db)
        if [[ "$imports" == *"github.com/novshi-tech/boid/internal/"* ]]; then
          echo "forbidden import: internal/db should not depend on other internal packages" >&2
          failed=1
        fi
        ;;
      github.com/novshi-tech/boid/internal/client)
        if [[ "$imports" == *"github.com/novshi-tech/boid/internal/"* ]]; then
          echo "forbidden import: internal/client should not depend on other internal packages" >&2
          failed=1
        fi
        ;;
    esac
  done < <(go list -f '{{.ImportPath}}|{{join .Imports ","}}' ./internal/...)

  return "$failed"
}

case "$MODE" in
  current)
    check_allowed_set current_allowed
    check_empty_project_dir
    check_import_graph
    check_forbidden_imports
    ;;
  target)
    check_allowed_set target_allowed
    check_import_graph
    check_forbidden_imports
    ;;
  imports)
    check_import_graph
    ;;
  *)
    echo "usage: $0 [current|target|imports]" >&2
    exit 2
    ;;
esac
