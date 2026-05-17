# 1. Install

This page gets `boid` running on your machine and verifies the install. It takes about two minutes.

## Prerequisites

- **Linux.** `boid` is Linux-only; macOS and Windows are not supported.
- **Go 1.24 or later.** Required because installation goes through `go install`.
- **`$GOBIN` (or `$GOPATH/bin`) on `PATH`.** Verify with `go env GOBIN` and check that the directory it prints — or `$HOME/go/bin` if it is empty — is on your `PATH`.

## Install

```bash
go install github.com/novshi-tech/boid@latest
```

Verify the binary is reachable:

```bash
boid --help
```

You should see a list of subcommands (`start`, `task`, `job`, `project`, `web`, `kit`, `secret`, `gc`, `stop`, ...).

## Start the server (daemon)

`boid` runs a long-lived background server alongside the CLI — referred to as the daemon throughout these docs — that owns task persistence, execution, and observation.

```bash
boid start
```

`boid start` spawns the daemon as a child process and returns immediately. The output reports the PID, the UNIX socket path, and the HTTP listen address (default `:8080`). You do not need `nohup`, `&`, or systemd — `boid start` detaches the process itself.

If the daemon is not running, the first command that needs it (for example `boid task list`) starts it automatically, so you do not have to type `boid start` every time.

## Verify it works

```bash
boid task list
```

The list is empty on a fresh install — that is the expected output.

Open the Web UI in a browser at `http://localhost:8080`. Requests from the same machine (loopback addresses 127.0.0.1 / ::1) skip device pairing, so the task list should load straight away.

## Stop the daemon

```bash
boid stop
```

`boid stop` is the only correct way to stop the server. Killing the process by PID can leave a stale socket file.

## Where files live

`boid` follows the [XDG Base Directory specification](https://specifications.freedesktop.org/basedir-spec/basedir-spec-latest.html). Default paths assume the standard XDG environment:

| Path | Contents |
|---|---|
| `~/.local/share/boid/boid.db` | SQLite database holding tasks, jobs, and projects |
| `~/.local/share/boid/kits/` | Source trees of installed extension packages (kits) |
| `~/.local/share/boid/runtimes/` | Per-task working directories used during execution (auto-deleted after a retention window) |
| `~/.local/share/boid/secret.key` | Encryption key for stored secret values such as API tokens (mode 0600) |
| `~/.local/share/boid/web_secret` | Signing key for the Web UI session cookies (mode 0600) |
| `~/.local/state/boid/boid.log` | Captured stdout/stderr of the daemon (rotated by size) |
| `~/.config/boid/config.yaml` | User-supplied configuration overrides |
| `$XDG_RUNTIME_DIR/boid.sock` | UNIX socket bridging the CLI and the daemon (falls back to `/tmp/boid-<uid>.sock` when `XDG_RUNTIME_DIR` is unset) |

`~/.config/boid/config.yaml` is optional. Defaults are used if it does not exist.

## Update

Re-run `go install` with `@latest`, then restart the daemon:

```bash
go install github.com/novshi-tech/boid@latest
boid stop
boid start
```

The restart matters: the running daemon process keeps the previous binary's code resident in memory, so skipping `boid stop` leaves the new binary on disk while the old code keeps running.

## Uninstall

```bash
boid stop
rm -rf ~/.local/share/boid ~/.local/state/boid ~/.config/boid
rm "$(go env GOPATH)/bin/boid"
```

The first `rm` removes all local data including tasks, secret values, and installed extension packages. Skip the data paths if you want to preserve them across a reinstall.

---

Next: [2. Initialize a project](02-init-project.md)
