package profiles

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Token is the on-disk shape of ~/.config/boid/tokens/<profile>.json
// (decision 2, docs/plans/cli-remote-connection.md). Writing this file is
// PR2's job (`boid login`); this package only reads and validates it.
type Token struct {
	DeviceID string    `json:"device_id"`
	Token    string    `json:"token"`
	IssuedAt time.Time `json:"issued_at,omitempty"`
	// URL is the canonical origin the token was issued against (the
	// canonical_url field of POST /api/auth/device's response — see
	// internal/api/device_auth.go's deviceAuthResponse). Resolve compares
	// this byte-for-byte against the profile's own configured URL and hard-
	// errors on any mismatch (decision 9: "token は canonical origin に強く
	// bind") — never silently proceeds with a token that was issued for a
	// different origin than the one config.yaml now points the profile at.
	URL string `json:"url"`
}

// tokenFilePerm is the required permission for a token file (decision 2:
// "0600、親 dir 0700"). LoadToken only ever warns on a looser permission
// today (PR2, which will actually WRITE the file, is what enforces 0600 at
// creation time) — see LoadToken's doc comment for why a warning rather
// than a hard error is the right call for the read path.
const tokenFilePerm = 0o600

// TokensDir returns ~/.config/boid/tokens (decision 2), alongside
// ConfigPath's ~/.config/boid/config.yaml under the same XDG-resolved boid
// config directory.
func TokensDir() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("profiles: could not determine user config directory: %w", err)
	}
	return filepath.Join(configDir, "boid", "tokens"), nil
}

// TokenPath returns the token file path for profileName. profileName is
// NOT re-validated here — callers (Resolve) are expected to have already
// run it through ValidateSlug before it ever reaches a filesystem path, so
// a rejected traversal-shaped name never gets this far in the first place.
func TokenPath(profileName string) (string, error) {
	dir, err := TokensDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, profileName+".json"), nil
}

// LoadToken reads and parses the token file for profileName. A missing
// file is reported via the plain os.ErrNotExist-wrapping error LoadToken
// returns unchanged (callers use os.IsNotExist / errors.Is on it) — Resolve
// turns that specific case into the "run 'boid login <url>' first" message
// from the spec, so this function does not editorialize about *why* the
// file is missing.
//
// A permission looser than 0600 is logged as a warning, not a hard error
// (decision 2: "起動時に権限緩ければ警告") — the token itself is still
// usable and refusing to read it outright would turn a merely-suspicious
// filesystem state into an outage; a human fixing `chmod 600` is a
// perfectly adequate remediation once warned.
func LoadToken(profileName string) (*Token, error) {
	path, err := TokenPath(profileName)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if perm := info.Mode().Perm(); perm&^tokenFilePerm != 0 {
		slog.Warn("token file has looser permissions than required; run chmod 600",
			"path", path, "mode", perm.String(), "want", os.FileMode(tokenFilePerm).String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("profiles: read token %q: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var tok Token
	if err := dec.Decode(&tok); err != nil {
		return nil, fmt.Errorf("profiles: parse token %q: %w", path, err)
	}
	// A token file with an empty `token` or `url` field is unusable — the
	// HTTPS request would either go out Bearer-less (silent auth failure)
	// or fail the origin-bind check with an equally confusing "config=X,
	// token=" diagnostic. Reject up front with a message that names the
	// missing field so the operator knows to re-login.
	if tok.Token == "" {
		return nil, fmt.Errorf("profiles: token file %q has empty \"token\" field; re-login required", path)
	}
	if tok.URL == "" {
		return nil, fmt.Errorf("profiles: token file %q has empty \"url\" field; re-login required", path)
	}
	return &tok, nil
}
