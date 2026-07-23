// Package install provides the daemon's install identity: a plain UUID
// persisted once per boid installation and stamped as the boid.install_id
// label on every container/network/volume the daemon creates
// (docs/plans/phase6-container-backend.md §PR6, §決定6). It is not a
// secret — same non-secret, same-data-dir convention as web_secret
// (internal/dispatcher.LoadOrCreateKey) and the internal mTLS CA
// (internal/mtls.LoadOrCreate) — so LoadOrCreate mirrors both packages'
// load-or-generate-and-persist shape.
//
// This value is deliberately NOT derived from anything host-identifying
// (hostname, MAC address, /etc/machine-id, ...): §決定6 calls for "平文
// UUID を LoadOrCreate" specifically because boid has no existing notion of
// a machine id (2026-07-22 実査, plan doc §決定6) and a random UUID is
// sufficient to scope reap (§決定6's `boid reap`) to "resources this
// install created" without any host-fingerprinting privacy cost.
package install

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileName is the file LoadOrCreate reads/writes under its dir argument.
const FileName = "install_id"

// LoadOrCreate reads dir/install_id, or generates a fresh random UUID (v4)
// and persists it there if the file is missing or empty. dir is created
// (0700, matching mtls.LoadOrCreate's directory permissions) if needed.
//
// The returned value is always the exact trimmed file contents on a
// successful read — this package treats it as an opaque label value (used
// verbatim as a docker `boid.install_id` label), not a UUID to be
// re-validated on every load. Only a missing or empty file triggers
// generation.
//
// Concurrent-safe (Major 7, PR6 codex review): two LoadOrCreate calls
// racing on the same dir (e.g. two daemon instances starting at once
// against the same data dir) cannot both "win" and each persist a
// different UUID. The create path writes the full content to a temp file
// first, then publishes it onto path with os.Link — which fails with
// os.IsExist if path already exists, unlike os.Rename, which would
// silently replace it — so "path exists" only ever means "path already
// has complete content" (there is no observable window where a reader
// sees an empty, half-created file — the fatal gap in an
// O_CREATE|O_EXCL-then-separate-Write approach, where a losing goroutine
// can read the winner's file before the winner's own Write call has run
// and misidentify it as a crash-corrupted empty file needing repair). A
// losing caller re-reads the winner's now-guaranteed-complete file
// instead of silently returning its own freshly generated (and
// never-actually-persisted) id. §決定6 requires exactly one id per
// install, not "whichever caller's write happened to land last".
func LoadOrCreate(dir string) (string, error) {
	path := filepath.Join(dir, FileName)

	if id, ok, err := readValidInstallID(path); err != nil {
		return "", err
	} else if ok {
		return id, nil
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("install: mkdir %s: %w", dir, err)
	}

	id, err := newUUIDv4()
	if err != nil {
		return "", err
	}

	tmpPath, err := writeInstallIDTempFile(dir, id)
	if err != nil {
		return "", err
	}
	// Always clean up the temp name: os.Link (the success path) leaves it
	// behind as a second, now-redundant hard link to the same inode;
	// os.Rename (the repair path below) already consumes it, so this
	// becomes a harmless no-op ENOENT in that case.
	defer os.Remove(tmpPath)

	if err := os.Link(tmpPath, path); err != nil {
		if !os.IsExist(err) {
			return "", fmt.Errorf("install: publish %s: %w", path, err)
		}
		// path already exists. Every writer that ever reaches this point
		// uses this same write-temp-then-Link protocol, so if a
		// concurrent LoadOrCreate call published it, its content is
		// already complete — re-reading finds it. The only other way
		// path can exist here is a stale, invalid artifact with no live
		// writer racing us (a prior crash mid-write under the old
		// pre-Major-7 code path, or a directly-seeded empty file) —
		// os.Rename (unlike another os.Link) replaces it outright rather
		// than failing, which is correct precisely because nothing else
		// holds a legitimate claim on that content.
		if existingID, ok, rerr := readValidInstallID(path); rerr != nil {
			return "", rerr
		} else if ok {
			return existingID, nil
		}
		if rerr := os.Rename(tmpPath, path); rerr != nil {
			return "", fmt.Errorf("install: repair %s: %w", path, rerr)
		}
	}
	return id, nil
}

// writeInstallIDTempFile writes content+"\n" to a fresh temp file created
// in dir (so LoadOrCreate's subsequent os.Link/os.Rename onto FileName
// stays on the same filesystem, a hard-link requirement) and returns its
// path, fully written, permissioned, and closed before returning.
func writeInstallIDTempFile(dir, content string) (string, error) {
	tmp, err := os.CreateTemp(dir, ".install_id-*.tmp")
	if err != nil {
		return "", fmt.Errorf("install: create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	// 0644 (not the 0600 mtls.LoadOrCreate / LoadOrCreateKey use, and not
	// os.CreateTemp's own default 0600): install_id is explicitly
	// non-secret (§決定6 — "平文 UUID... 非秘密"), unlike web_secret or the
	// mTLS CA's private key. There is no confidentiality requirement to
	// enforce with tighter permissions here — chmod before the temp file
	// is published (Link/Rename) so path never has a moment at the wrong
	// mode.
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return "", fmt.Errorf("install: chmod temp file: %w", err)
	}
	if _, err := tmp.WriteString(content + "\n"); err != nil {
		tmp.Close()
		return "", fmt.Errorf("install: write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("install: close temp file: %w", err)
	}
	return tmpPath, nil
}

// readValidInstallID reads path and returns its trimmed content when
// non-empty. A missing file is not an error (ok=false, err=nil) — the
// caller falls through to generation. An empty file (e.g. truncated by a
// crash mid-write) is treated the same way: ok=false so the caller
// regenerates rather than handing back an empty label value.
func readValidInstallID(path string) (id string, ok bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("install: read %s: %w", path, err)
	}
	if trimmed := strings.TrimSpace(string(data)); trimmed != "" {
		return trimmed, true, nil
	}
	return "", false, nil
}

// newUUIDv4 generates a random RFC 4122 version-4 UUID using crypto/rand
// only (CLAUDE.md: 外部ライブラリは最小限。標準ライブラリで実現できるものは追加しない —
// no google/uuid dependency for 16 random bytes and two bit twiddles).
func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("install: generate uuid: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
