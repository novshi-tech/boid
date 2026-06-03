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

### 1. Update `project.yaml`

Remove the docker kit from the `kits` list and add `capabilities.docker`.

**Before:**

```yaml
kits:
  - github.com/novshi-tech/boid-kits/claude-code
  - github.com/novshi-tech/boid-kits/docker   # ← remove

task_behaviors:
  executor:
    ...
```

**After:**

```yaml
kits:
  - github.com/novshi-tech/boid-kits/claude-code

capabilities:
  docker: {}   # ← add

task_behaviors:
  executor:
    ...
```

Run `boid project reload` after saving the file.

### 2. Check `host_commands`

When `capabilities.docker` is enabled, registering `docker` in `host_commands` without subcommand restrictions causes an error at job launch:

```
host_commands.docker: unrestricted docker access bypasses the docker proxy
(capabilities.docker is enabled); remove docker from host_commands or restrict
to specific subcommands (e.g. allow: [build])
```

If `docker` is registered in `host_commands`, choose one of:

- **Remove it (recommended)**: the proxy socket routes docker CLI, SDKs, and TestContainers automatically — no `host_commands` entry is needed.
- **Restrict to specific subcommands**: if host-side execution is genuinely required (e.g. for image builds), restrict to the needed subcommand:

```yaml
host_commands:
  docker:
    allow: [build]   # host-side docker build only
```

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
