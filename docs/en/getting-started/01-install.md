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

## Start the daemon

```bash
boid start
```

`boid start` spawns a detached daemon and returns immediately. The output reports the PID, the UNIX socket path, and the HTTP listen address (default `:8080`). You do not need `nohup`, `&`, or systemd — `boid start` is self-daemonizing.

The daemon also starts automatically on the first command that needs it (for example `boid task list`), so you can skip `boid start` and just run a command.

## Verify it works

```bash
boid task list
```

The list is empty on a fresh install — that is the expected output.

Open the Web UI in a browser at `http://localhost:8080`. Loopback access does not require pairing, so it should load straight to the task list.

## Stop the daemon

```bash
boid stop
```

`boid stop` is the only correct way to stop the server. Killing the process by PID can leave a stale socket file.

## Where files live

`boid` follows the [XDG Base Directory specification](https://specifications.freedesktop.org/basedir-spec/basedir-spec-latest.html). Default paths assume the standard XDG environment:

| Path | Contents |
|---|---|
| `~/.local/share/boid/boid.db` | SQLite database |
| `~/.local/share/boid/kits/` | Installed kits |
| `~/.local/share/boid/runtimes/` | Per-task sandbox runtime directories (auto-GC'd) |
| `~/.local/share/boid/secret.key` | Secret-store encryption key (mode 0600) |
| `~/.local/share/boid/web_secret` | Web UI signing key (mode 0600) |
| `~/.local/state/boid/boid.log` | Daemon log (rotated) |
| `~/.config/boid/config.yaml` | Optional user config |
| `$XDG_RUNTIME_DIR/boid.sock` | UNIX socket (falls back to `/tmp/boid-<uid>.sock`) |

`~/.config/boid/config.yaml` is optional. Defaults are used if it does not exist.

## Update

Re-run `go install` with `@latest`, then restart the daemon:

```bash
go install github.com/novshi-tech/boid@latest
boid stop
boid start
```

The restart matters: the running daemon has the old binary mapped into memory. Skipping `boid stop` will leave the new binary on disk but the old code in the running process.

## Uninstall

```bash
boid stop
rm -rf ~/.local/share/boid ~/.local/state/boid ~/.config/boid
rm "$(go env GOPATH)/bin/boid"
```

The first `rm` removes all local data including tasks, secrets, and installed kits. Skip the data paths if you want to preserve them across a reinstall.

---

Next: [2. Your first task](02-first-task.md)
