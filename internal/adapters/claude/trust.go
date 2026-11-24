package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// claudeHome is the HOME the claude child sees: RunContext.Env overrides the
// inherited environment.
func claudeHome(env map[string]string) string {
	if h := env["HOME"]; h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

// trustWorkspace marks dir as trusted in claude's ~/.claude.json so the
// folder-trust prompt never blocks a job that no one is watching.
func trustWorkspace(home, dir string) error {
	path := filepath.Join(home, ".claude.json")
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	mode := os.FileMode(0o600)
	if fi, statErr := os.Stat(path); statErr == nil {
		mode = fi.Mode().Perm()
	}

	top := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &top); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	}
	projects := map[string]map[string]json.RawMessage{}
	if p, ok := top["projects"]; ok {
		if err := json.Unmarshal(p, &projects); err != nil {
			return fmt.Errorf("parse %s projects: %w", path, err)
		}
	}
	entry := projects[dir]
	if string(entry["hasTrustDialogAccepted"]) == "true" {
		return nil
	}
	if entry == nil {
		entry = map[string]json.RawMessage{}
	}
	entry["hasTrustDialogAccepted"] = json.RawMessage("true")
	projects[dir] = entry
	p, err := json.Marshal(projects)
	if err != nil {
		return err
	}
	top["projects"] = p
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(home, ".claude.json.*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
