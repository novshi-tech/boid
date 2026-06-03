#!/usr/bin/env bash
set -euo pipefail

# DOCKER_HOST is injected by the proxy: unix:///run/boid/docker-proxy.sock
DOCKER_SOCK="${DOCKER_HOST#unix://}"
if [[ -z "$DOCKER_SOCK" ]]; then
  echo "ERROR: DOCKER_HOST not set or not a unix:// path" >&2
  exit 1
fi

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

# Assert HTTP status; exit 1 on mismatch (fails the task → tests the deny path).
assert_http() {
  local want="$1"
  local got="$2"
  local desc="$3"
  if [[ "$got" != "$want" ]]; then
    echo "FAIL [$desc]: expected HTTP $want, got HTTP $got" >&2
    exit 1
  fi
}

case "${DOCKER_PROXY_TEST_CASE:?}" in

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
    echo "intentional failure for reap-on-failure test" >&2
    exit 1
    ;;

  passthrough)
    c1=$(proxy_req GET /version)
    assert_http 200 "$c1" "passthrough: GET /version"
    c2=$(proxy_req GET /containers/json)
    assert_http 200 "$c2" "passthrough: GET /containers/json"
    ;;

  *)
    echo "ERROR: unknown DOCKER_PROXY_TEST_CASE=${DOCKER_PROXY_TEST_CASE}" >&2
    exit 1
    ;;
esac

mkdir -p "$HOME/.boid/output"
cat > "$HOME/.boid/output/payload_patch.json" <<'EOF'
{"payload_patch":{"artifact":{"result":"pass"}}}
EOF
