# 3. Set up the Web UI

This page gets you to a working `boid` Web UI in the browser. The next chapter runs a real task, and it is much easier to follow if you can watch it live from the browser as well as the terminal. It takes about three minutes.

This page assumes you have registered the `demo` project from [2. Initialize a project](02-init-project.md).

## Why look at the Web UI first

`boid`'s main job is to take long-running tasks off your hands. Keeping the Web UI open in a tab is the easiest way to see, at a glance, what is currently running, what is waiting on input, and what is stuck — without staying glued to the terminal. It is also the fastest path to driving `boid` from a phone, which is handy when you want to check progress away from the desk.

## Open it locally

After `boid start` (from [1. Install](01-install.md)), the daemon is already listening on `:8080`. Open it in a browser:

```
http://localhost:8080
```

You should see the `demo` project from [2. Initialize a project](02-init-project.md) and an empty task list. Requests from the same machine (loopback addresses 127.0.0.1 / ::1) skip pairing.

When the next chapter creates a task, it is convenient to keep this tab open next to a `boid task watch` terminal.

## Change the listen address (optional)

If the default `:8080` clashes with something else, change it:

```bash
boid web set-addr 127.0.0.1:5171
boid stop
boid start
```

`boid web set-addr` writes to `web.listen` in `~/.config/boid/config.yaml`. The change only takes effect after a daemon restart.

To disable the Web UI entirely, pass an empty string:

```bash
boid web set-addr ""
```

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

The rest of this tutorial only needs loopback access, so you can skip the external exposure. Full details live in the [Web UI guide](../guide/web-ui.md).

## Recap

What this tutorial introduced:

- Opened the Web UI locally (loopback skips pairing).
- Showed how to change the listen address (`boid web set-addr`).
- Outlined how to expose the UI to other devices (`boid web set-url` + `boid web pair`).

In the next chapter you will set up the Claude Code kit; the chapter after that runs a small task and watches it from this same Web UI.

---

Next: [4. Set up a kit](04-kits.md)
