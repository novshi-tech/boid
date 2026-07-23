#!/usr/bin/env bash
set -euo pipefail

# Write a diagnostic payload on failure so the aborted task has debug info.
# Applied immediately via the broker's payload-patch RPC (`boid task update
# --payload-patch @-`, docs/plans/phase5-shim-and-task-context.md decision
# 6/7) — Phase 6 PR8 retired the $HOME/.boid/output/payload_patch.json file
# convention this used to write to (docs/plans/phase6-container-backend.md
# §決定 9).
fail_with_diag() {
  local reason="$1"
  local diag
  diag="DOCKER_HOST=${DOCKER_HOST:-UNSET} DOCKER_PROXY_TEST_CASE=${DOCKER_PROXY_TEST_CASE:-UNSET}"
  printf '{"artifact":{"result":"fail","reason":"%s","diag":"%s"}}\n' \
    "$reason" "$diag" | boid task update --payload-patch @-
  echo "FAIL: $reason ($diag)" >&2
  exit 1
}

# DOCKER_HOST is injected by the proxy: unix:///run/boid/docker-proxy.sock
# Use ${:-} to avoid -u "unbound variable" error when DOCKER_HOST is unset
# (which happens when startDockerProxy failed silently in the daemon).
_dh="${DOCKER_HOST:-}"
if [[ -z "$_dh" ]]; then
  fail_with_diag "DOCKER_HOST is unset (startDockerProxy likely failed)"
fi
if [[ "$_dh" != unix://* ]]; then
  fail_with_diag "DOCKER_HOST is not a unix:// path: $_dh"
fi
DOCKER_SOCK="${_dh#unix://}"

# Send a request through the proxy and return the HTTP status code.
proxy_req() {
  local method="$1"
  local path="$2"
  local body="${3:-}"
  local -a curl_args=(-s -o /dev/null -w "%{http_code}" --max-time 10 --unix-socket "$DOCKER_SOCK" -X "$method")
  if [[ -n "$body" ]]; then
    curl_args+=(-H 'Content-Type: application/json' -d "$body")
  fi
  curl "${curl_args[@]}" "http://localhost${path}"
}

# Assert HTTP status; write diagnostic and exit 1 on mismatch.
assert_http() {
  local want="$1"
  local got="$2"
  local desc="$3"
  if [[ "$got" != "$want" ]]; then
    fail_with_diag "[$desc] expected HTTP $want, got HTTP $got"
  fi
}

case "${DOCKER_PROXY_TEST_CASE:-}" in
  "")
    fail_with_diag "DOCKER_PROXY_TEST_CASE not set"
    ;;

  bind-escape)
    code=$(proxy_req POST /containers/create '{"HostConfig":{"Binds":["/etc:/etc"]}}')
    assert_http 403 "$code" "bind-escape: Binds=[/etc:/etc]"
    ;;

  mount-escape)
    code=$(proxy_req POST /containers/create \
      '{"HostConfig":{"Mounts":[{"Type":"bind","Source":"/etc","Target":"/mnt"}]}}')
    assert_http 403 "$code" "mount-escape: Mounts[type=bind]"
    ;;

  volume-bind-escape)
    code=$(proxy_req POST /containers/create \
      '{"HostConfig":{"Mounts":[{"Type":"volume","Target":"/mnt","VolumeOptions":{"DriverConfig":{"Name":"local","Options":{"type":"none","device":"/etc","o":"bind"}}}}]}}')
    assert_http 403 "$code" "volume-bind-escape: volume+device+o=bind"
    ;;

  privileged)
    code=$(proxy_req POST /containers/create '{"HostConfig":{"Privileged":true}}')
    assert_http 403 "$code" "privileged: Privileged=true"
    ;;

  host-network)
    code=$(proxy_req POST /containers/create '{"HostConfig":{"NetworkMode":"host"}}')
    assert_http 403 "$code" "host-network: NetworkMode=host"
    ;;

  container-network)
    code=$(proxy_req POST /containers/create '{"HostConfig":{"NetworkMode":"container:somecontainer"}}')
    assert_http 403 "$code" "container-network: NetworkMode=container:"
    ;;

  security-opt)
    code=$(proxy_req POST /containers/create '{"HostConfig":{"SecurityOpt":["seccomp=unconfined"]}}')
    assert_http 403 "$code" "security-opt: SecurityOpt=[seccomp=unconfined]"
    ;;

  capadd)
    code=$(proxy_req POST /containers/create '{"HostConfig":{"CapAdd":["NET_ADMIN"]}}')
    assert_http 403 "$code" "capadd: CapAdd=[NET_ADMIN]"
    ;;

  device)
    code=$(proxy_req POST /containers/create \
      '{"HostConfig":{"Devices":[{"PathOnHost":"/dev/sda","PathInContainer":"/dev/sda","CgroupPermissions":"rwm"}]}}')
    assert_http 403 "$code" "device: Devices=[/dev/sda]"
    ;;

  build-denied)
    c1=$(proxy_req POST /build)
    assert_http 403 "$c1" "build-denied: POST /build"
    c2=$(proxy_req POST /session)
    assert_http 403 "$c2" "build-denied: POST /session"
    ;;

  cross-job-isolation)
    # The ledger has no entries → proxy returns 404 for unknown container IDs.
    code=$(proxy_req POST /containers/not-my-container/stop)
    assert_http 404 "$code" "cross-job-isolation: unknown container"
    ;;

  reap-on-success)
    code=$(proxy_req POST /containers/create '{}')
    assert_http 201 "$code" "reap-on-success: container create"
    # Exit 0 → task done → reap fires after sandbox exits.
    ;;

  reap-on-failure)
    code=$(proxy_req POST /containers/create '{}')
    assert_http 201 "$code" "reap-on-failure: container create"
    # Intentional failure: exit 1 → task aborted, but reap still fires.
    printf '{"artifact":{"result":"intentional-fail"}}\n' | boid task update --payload-patch @-
    exit 1
    ;;

  network-create)
    code=$(proxy_req POST /networks/create '{"Driver":"host"}')
    assert_http 403 "$code" "network-create: Driver=host deny"
    code=$(proxy_req POST /networks/create '{"Driver":"bridge"}')
    assert_http 201 "$code" "network-create: Driver=bridge allow"
    ;;

  volume-create)
    code=$(proxy_req POST /volumes/create '{"DriverOpts":{"type":"none","device":"/etc","o":"bind"}}')
    assert_http 403 "$code" "volume-create: DriverOpts device=/etc deny"
    code=$(proxy_req POST /volumes/create '{"Driver":"local"}')
    assert_http 201 "$code" "volume-create: Driver=local allow"
    ;;

  passthrough)
    c1=$(proxy_req GET /version)
    assert_http 200 "$c1" "passthrough: GET /version"
    c2=$(proxy_req GET /containers/json)
    assert_http 200 "$c2" "passthrough: GET /containers/json"
    ;;

  *)
    fail_with_diag "unknown DOCKER_PROXY_TEST_CASE=${DOCKER_PROXY_TEST_CASE}"
    ;;
esac

printf '{"artifact":{"result":"pass"}}\n' | boid task update --payload-patch @-
