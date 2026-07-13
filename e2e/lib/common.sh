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

e2e_wait_for_file() {
  local path="$1"
  local timeout="${2:-10}"
  local interval="${3:-0.05}"
  local deadline=$((SECONDS + timeout))

  while [[ ! -f "$path" ]]; do
    if (( SECONDS >= deadline )); then
      e2e_fail "timed out waiting for file: $path"
    fi
    sleep "$interval"
  done
}

# e2e_setup_fixture_upstream <workspace_dir>
#
# docs/plans/git-gateway-cutover.md PR7a: gives every scenario project a
# real, reachable git origin *before* `boid project add` runs, so that the
# post-cutover world ("origin の無い project は登録拒否", PR2) is already
# true during e2e today, instead of relying only on the fake host git
# shim's hardcoded https://example.invalid/e2e/fake-repo.git placeholder
# (e2e/fixtures/hostbin/git). That placeholder is left completely untouched
# by this function — it still governs what `boid project add` actually
# captures as upstream_url, since the boid daemon resolves `git` via $PATH
# and the shim is first on it (see the fake git script's own comment).
# What this function changes is that the project directories themselves
# gain a real remote pointing at a real, running server, which is the part
# that matters once cutover (PR6) starts actually cloning from origin.
#
# Starts one fixture upstream HTTP server (e2e/upstream, bare repos served
# via `git http-backend`) per scenario invocation, scoped to the scenario's
# own $E2E_ROOT so it shuts down with everything else (matches this
# harness's existing full per-scenario isolation: fresh tmpdir, fresh
# daemon, ...).
#
# For every subdirectory of workspace_dir containing .boid/project.yaml,
# this creates a bare repo on the fixture server named after the
# directory's basename, git-inits the directory for real if it is not
# already a repo, commits its current contents, and pushes to the fixture
# origin. Real git is always invoked via its absolute path (/usr/bin/git)
# rather than through $PATH — this bypasses the fake host git shim, which
# is for the boid *daemon's* own git invocations, not this harness-level
# setup.
#
# E2E_FIXTURE_UPSTREAM_OWNER prefixes every fixture repo path with a fixed
# synthetic "owner" segment (http://host:port/<owner>/<repo>.git) instead of
# the flat http://host:port/<repo>.git this function originally produced.
# The flat form has exactly two URL path segments (host + repo); the git
# gateway's repoKeyFromUpstreamURL (internal/dispatcher/gitgateway_wire.go)
# is deliberately GitHub/Bitbucket-shaped and requires exactly three
# (host/owner/repo — see its doc comment), so every fixture-seeded project's
# upstream_url has always failed that parse ("does not resolve to
# host/owner/repo") since PR6 started requiring a resolvable gatewayCloneURL
# for every project-visible dispatch. That failure was silently masked by a
# separate run.sh bug (fixed alongside this one) that swallowed a failing
# scenario's exit status, so it went unnoticed since PR6 merged — see
# docs/plans/git-gateway-cutover.md and PR #735's discussion for the full
# trail. Adding the owner segment here is the fix: git-http-backend serves
# nested repo paths natively (GIT_PROJECT_ROOT-relative), so this needs no
# change on the serving side (e2e/upstream), only here and in the
# `upstream-serve` positional repo names below, which must match.
readonly E2E_FIXTURE_UPSTREAM_OWNER="e2e-fixture"

e2e_setup_fixture_upstream() {
  local workspace_dir="$1"

  local project_dirs=()
  while IFS= read -r -d '' project_yaml; do
    project_dirs+=("$(dirname "$(dirname "$project_yaml")")")
  done < <(find "$workspace_dir" -mindepth 3 -maxdepth 3 -name project.yaml -print0 | sort -z)

  if [[ ${#project_dirs[@]} -eq 0 ]]; then
    return 0
  fi

  local upstream_dir="$E2E_ROOT/upstream-repos"
  local ready_file="$E2E_ROOT/upstream.addr"
  local cert_file="$E2E_ROOT/upstream.crt"
  mkdir -p "$upstream_dir"

  local repo_names=()
  local project_dir repo_name
  for project_dir in "${project_dirs[@]}"; do
    repo_names+=("${E2E_FIXTURE_UPSTREAM_OWNER}/$(basename "$project_dir")")
  done

  e2e_log "starting fixture upstream server for: ${repo_names[*]}"
  "$E2E_BIN_DIR/boid-e2e" upstream-serve \
    --dir "$upstream_dir" \
    --ready-file "$ready_file" \
    --cert-file "$cert_file" \
    "${repo_names[@]}" \
    >"$E2E_LOG_DIR/upstream.stdout.log" \
    2>"$E2E_LOG_DIR/upstream.stderr.log" &
  E2E_UPSTREAM_PID=$!

  # --cert-file is written by upstream-serve strictly before --ready-file
  # (see e2e/cmd/boid-e2e/main.go's runUpstreamServe doc comment), so once
  # the ready-file wait below returns, cert_file is guaranteed to exist too —
  # no separate wait needed for it.
  e2e_wait_for_file "$ready_file" 10
  local upstream_addr
  upstream_addr="$(cat "$ready_file")"
  e2e_log "fixture upstream server listening on $upstream_addr (TLS, cert=$cert_file)"
  # docs/plans/git-gateway-cutover.md PR6 cutover (PR7a Opus heads-up):
  # export the resolved addr so PR7b's new scenarios (gateway-clone-based
  # assertions) can reach the fixture upstream directly without re-deriving
  # it from the ready file themselves.
  export E2E_UPSTREAM_ADDR="$upstream_addr"

  # Trust the fixture's self-signed certificate (docs/plans/git-gateway-cutover.md
  # PR #736 follow-up): the git gateway's outbound transport defaults every
  # unconfigured host to https (internal/gitgateway/credentials.go's
  # CredentialProvider.SchemeFor — deliberately production-correct, left
  # untouched), so the fixture now serves real TLS instead of plain HTTP (see
  # e2e/upstream/upstream.go's New doc comment). SSL_CERT_FILE is exported so
  # the boid daemon (started later in run.sh, inheriting this shell's
  # environment) trusts it via Go's default x509 cert pool — zero gateway/
  # production code changes. GIT_SSL_CAINFO covers the harness's own `git
  # push` below, which talks to the fixture directly as a real git client
  # (not through boid at all).
  export SSL_CERT_FILE="$cert_file"
  export GIT_SSL_CAINFO="$cert_file"

  for project_dir in "${project_dirs[@]}"; do
    repo_name="$(basename "$project_dir")"
    local origin_url="https://${upstream_addr}/${E2E_FIXTURE_UPSTREAM_OWNER}/${repo_name}.git"
    (
      cd "$project_dir"
      if [[ ! -d .git ]]; then
        /usr/bin/git init -q -b main
        /usr/bin/git config user.name "E2E Fixture"
        /usr/bin/git config user.email "e2e-fixture@boid.test"
      fi
      /usr/bin/git add -A
      /usr/bin/git commit -q -m "e2e fixture upstream seed" --allow-empty
      /usr/bin/git remote add origin "$origin_url"
      /usr/bin/git push -q -u origin HEAD
    )
  done
}
