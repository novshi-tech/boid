# Web UI

`boid` ships a Web UI alongside the CLI. It is enabled by default and listens on `:8080`. From the loopback interface no authentication is needed; from anywhere else (typically a phone over Cloudflare Tunnel) you pair a device first.

## Open the UI locally

After `boid start`, point a browser at `http://localhost:8080`. You should see the task list.

The listen address can be changed with `boid web set-addr`:

```bash
boid web set-addr 127.0.0.1:5171
```

**The Web UI cannot be disabled.** Setting the address to an empty string (e.g. `boid web set-addr ""`) does not prevent the HTTP listener from starting; the daemon falls back to `:8080`. There is currently no way to stop the HTTP listener entirely.

## Access from another device

`boid` is single-user. Pairing only protects against accidental access; treat the daemon as if it were on your own laptop.

Three steps:

1. Make the URL reachable. Either run on a LAN address or — recommended for phones — front it with [Cloudflare Tunnel](#cloudflare-tunnel).
2. Set the public URL once: `boid web set-url https://boid.example.com`. This is used to render the magic link in the pairing payload.
3. Run `boid web pair` and copy the code into the device's login page. The code is good for 5 minutes and one device.

```bash
boid web pair                    # issue a pairing code
boid web devices                 # list paired devices
boid web revoke <device-id>      # revoke one device
boid web revoke-all              # revoke all
```

A device cookie lasts 90 days (rolling) or 30 days idle. CSRF is enforced via a double-submit cookie.

### Pairing code format

`WX7K-4QJP` (8 alphanumeric characters with a hyphen). Single use, 5-minute lifetime, rate-limited to 5 attempts per 5 minutes per IP.

### Loopback exception

Requests from `127.0.0.1` or `::1` skip pairing entirely. The check also rejects loopback if the request carries `X-Forwarded-For`, `CF-Connecting-IP`, or `Forwarded` headers, so a Tunnel that proxies to localhost will not bypass auth by accident.

## Cloudflare Tunnel

The recommended way to expose `boid` to your phone is `cloudflared` running as a user systemd service.

### Prerequisites

- A Cloudflare account and a domain managed in Cloudflare DNS (e.g. `nosen.dev`).
- `cloudflared` installed (`apt install cloudflared` or via Cloudflare's package repository).

### One-time setup

1. Authenticate `cloudflared` with your Cloudflare account.

   ```bash
   cloudflared tunnel login
   ```

2. Create a tunnel.

   ```bash
   cloudflared tunnel create boid
   ```

   This generates a credentials JSON under `~/.cloudflared/<tunnel-id>.json`.

3. Configure routing. Create `~/.cloudflared/config.yml`:

   ```yaml
   tunnel: <tunnel-id>
   credentials-file: /home/<you>/.cloudflared/<tunnel-id>.json

   ingress:
     - hostname: boid.example.com
       service: http://127.0.0.1:8080
     - service: http_status:404
   ```

4. Map the hostname to the tunnel.

   ```bash
   cloudflared tunnel route dns boid boid.example.com
   ```

5. Run the tunnel as a user-level systemd unit (`~/.config/systemd/user/cloudflared-boid.service`):

   ```ini
   [Unit]
   Description=cloudflared tunnel for boid
   After=network-online.target

   [Service]
   ExecStart=/usr/bin/cloudflared tunnel run boid
   Restart=on-failure

   [Install]
   WantedBy=default.target
   ```

   Enable and start it:

   ```bash
   systemctl --user enable --now cloudflared-boid.service
   ```

6. Tell `boid` the public URL so magic links work:

   ```bash
   boid web set-url https://boid.example.com
   ```

### From the phone

Visit `https://boid.example.com`, enter the pairing code from `boid web pair`, and the device cookie is set. From then on the device can drive `boid` until you revoke it or 90 days pass.

### Security notes

- Pairing is not a substitute for proper firewalling — it just prevents random visitors from poking at the API. Do not skip the public URL guard or disable HTTPS.
- Cloudflare Access can be layered on top of the tunnel for an additional auth check (email or service token), if you want belt-and-suspenders.
- Revoke any device you no longer use. There is no inactivity timeout shorter than 30 days.

## Sessions

A session is a running job that is not tied to any task — started via `boid agent` or the Web UI's [New Session] dialog. The mental model is similar to `tmux ls`: interactive sessions that are currently running and can be reattached at any time.

### Session list (/sessions)

Click **Sessions** in the global nav. The page shows only **currently running sessions** across all projects. Finished sessions disappear from the list (no history is kept).

Click any row to navigate to `/jobs/{id}` and reattach to the agent's terminal.

### New session (/sessions/new)

Click **Create** at the bottom right of the sessions list, or navigate to `/sessions/new` directly.

1. **Select a project** — the harness form appears once you pick one.
2. **Select a harness** — `claude`, `opencode`, or `shell`.
3. **Instruction (optional)** — a prompt delivered as the first turn. Leave empty to use the harness default.
4. **readonly checkbox** — check to mount the project directory read-only (default: writable).
5. **Session name (optional)** — the label shown in the sessions list.
6. Click **Start session**. The browser redirects to `/jobs/{id}/terminal` automatically.

### CLI equivalent

```bash
boid agent claude -p <project>   # same as [New Session] from the Web UI
```

## Pages

The current Web UI covers:

- **Task list** with filters (status, behavior, project)
- **Task detail** with payload, jobs, and inline actions
- **Session list** (running task-less jobs across all projects)
- **New session** (pick a project and harness, then launch)
- **Project list / detail**
- **Job list / detail** with inline interactive terminal (xterm.js, live attach via `GET /api/jobs/{id}/attach/ws`)
- **Pairing / login** flow

---

Next: [Troubleshooting](troubleshooting.md)
