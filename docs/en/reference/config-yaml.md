# config.yaml Reference

`~/.config/boid/config.yaml` is the user configuration file for the boid daemon (XDG-compliant).
If the file does not exist, default values are used without error.

Changes take effect after `boid stop && boid start`.

---

## gc — Garbage Collection

```yaml
gc:
  enabled: true       # set to false to disable automatic GC
  interval: 24h       # how often GC runs (default: 24h)
  older_than: 720h    # delete data older than this (default: 720h = 30 days)
```

| Key | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `true` | Enable/disable automatic GC |
| `interval` | duration | `24h` | GC run interval |
| `older_than` | duration | `720h` | Minimum age of data to delete |

These settings are read from `config.yaml` via a custom YAML parser (`Config.UnmarshalYAML`).
Although the `GCConfig` struct fields carry `yaml:"-"` tags, the values are explicitly decoded and applied at load time.

> **Note:** `older_than` in `config.yaml` only affects the **automatic GC loop** (daemon background goroutine).
> Manual `boid gc` (and `POST /api/gc`) uses a **hardcoded default of 720h (30 days)** and does not read the config value.
> To override the threshold for a one-off manual run, use `boid gc --older-than <duration>`.

Manual GC can be triggered with `boid gc`.

---

## web — Web UI

```yaml
web:
  http_addr: ":8080"                      # listen address (default: :8080)
  public_url: "https://boid.example.com"  # external URL for magic links
```

| Key | Type | Default | Description |
|---|---|---|---|
| `http_addr` | string | `""` | HTTP server listen address |
| `public_url` | string | — | External URL when exposed via Cloudflare Tunnel etc. |

> **Default address:** `config.DefaultConfig()` leaves `http_addr` empty. The effective default of `127.0.0.1:8080` is applied as a fallback in `cmd/start.go` at startup time.

`http_addr` can also be changed with `boid web set-addr <addr>`.

> **Warning:** `boid web set-addr` and `boid web set-url` rewrite `config.yaml` via YAML round-trip (`yaml.Marshal`), which **strips all comments** from the file.

---

## notify — Notifications

```yaml
notify:
  command: ["/home/you/bin/boid-notify.sh", "--title", "boid"]
```

| Key | Type | Default | Description |
|---|---|---|---|
| `command` | []string | — | Command to exec when `boid task notify` is called |

If empty, `boid task notify` returns HTTP 501 and skips the notification (does not affect task execution).

---

## sandbox — Sandbox

```yaml
sandbox:
  allowed_domains:
    - ".github.com"       # leading dot = suffix match
    - "api.example.com"   # no dot = exact match
```

| Key | Type | Default | Description |
|---|---|---|---|
| `allowed_domains` | []string | `[]` | Domains to append to the built-in allow list |

These are merged with `defaultAllowedDomains` (Anthropic/OpenAI APIs, language package registries, etc.) at startup.
See [Sandbox Internals](../architecture/sandbox-internals.md) for details on the proxy allow list.
