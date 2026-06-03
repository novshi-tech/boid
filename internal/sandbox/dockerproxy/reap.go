package dockerproxy

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
)

// Reap reads the ledger and issues stop+rm to the upstream Docker socket for
// every resource the job created.  Deletion order is containers → networks →
// volumes (dependency order); exec entries are skipped because they are
// cleaned up automatically when their container is removed.
//
// Individual stop/rm failures are logged and skipped rather than aborting,
// so a single stuck container does not prevent the rest from being cleaned up.
// C5 (cleanupSandboxAfterWait and the GC loop) calls this function.
func Reap(ctx context.Context, upstreamSocket string, l *Ledger) error {
	resources, err := l.ReadAll()
	if err != nil {
		return fmt.Errorf("reap: reading ledger: %w", err)
	}
	if len(resources) == 0 {
		return nil
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", upstreamSocket)
		},
	}
	client := &http.Client{Transport: transport}

	var containers, networks, volumes []string
	for _, r := range resources {
		switch r.Type {
		case "container":
			containers = append(containers, r.ID)
		case "network":
			networks = append(networks, r.ID)
		case "volume":
			volumes = append(volumes, r.ID)
		}
	}

	// Containers first: stop (best-effort, t=0 for fast exit) then remove.
	for _, id := range containers {
		if err := reapPost(ctx, client, "/containers/"+id+"/stop?t=0"); err != nil {
			slog.Warn("docker reap: stop container", "id", id, "err", err)
		}
		if err := reapDelete(ctx, client, "/containers/"+id+"?force=1"); err != nil {
			slog.Warn("docker reap: remove container", "id", id, "err", err)
		}
	}

	// Networks after containers (containers must be disconnected first).
	for _, id := range networks {
		if err := reapDelete(ctx, client, "/networks/"+id); err != nil {
			slog.Warn("docker reap: remove network", "id", id, "err", err)
		}
	}

	// Volumes last (may be in use by containers until they are removed).
	for _, id := range volumes {
		if err := reapDelete(ctx, client, "/volumes/"+id); err != nil {
			slog.Warn("docker reap: remove volume", "id", id, "err", err)
		}
	}

	return nil
}

func reapPost(ctx context.Context, client *http.Client, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker"+path, bytes.NewReader(nil))
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	// 304 (not modified) and 404 (already gone) are acceptable.
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusNotModified {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func reapDelete(ctx context.Context, client *http.Client, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	// 404 means already gone — not an error for cleanup purposes.
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
