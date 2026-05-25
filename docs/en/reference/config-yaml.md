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
| `http_addr` | string | `":8080"` | HTTP server listen address |
| `public_url` | string | — | External URL when exposed via Cloudflare Tunnel etc. |

`http_addr` can also be changed with `boid web set-addr <addr>`.

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
