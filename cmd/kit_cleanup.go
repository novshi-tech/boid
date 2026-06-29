package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// cleanupResultFilename is the basename the boid-kit-init skill writes inside
// kitsDir to describe any legacy-* reorganisation it performed. cmd/kit.go
// reads this file after the sandbox exits and applies the corresponding
// workspace.kits updates that the sandbox itself cannot make because
// workspace.yaml lives outside its writable bind.
//
// The leading dot keeps it out of `boid kit list` (Registry only descends
// into directories) and the .json extension keeps it out of scanNewKitDirs
// (which only inspects *.yaml files).
const cleanupResultFilename = ".kit-init-cleanup-result.json"

// KitCleanupResult is the on-disk record the boid-kit-init skill writes when
// it has reorganised legacy-* kits. cmd/kit.go reads it after the sandbox
// exits and rewrites the matching workspace.kits entries.
type KitCleanupResult struct {
	Renamed []KitRename `json:"renamed,omitempty"`
	Deleted []KitDelete `json:"deleted,omitempty"`
}

// KitRename describes a kit-directory rename. workspace.kits entries equal to
// From are rewritten to To.
type KitRename struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// KitDelete describes a kit-directory removal. workspace.kits entries equal
// to Name are dropped. When ReplacedBy is non-empty it is appended (idempotent:
// only added when the slug is not already present in the same workspace).
type KitDelete struct {
	Name       string `json:"name"`
	ReplacedBy string `json:"replaced_by,omitempty"`
}

// applyKitCleanupResult reads the cleanup result file written by the
// boid-kit-init skill (if any), updates every workspace.yaml to follow the
// rename/delete mapping, deletes the result file, and prints a summary.
//
// A missing result file is not an error: the skill performed no legacy-*
// cleanup. workspacesDir empty falls back to the WorkspaceStore default
// (XDG_CONFIG_HOME/boid/workspaces) so production callers can pass "".
func applyKitCleanupResult(kitsDir, workspacesDir string, out io.Writer) error {
	path := filepath.Join(kitsDir, cleanupResultFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read cleanup result: %w", err)
	}

	var result KitCleanupResult
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("parse cleanup result %s: %w", path, err)
	}
	if err := validateCleanupResult(result); err != nil {
		return fmt.Errorf("invalid cleanup result %s: %w", path, err)
	}

	if len(result.Renamed) == 0 && len(result.Deleted) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove cleanup result: %w", err)
		}
		return nil
	}

	store := orchestrator.NewWorkspaceStore(workspacesDir)
	slugs, err := store.List()
	if err != nil {
		return fmt.Errorf("list workspaces: %w", err)
	}

	var summary []string
	for _, slug := range slugs {
		ws, loadErr := store.Load(slug)
		if loadErr != nil {
			// Skip unreadable workspaces but surface so the user notices —
			// silently dropping a workspace would let stale legacy references
			// linger after the user thinks the cleanup is done.
			fmt.Fprintf(out, "warning: skipping workspace %q (load failed: %v)\n", slug, loadErr)
			continue
		}
		before := append([]string(nil), ws.Kits...)
		after := applyCleanupToKitsList(before, result)
		if stringSlicesEqual(before, after) {
			continue
		}
		ws.Kits = after
		if err := store.Save(slug, ws); err != nil {
			return fmt.Errorf("save workspace %q: %w", slug, err)
		}
		summary = append(summary, fmt.Sprintf("  %s: %v -> %v", slug, before, after))
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		// Non-fatal but worth surfacing — a stale file would re-apply on the
		// next `boid kit init` run.
		fmt.Fprintf(out, "warning: could not remove cleanup result %s: %v\n", path, err)
	}

	if len(summary) > 0 {
		fmt.Fprintln(out, "applied legacy kit cleanup to workspaces:")
		for _, line := range summary {
			fmt.Fprintln(out, line)
		}
	}
	return nil
}

// validateCleanupResult rejects entries that would let us write garbage into
// workspace.yaml. The "write side" of each mapping (Renamed.To /
// Deleted.ReplacedBy) must pass ValidKitName so the slug we splice in is
// later loadable via WorkspaceStore. The "match side" (Renamed.From /
// Deleted.Name) is only ever compared for string equality against existing
// workspace.kits entries, so an invalid slug there just means "no match" —
// a stricter check would turn an upstream skill bug into a fatal block on
// `boid kit init` for what is otherwise a safe no-op. We still require the
// match side to be non-empty so a `""` entry can't silently mass-match.
func validateCleanupResult(r KitCleanupResult) error {
	for i, x := range r.Renamed {
		if x.From == "" {
			return fmt.Errorf("renamed[%d].from: empty", i)
		}
		if err := orchestrator.ValidKitName(x.To); err != nil {
			return fmt.Errorf("renamed[%d].to: %w", i, err)
		}
	}
	for i, x := range r.Deleted {
		if x.Name == "" {
			return fmt.Errorf("deleted[%d].name: empty", i)
		}
		if x.ReplacedBy != "" {
			if err := orchestrator.ValidKitName(x.ReplacedBy); err != nil {
				return fmt.Errorf("deleted[%d].replaced_by: %w", i, err)
			}
		}
	}
	return nil
}

// applyCleanupToKitsList returns a new kits slice with the rename and delete
// operations applied. Order is preserved relative to the input: each original
// entry is replaced in-place by its renamed slug (if any) or dropped (if
// deleted). For deleted entries with a replaced_by, the replacement slug is
// spliced into the same position. Duplicates are collapsed in case the
// replacement already appears elsewhere.
//
// Rename takes precedence over delete when the same slug appears in both
// mappings (which should never happen — the skill is expected to keep them
// disjoint — but a safe deterministic fallback is preferable to ambiguity).
func applyCleanupToKitsList(kits []string, r KitCleanupResult) []string {
	rename := make(map[string]string, len(r.Renamed))
	for _, x := range r.Renamed {
		rename[x.From] = x.To
	}
	deleteMap := make(map[string]string, len(r.Deleted))
	for _, x := range r.Deleted {
		deleteMap[x.Name] = x.ReplacedBy
	}
	seen := make(map[string]bool, len(kits)+len(r.Deleted))
	out := make([]string, 0, len(kits))
	add := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	for _, name := range kits {
		if to, ok := rename[name]; ok {
			add(to)
			continue
		}
		if replacement, ok := deleteMap[name]; ok {
			if replacement != "" {
				add(replacement)
			}
			continue
		}
		add(name)
	}
	return out
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
