# Docker proxy migration guide

How to migrate from the docker kit (cetusguard-based) to the boid native Docker proxy (`capabilities.docker`).

## Background

The previous docker kit delegated Docker API access control to the external tool [cetusguard](https://github.com/hectorm/cetusguard). This required users to manually install the cetusguard binary, create a rules file, and enable a systemd unit before the sandbox could use Docker. Additionally, cetusguard only matched on HTTP method and URL path — it **did not inspect request bodies** — so dangerous settings like `HostConfig.Privileged`, `HostConfig.Binds`, and `HostConfig.NetworkMode=host` could not be blocked.

The boid native proxy solves both problems:

- Managed entirely by the boid daemon — no external setup required
- Inspects request bodies and rejects dangerous `HostConfig` settings
- Enforces per-job ID scope checks so each sandbox can only access the resources it created
- Automatically disables TestContainers' Ryuk and cleans up containers on job exit

**The docker kit still exists and has not been removed.** However, the native proxy is recommended for new projects. Existing projects can migrate using the steps below.

## Migration steps

> **Note:** `capabilities` and `host_commands` are no longer `project.yaml` fields. The current schema rejects both of them at load time — machine-local runtime configuration lives on a **workspace** instead (`boid workspace create`/`edit`, or `boid workspace apply -f` for the exported envelope shape used below). If your `project.yaml` still carries these fields under the old schema, convert it to a workspace first with `boid project migrate <dir> --apply` (see the [Migration guide](migration.md)).

### 1. Update the workspace

Remove the docker kit reference from the workspace's `kits:` and add `capabilities.docker` directly to the workspace. First, export its current contents — `boid workspace export` writes an `apiVersion: boid.dev/v1 / kind: Workspace` envelope document (2026-07, docs/plans/volume-only-daemon.md §論点g), the same shape `boid workspace apply -f` reads back, so this file can be edited and re-applied directly with no reshaping in between:

```bash
boid workspace export <slug> > ws.yaml
```

> **Note (Phase 2.5 PR7):** `WorkspaceMeta.Kits` was removed from the code outright, so `boid workspace export`'s output never contains a `kits:` key at all (a DB-backed workspace never had a `kits` column to begin with) — there is nothing to remove from the exported file itself. `kits:` only ever shows up in a legacy, not-yet-migrated workspace *shadow yaml* hand-edited directly on disk (a different, less common path than the export/apply flow below); if that is what you are editing, see `boid workspace assign`'s auto-create convenience path instead.

The examples below are illustrative — your actual `ws.yaml` reflects whatever the workspace currently has (`env`, assigned `projects`, etc., all populated, not empty). The only change to make is adding `spec.capabilities.docker`; leave every other field exactly as exported, especially `spec.projects` — it is present in a real export and, per `apply`'s upsert semantics, REPLACES the workspace's whole project-assignment set, so clearing or hand-typing it risks detaching every project actually assigned to this workspace.

**Before (`ws.yaml`, as `boid workspace export` writes it — `capabilities` absent/empty):**

```yaml
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: <slug>
spec:
  host_commands: []
  env: {}
  allowed_domains: []
  extra_repos: []
  container_image: ""
  capabilities: {}
  projects: []
```

**After (add `spec.capabilities.docker`, leave every other field as exported):**

```yaml
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: <slug>
spec:
  host_commands: []
  env: {}
  allowed_domains: []
  extra_repos: []
  container_image: ""
  capabilities:
    docker: {}   # ← add
  projects: []
```

Apply it:

```bash
boid workspace apply -f ws.yaml
```

| Old (`project.yaml`, removed) | New (workspace) |
|---|---|
| `kits: [..., docker]` (docker kit referenced at the project top level) | Remove the docker kit name from the workspace's `kits:`, set `capabilities: { docker: {} }` directly |
| `capabilities.docker: {}` (top-level `project.yaml`) | `capabilities.docker: {}` (workspace) — same shape, just a different location |

### 2. Check `host_commands`

When `capabilities.docker` is enabled on a workspace, registering `docker` in `host_commands` without subcommand restrictions causes an error at job launch:

```
host_commands.docker: unrestricted docker access bypasses the docker proxy
(capabilities.docker is enabled); remove docker from host_commands or restrict
to specific subcommands (e.g. allow: [build])
```

`host_commands` is a two-tier structure — a workspace only carries a list of reference **names**; the actual definitions (`allow`/`deny`/`path`, etc.) live in the daemon-wide registry `~/.config/boid/host_commands.yaml`. If the workspace's `host_commands` list includes `docker` and the registry's definition for it has no subcommand restriction, choose one of:

- **Remove it (recommended)**: the proxy socket routes docker CLI, SDKs, and TestContainers automatically — no `host_commands` entry is needed. Remove `docker` from the workspace's `host_commands` list.
- **Restrict to specific subcommands**: if host-side execution is genuinely required (e.g. for image builds), restrict the registry's definition:

```yaml
# ~/.config/boid/host_commands.yaml
host_commands:
  docker:
    allow: [build]   # host-side docker build only
```

```bash
boid host-commands reload
```

| Old (`project.yaml`, removed) | New |
|---|---|
| `host_commands.docker: { allow: [...] }` (top-level `project.yaml`) | Workspace's `host_commands: [docker]` (reference name) + the actual definition `docker: { allow: [...] }` in `~/.config/boid/host_commands.yaml` |

See [Onboarding / Defining host_commands](onboarding.md#defining-host_commands-the-daemon-wide-registry) for details.

### 3. Remove cetusguard

cetusguard is no longer needed. Remove it with the following steps.

**Stop and disable the systemd unit:**

```sh
systemctl --user stop cetusguard.service
systemctl --user disable cetusguard.service
```

**Delete the unit file:**

```sh
rm ~/.config/systemd/user/cetusguard.service
systemctl --user daemon-reload
```

**Delete the rules file:**

```sh
rm -rf ~/.config/cetusguard/
```

**Delete the cetusguard binary** (adjust the path to match your installation):

```sh
# If installed via go install
rm ~/go/bin/cetusguard

# If installed to ~/.local/bin
rm ~/.local/bin/cetusguard
```

### 4. Verify

After migrating, confirm Docker access works from the sandbox. Use `boid exec` to enter the sandbox and run:

```sh
# Check that the proxy socket is reachable
curl --unix-socket /run/boid/docker-proxy.sock http://d/_ping
# → "OK" means success

# Or use the docker CLI if available in the sandbox
docker info
```

## Using the docker CLI through the proxy

Inside the sandbox, `DOCKER_HOST` is set automatically by boid — no extra configuration is needed. If the docker binary is on the sandbox `PATH`, commands work as-is:

```sh
# Run inside the sandbox (DOCKER_HOST is already set)
docker ps
docker run --rm hello-world
```

TestContainers reads `DOCKER_HOST` as well, so no code changes are required.

## Security notes

The proxy is the primary defence layer, but we recommend running the host Docker daemon in **rootless mode** to limit the impact of any proxy bypass. Rootless Docker confines containers to a user namespace, making host-root escalation structurally impossible.

```sh
# Set up rootless Docker (one time)
curl -fsSL https://get.docker.com/rootless | sh
# or via distro package
apt install docker-ce-rootless-extras   # Ubuntu/Debian
```

boid resolves the upstream socket at startup: `DOCKER_HOST` env → rootless path (`$XDG_RUNTIME_DIR/docker.sock`) → rootful `/var/run/docker.sock`.

For the full security model see [Sandbox internals / Docker proxy](../architecture/sandbox-internals.md#docker-proxy-capabilitiesdocker).

## Related documents

- [`project.yaml` reference / capabilities.docker](../reference/project-yaml.md#capabilitiesdocker)
- [Sandbox internals / Docker proxy](../architecture/sandbox-internals.md#docker-proxy-capabilitiesdocker)
