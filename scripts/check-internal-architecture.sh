#!/usr/bin/env bash

set -euo pipefail

MODE="${1:-current}"
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INTERNAL_DIR="$ROOT_DIR/internal"

current_allowed=(
  adapters
  api
  apigateway
  apiwire
  atomicfile
  client
  config
  daemon
  db
  dispatcher
  dockerres
  gitgateway
  gwtransport
  humanize
  initwizard
  install
  logrotate
  mtls
  notify
  orchestrator
  profiles
  qrterm
  reap
  sandbox
  selfuser
  server
  skills
  timeline
  version
  yamlutil
)

target_allowed=(
  adapters
  api
  apigateway
  apiwire
  atomicfile
  client
  config
  daemon
  db
  dispatcher
  dockerres
  gitgateway
  gwtransport
  humanize
  initwizard
  install
  logrotate
  mtls
  notify
  orchestrator
  profiles
  qrterm
  reap
  sandbox
  selfuser
  server
  skills
  timeline
  version
  yamlutil
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

# import_present reports (via exit status) whether comma-joined import list
# "$2" contains the exact import path "github.com/novshi-tech/boid/internal/$1"
# — an EXACT element match, not a substring test. A plain `[[ "$imports" ==
# *"internal/api"* ]]` substring check would false-positive on
# "internal/apigateway" (docs/plans/api-gateway.md PR1 introduced that
# package name, and internal/dispatcher legitimately imports it) since
# "internal/apigateway" contains "internal/api" as a literal prefix
# substring. Splitting on commas and comparing whole elements is what makes
# "api" and "apigateway" distinguishable.
import_present() {
  local needle="github.com/novshi-tech/boid/internal/$1"
  local imports="$2"
  local imp
  local IFS=','
  for imp in $imports; do
    if [[ "$imp" == "$needle" ]]; then
      return 0
    fi
  done
  return 1
}

# any_internal_import_present is import_present's "any package at all"
# counterpart, used by the internal/db and internal/dockerres leaf checks
# below (which forbid every internal/* import, not one specific package).
# A prefix test is correct here (unlike import_present's exact-match): every
# genuine internal package import path DOES start with
# "github.com/novshi-tech/boid/internal/", so there is no analogous
# "apigateway looks like a substring of api" collision to guard against.
any_internal_import_present() {
  local prefix="github.com/novshi-tech/boid/internal/"
  local imports="$1"
  local imp
  local IFS=','
  for imp in $imports; do
    if [[ "$imp" == "$prefix"* ]]; then
      return 0
    fi
  done
  return 1
}

# check_client_portability_deps は internal/client の api 依存禁止を
# **推移的に** 検査する。check_forbidden_imports の各ルールは go list の
# .Imports、すなわち直接 import しか見ていないため、client -> X -> api の
# 経路が素通りする (レビューで実証済み: internal/humanize 経由の中継を
# 挟むと script は exit 0 のまま、GOOS=windows のビルドだけが落ちた)。
#
# この 1 本だけ推移的にするのは、ここが「ビルドが壊れる境界」そのもの
# だから (docs/plans/windows-client-build.md)。他のルール (server/db/
# dispatcher/sandbox) は直接 import 禁止のままで足りる — それらは client
# が要する型を持たず、経由して型を借りる動機が無い。
#
# なお internal/db は現在も client の推移的依存に含まれる (config →
# gitgateway/apigateway 経由)。windows でビルドできているので移植性の
# 問題ではなく、ここで db を推移的に禁止することはできない。
check_client_portability_deps() {
  local dep
  for dep in $(go list -deps ./internal/client 2>/dev/null); do
    if [[ "$dep" == "github.com/novshi-tech/boid/internal/api" ]]; then
      echo "forbidden transitive dependency: internal/client -> ... -> internal/api" >&2
      echo "  (go list -deps ./internal/client で経路を確認すること。" >&2
      echo "   internal/api は daemon の handler パッケージなので、経由 import であっても" >&2
      echo "   dispatcher/db/sandbox/skills をクライアントのコンパイル経路に載せ、" >&2
      echo "   GOOS=windows / darwin のビルドを壊す)" >&2
      return 1
    fi
  done
  return 0
}

check_forbidden_imports() {
  local failed=0
  local line path imports

  while IFS='|' read -r path imports; do
    case "$path" in
      github.com/novshi-tech/boid/internal/api)
        if import_present db "$imports"; then
          echo "forbidden import: internal/api -> internal/db" >&2
          failed=1
        fi
        if import_present server "$imports"; then
          echo "forbidden import: internal/api -> internal/server" >&2
          failed=1
        fi
        ;;
      github.com/novshi-tech/boid/internal/orchestrator)
        if import_present api "$imports"; then
          echo "forbidden import: internal/orchestrator -> internal/api" >&2
          failed=1
        fi
        if import_present server "$imports"; then
          echo "forbidden import: internal/orchestrator -> internal/server" >&2
          failed=1
        fi
        ;;
      github.com/novshi-tech/boid/internal/dispatcher)
        if import_present api "$imports"; then
          echo "forbidden import: internal/dispatcher -> internal/api" >&2
          failed=1
        fi
        if import_present server "$imports"; then
          echo "forbidden import: internal/dispatcher -> internal/server" >&2
          failed=1
        fi
        ;;
      github.com/novshi-tech/boid/internal/db)
        if any_internal_import_present "$imports"; then
          echo "forbidden import: internal/db should not depend on other internal packages" >&2
          failed=1
        fi
        ;;
      github.com/novshi-tech/boid/internal/dockerres)
        # dockerres (docs/plans/workspace-home-volume-persistence.md PR1) は
        # boid が docker リソースに付ける名前と label の単一出典。存在理由が
        # 「互いを import できない 3 パッケージ (dispatcher / reap /
        # dockerproxy) が同じ文字列に合意する」ことなので、leaf であることが
        # 機能要件そのものである。ここで内部依存が 1 本でも生えると
        # internal/reap が cmd から import できなくなり (= daemon が起動不能な
        # 状態でも動く `boid reap` の契約が壊れ)、逃げ道として各パッケージが
        # 文字列を再び手打ちで複製する — prefix の drift が workspace の
        # 認証情報消失に直結する、まさにこの package が潰した失敗モード。
        if any_internal_import_present "$imports"; then
          echo "forbidden import: internal/dockerres must stay a leaf (no other internal packages)" >&2
          failed=1
        fi
        ;;
      github.com/novshi-tech/boid/internal/gitgateway)
        # gitgateway (docs/plans/git-gateway-cutover.md PR3) is a standalone,
        # inert reverse-proxy package. It must not pull in the sqlite-backed
        # internal/db (directly or via dispatcher/api/server), so a sandbox
        # test run — which cannot build internal/db — can still build and
        # test this package. Secret resolution and notification are
        # expressed as small function/interface seams instead of importing
        # internal/dispatcher.SecretStore directly.
        for forbidden in db dispatcher api server sandbox; do
          if import_present "$forbidden" "$imports"; then
            echo "forbidden import: internal/gitgateway -> internal/$forbidden" >&2
            failed=1
          fi
        done
        ;;
      github.com/novshi-tech/boid/internal/apigateway)
        # apigateway (docs/plans/api-gateway.md PR1) mirrors gitgateway's own
        # leaf-package layering rule exactly, for the identical reason: it
        # must not pull in the sqlite-backed internal/db (directly or via
        # dispatcher/api/server/sandbox), so a sandbox test run — which
        # cannot build internal/db — can still build and test this package.
        # Secret resolution and notification/recording are expressed as
        # small function/interface seams instead of importing
        # internal/dispatcher.SecretStore directly.
        for forbidden in db dispatcher api server sandbox; do
          if import_present "$forbidden" "$imports"; then
            echo "forbidden import: internal/apigateway -> internal/$forbidden" >&2
            failed=1
          fi
        done
        ;;
      github.com/novshi-tech/boid/internal/gwtransport)
        # gwtransport は gitgateway / apigateway が共有する outbound
        # http.Transport の単一出典 (fix: 半死 upstream 接続の使い回し)。
        # leaf であることが機能要件そのもの: 上の gitgateway / apigateway
        # ルールは go list の .Imports、つまり **直接 import しか見ていない**
        # ため、gwtransport が internal/db 等を引くと両 gateway の
        # 「sandbox でビルド・テストできる (= sqlite に依存しない)」契約が
        # 推移経路で素通りに壊れる。ここを leaf に固定することがその再発ゲート。
        if any_internal_import_present "$imports"; then
          echo "forbidden import: internal/gwtransport must stay a leaf (no other internal packages)" >&2
          failed=1
        fi
        ;;
      github.com/novshi-tech/boid/internal/client)
        # client は daemon の HTTP API を叩く薄いフロント。不変条件は:
        #   - 型の共有は許可: api の DTO / orchestrator のドメイン型を import して
        #     リクエスト/レスポンスをマーシャルするのは正当 (バケツリレー回避)。
        #   - 振る舞いへの直接依存は禁止: client は必ず API を叩く。内部パッケージの
        #     関数/挙動をローカルで呼んではならない。
        # import グラフでは「型 import」と「振る舞い呼び出し」を区別できないため、
        # ここでは振る舞いのみを持つ backend 層 (server/db/dispatcher/sandbox) の
        # import を hard ban する (これらは client が要する型を持たない)。
        # orchestrator 内の振る舞い関数の呼び出し禁止は識別子単位の静的解析が要り、
        # 本スクリプトの粒度では担保できない (レビュー/規約で補完)。
        #
        # api も hard ban に追加した (docs/plans/windows-client-build.md)。
        # 「型の共有は許可」という上の原則自体は変わらないが、共有先が
        # internal/api から internal/apiwire に移った — api は daemon の
        # handler パッケージなので、DTO 1 個のために import すると
        # dispatcher / db / sandbox / skills / web/templates までクライアントの
        # コンパイル経路に載り、GOOS=windows / darwin のビルドが壊れる。
        # これが実際に起きていた状態であり、この 1 行がその再発ゲート。
        for forbidden in server db dispatcher sandbox api; do
          if import_present "$forbidden" "$imports"; then
            echo "forbidden import: internal/client -> internal/$forbidden" >&2
            failed=1
          fi
        done
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
    check_client_portability_deps
    ;;
  target)
    check_allowed_set target_allowed
    check_import_graph
    check_forbidden_imports
    check_client_portability_deps
    ;;
  imports)
    check_import_graph
    ;;
  *)
    echo "usage: $0 [current|target|imports]" >&2
    exit 2
    ;;
esac
