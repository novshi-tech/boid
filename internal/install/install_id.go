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
func LoadOrCreate(dir string) (string, error) {
	path := filepath.Join(dir, FileName)

	data, err := os.ReadFile(path)
	if err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id, nil
		}
		// Empty file (e.g. truncated by a crash mid-write): fall through
		// and regenerate rather than handing callers an empty label value.
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("install: read %s: %w", path, err)
	}

	id, err := newUUIDv4()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("install: mkdir %s: %w", dir, err)
	}
	// 0644 (not the 0600 mtls.LoadOrCreate / LoadOrCreateKey use): install_id
	// is explicitly non-secret (§決定6 — "平文 UUID... 非秘密"), unlike
	// web_secret or the mTLS CA's private key. There is no confidentiality
	// requirement to enforce with tighter permissions here.
	if err := os.WriteFile(path, []byte(id+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("install: write %s: %w", path, err)
	}
	return id, nil
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
