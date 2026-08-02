# 3. Set up the Web UI

This page gets you to a working `boid` Web UI in the browser. The next chapter runs a real task, and it is much easier to follow if you can watch it live from the browser as well as the terminal. It takes about three minutes.

This page assumes you have registered the `demo` project from [2. Initialize a project](02-init-project.md).

## Why look at the Web UI first

`boid`'s main job is to take long-running tasks off your hands. Keeping the Web UI open in a tab is the easiest way to see, at a glance, what is currently running, what is waiting on input, and what is stuck — without staying glued to the terminal. It is also the fastest path to driving `boid` from a phone, which is handy when you want to check progress away from the desk.

## Open it locally

After `boid start` (from [1. Install](01-install.md)), the daemon (compose stack) is already listening on `:8080`.

Under the compose daemon, the loopback address (127.0.0.1) the CONTAINER itself sees is not the same thing as `http://localhost:8080` seen from your host, so the old "loopback skips pairing" exception from the bare-host days does not apply. If you have not already run `boid web pair` from [1. Install](01-install.md), do that first:

```bash
boid web pair
```

Authenticate from your browser with the printed code / URL / QR, then open the Web UI:

```
http://localhost:8080
```

You should see the `demo` project from [2. Initialize a project](02-init-project.md) and an empty task list.

When the next chapter creates a task, it is convenient to keep this tab open next to a `boid task watch` terminal.

## Change the listen address (optional)

**Note:** under the standard compose deployment, `boid web set-addr` alone does NOT resolve a port conflict. `build/container/compose.yml`'s `ports:` mapping is fixed at host `127.0.0.1:8080` → container `8080`. `boid web set-addr` only changes the bind address **inside the container** — pointing it at anything other than port 8080 (e.g. `127.0.0.1:5171`) means nothing listens on the container's port 8080 anymore, and port 5171 was never published to the host, so **the Web UI becomes unreachable**:

```bash
# Only changes the in-container bind address -- becomes unreachable from the host
boid web set-addr 127.0.0.1:5171   # DON'T: this breaks Web UI reachability as-is
boid stop
boid start
```

To actually change the port visible from the host (default 8080), edit the `ports:` section of `build/container/compose.yml` directly (the `"127.0.0.1:8080:8080"` line) — this is a developer workflow that requires a checkout of this repository; there is currently no equivalent a `go install`-only user can reach for (the embedded `compose.yml` extracted when no checkout is found gets overwritten on every `boid start`, so a hand edit there would not persist either). If the default `:8080` clashes with something else, either free up that port on the conflicting service instead, or put a reverse proxy in front that forwards a different host port to `:8080`.

> **Note:** There is currently no way to disable the Web UI entirely. Passing an empty string still causes the daemon to fall back to `:8080` and keep the TCP listener running.

## Reach it from another device (optional)

To open the Web UI from your phone or another laptop, you need a reachable URL and a paired device.

1. Make the URL reachable. Either expose `boid` on your LAN address, or — recommended for mobile use — front it with a Cloudflare Tunnel (see the [Web UI guide](../guide/web-ui.md#cloudflare-tunnel) for the full procedure).
2. Tell `boid` the public URL once (used to render magic links):

   ```bash
   boid web set-url https://boid.example.com
   ```

3. Issue a pairing code and type it into the device's login screen:

   ```bash
   boid web pair
   ```

   Codes are good for five minutes and single-use.

```bash
boid web devices                 # list paired devices
boid web revoke <device-id>      # revoke one device
boid web revoke-all              # revoke all
```

The rest of this tutorial only needs the browser you already paired in [1. Install](01-install.md), so you can skip the external exposure. Full details live in the [Web UI guide](../guide/web-ui.md).

## Recap

What this tutorial introduced:

- Paired and opened the Web UI (the compose daemon requires pairing even from loopback).
- `boid web set-addr` only changes the in-container bind address (the standard compose deployment does not let you change the host-visible port that way); the Web UI cannot be disabled entirely either.
- Outlined how to expose the UI to other devices (`boid web set-url` + `boid web pair`).

In the next chapter you will run a small task and watch it live from this same Web UI.

---

Next: [4. Your first task](04-first-task.md)
