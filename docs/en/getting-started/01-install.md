# 1. Install

This page gets `boid` running on your machine and verifies the install. It takes about five minutes.

## Prerequisites

- **Linux.** `boid` is Linux-only; macOS and Windows are not supported.
- **Go 1.24 or later.** Required because the CLI install goes through `go install`.
- **`$GOBIN` (or `$GOPATH/bin`) on `PATH`.** Verify with `go env GOBIN` and check that the directory it prints — or `$HOME/go/bin` if it is empty — is on your `PATH`.
- **docker (with the compose plugin) or podman (with podman-compose).** The daemon runs as a container managed by `docker compose` / `podman-compose` (`build/container/compose.yml`). Sandbox execution also goes through the same engine (docker-out-of-docker), so it must be reachable.

## Install

```bash
go install github.com/novshi-tech/boid@latest
```

Verify the binary is reachable:

```bash
boid --help
```

You should see a list of subcommands (`start`, `check`, `task`, `job`, `project`, `workspace`, `web`, `agent`, `secret`, `gc`, `stop`, ...).

## Check prerequisites

```bash
boid check
```

This reports on engine (docker/podman) reachability, the compose plugin, the docker-out-of-docker bind source socket, `podman.socket`'s active state, the uid the daemon will run as, and host-arch-vs-image-arch matching. Resolve anything it flags before continuing — the most common issues are a missing compose plugin or an unreachable docker/podman socket.

## Start the daemon

`boid` splits into a CLI and a daemon (the process that owns task persistence, execution, and observation). The daemon has exactly **one** supported shape: a compose stack — `boid` itself drives `docker compose up -d` / `podman-compose up -d` on your behalf (`build/container/compose.yml`, "host mode").

```bash
boid start
```

On first run this pulls the image matching this CLI binary's own version (`ghcr.io/novshi-tech/boid-runner:<version>`), brings the compose stack up, and waits for the daemon to answer its health check. This assumes the GHCR image is published publicly — if it is not yet public, or ends up private for any reason, the pull fails and you need `docker login ghcr.io` first (see the known-limitations note in `docs/plans/release-onboarding.md`). If you have a checkout on hand and `BOID_COMPOSE_ROOT` set (a developer workflow), it builds locally instead — see [CLI reference / Host mode](../reference/cli.md) for details.

Once it is up, you will see onboarding guidance for what to do next:

```
boid server started (compose, cli: http://127.0.0.1:8442)

Next steps:
  1. boid web pair                                      # pair this browser with the Web UI
  2. boid project add <git-url> --workspace=<name>      # register a project (new project? run `boid project init` first)
  3. boid workspace set-init-script <name> -f init.sh   # register an init script for automatic toolchain install
  4. boid agent claude -p <project>                     # open an interactive session to sign in
```

If the daemon is not running, the first command that needs it (for example `boid task list`) starts it automatically, so you do not have to type `boid start` every time (disable with `BOID_NO_AUTOSTART=1`).

## Pair the Web UI

Under the compose daemon, the loopback address (127.0.0.1) the CONTAINER itself sees is not the same thing as `http://localhost:8080` as seen from your host — so the old "loopback skips pairing" exception from the bare-host days does not apply. Pair once before opening the UI:

```bash
boid web pair
```

Authenticate from your browser using the code, URL, or QR shown, then the Web UI is reachable at `http://localhost:8080` (the default compose deployment's host-side port). See [3. Set up the Web UI](03-web-ui.md#change-the-listen-address-optional) for the caveat around changing this port number (`boid web set-addr` alone does not do it).

## Verify it works

```bash
boid task list
```

The list is empty on a fresh install — that is the expected output.

## Stop the daemon

```bash
boid stop
```

This brings the compose stack down (`docker/podman compose down` equivalent). Always use `boid stop` rather than killing the container directly.

## Where data lives

All of the daemon's persistent data (the SQLite database, kits, the secret encryption key, the Web UI signing key, logs, ...) lives inside a single named volume, `boid_boid_state` (the `boid_state` volume declared in `build/container/compose.yml`, prefixed with the compose project name, `boid`). **Host-side XDG paths such as `~/.local/share/boid/` are not visible to the daemon** — a named volume is used instead of a bind mount specifically to avoid rootless podman's uid-mapping complications across the host filesystem.

Each workspace's own home directory (its toolchain, and the claude/codex CLI's own login credentials) lives in a SEPARATE named volume per workspace, `boid-ws-home-<installID8>-<slug>` — see the [workspace home guide](../guide/workspace-home.md) for details; it is not part of `boid_boid_state`.

The only files actually created on the host (the machine running the `boid` CLI) are small ones needed to manage the compose daemon's lifecycle:

| Path | Contents |
|---|---|
| `~/.config/boid/cli-token` | Shared secret between the CLI and the daemon container (generated on first `boid start`, mode 0600) |
| `~/.local/state/boid/compose/` | Where the embedded `compose.yml`/`Dockerfile`/`deploy-container.sh` get extracted when no checkout is found, plus the CLI's mutual-exclusion lock file |

The daemon runs a GC loop that starts 10 seconds after launch and then repeats every 24 hours. It removes data older than 30 days across several scopes: `runtimes/<runtime_id>/` directories, `/tmp/boid-*` temporary files, terminal tasks/actions/jobs records from the database, and revoked device entries. You can also trigger GC manually with `boid gc`.

## Update

Re-run `go install` with `@latest`, then restart the daemon:

```bash
go install github.com/novshi-tech/boid@latest
boid stop
boid start
```

Since bumping the CLI version also changes the image ref `boid start` pulls (`internal/version.DefaultContainerImage()`), a plain `boid stop && boid start` keeps the CLI and the daemon's image in sync. If a DB migration is needed between versions, startup guidance explains what to do (see the [migration guide](../guide/migration.md)).

## Uninstall

```bash
boid stop

# The daemon's own data (DB, kits, secrets, ...). The volume name is the
# compose project name (default "boid") plus "_boid_state" -- see the
# `name: boid` field in build/container/compose.yml.
docker volume rm boid_boid_state    # or: podman volume rm boid_boid_state

# Each workspace's home (including claude/codex login credentials and
# installed toolchains) lives in its OWN separate named volume,
# boid-ws-home-<installID8>-<slug> -- list and remove them too (see the
# workspace home guide for details).
docker volume ls --filter name=boid-ws-home- -q | xargs -r docker volume rm

rm -rf ~/.config/boid ~/.local/state/boid/compose
rm "$(go env GOBIN 2>/dev/null || echo "$(go env GOPATH)/bin")/boid"
```

The two `docker volume rm` commands above remove all local data, including tasks, secret values, claude/codex login credentials, and installed extension packages. Skip either one if you want to preserve that data across a reinstall. See the [workspace home guide](../guide/workspace-home.md) for the exact naming/inspection details of the per-workspace volumes.

---

Next: [2. Initialize a project](02-init-project.md)
