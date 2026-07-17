package profiles

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/novshi-tech/boid/internal/config"
)

// WriteToken serializes tok as JSON and atomically writes it to
// ~/.config/boid/tokens/<profileName>.json (decision 2, docs/plans/
// cli-remote-connection.md: "0600、親 dir 0700"), creating the tokens/
// directory (0700) first if it does not exist yet. The write goes through
// config.WriteFileAtomic (temp file in the same directory, fsynced, renamed
// into place) so a reader — including this same process's own LoadToken, or
// a concurrent `boid` invocation — never observes a partially written file.
func WriteToken(profileName string, tok *Token) error {
	path, err := TokenPath(profileName)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return fmt.Errorf("profiles: marshal token: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("profiles: mkdir %q: %w", dir, err)
	}
	if err := config.WriteFileAtomic(path, data, tokenFilePerm); err != nil {
		return fmt.Errorf("profiles: write token %q: %w", path, err)
	}
	return nil
}

// DeleteToken removes the token file for profileName. A missing file is not
// an error — `boid logout` calls this unconditionally as part of an
// idempotent cleanup, so a second logout (or a logout of a profile that was
// hand-removed from config.yaml without ever running `boid logout`) must
// not fail merely because there was nothing left to delete.
func DeleteToken(profileName string) error {
	path, err := TokenPath(profileName)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("profiles: delete token %q: %w", path, err)
	}
	return nil
}
