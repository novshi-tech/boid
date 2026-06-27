package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// writeCleanupResult is a small helper that marshals the given result and
// writes it to kitsDir/.kit-init-cleanup-result.json.
func writeCleanupResult(t *testing.T, kitsDir string, r KitCleanupResult) {
	t.Helper()
	if err := os.MkdirAll(kitsDir, 0o755); err != nil {
		t.Fatalf("mkdir kitsDir: %v", err)
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kitsDir, cleanupResultFilename), data, 0o644); err != nil {
		t.Fatalf("write cleanup file: %v", err)
	}
}

// writeWorkspaceYAML pre-populates a workspace.yaml with the given kits.
func writeWorkspaceYAML(t *testing.T, wsDir, slug string, kits []string) {
	t.Helper()
	store := orchestrator.NewWorkspaceStore(wsDir)
	if err := store.Save(slug, &orchestrator.WorkspaceMeta{Kits: kits}); err != nil {
		t.Fatalf("save workspace %q: %v", slug, err)
	}
}

// loadWorkspaceKits returns the kits slice of a stored workspace, for assertions.
func loadWorkspaceKits(t *testing.T, wsDir, slug string) []string {
	t.Helper()
	store := orchestrator.NewWorkspaceStore(wsDir)
	ws, err := store.Load(slug)
	if err != nil {
		t.Fatalf("load workspace %q: %v", slug, err)
	}
	return ws.Kits
}

// TestApplyCleanupToKitsList covers the pure mapping logic in isolation.
func TestApplyCleanupToKitsList(t *testing.T) {
	cases := []struct {
		name   string
		kits   []string
		result KitCleanupResult
		want   []string
	}{
		{
			name: "rename in place preserves order",
			kits: []string{"a", "legacy-foo", "b"},
			result: KitCleanupResult{
				Renamed: []KitRename{{From: "legacy-foo", To: "github-cli-foo"}},
			},
			want: []string{"a", "github-cli-foo", "b"},
		},
		{
			name: "delete without replacement drops entry",
			kits: []string{"a", "legacy-bar", "b"},
			result: KitCleanupResult{
				Deleted: []KitDelete{{Name: "legacy-bar"}},
			},
			want: []string{"a", "b"},
		},
		{
			name: "delete with replacement splices in",
			kits: []string{"a", "legacy-baz", "b"},
			result: KitCleanupResult{
				Deleted: []KitDelete{{Name: "legacy-baz", ReplacedBy: "github-cli"}},
			},
			want: []string{"a", "github-cli", "b"},
		},
		{
			name: "delete with replacement that already exists dedupes",
			kits: []string{"github-cli", "legacy-baz", "b"},
			result: KitCleanupResult{
				Deleted: []KitDelete{{Name: "legacy-baz", ReplacedBy: "github-cli"}},
			},
			want: []string{"github-cli", "b"},
		},
		{
			name: "rename to existing slug dedupes",
			kits: []string{"node", "legacy-node"},
			result: KitCleanupResult{
				Renamed: []KitRename{{From: "legacy-node", To: "node"}},
			},
			want: []string{"node"},
		},
		{
			name: "no-ops when no mapping matches",
			kits: []string{"a", "b"},
			result: KitCleanupResult{
				Renamed: []KitRename{{From: "x", To: "y"}},
				Deleted: []KitDelete{{Name: "z"}},
			},
			want: []string{"a", "b"},
		},
		{
			name: "rename and delete combined",
			kits: []string{"legacy-one", "keep", "legacy-two"},
			result: KitCleanupResult{
				Renamed: []KitRename{{From: "legacy-one", To: "myapp-tools"}},
				Deleted: []KitDelete{{Name: "legacy-two", ReplacedBy: "docker"}},
			},
			want: []string{"myapp-tools", "keep", "docker"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := applyCleanupToKitsList(tc.kits, tc.result)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("applyCleanupToKitsList = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestApplyKitCleanupResult_MissingFileIsNoOp verifies the absence of the
// result file is treated as "skill performed no cleanup" — not as an error.
func TestApplyKitCleanupResult_MissingFileIsNoOp(t *testing.T) {
	kitsDir := t.TempDir()
	wsDir := t.TempDir()
	writeWorkspaceYAML(t, wsDir, "my-ws", []string{"keep-me"})

	var buf bytes.Buffer
	if err := applyKitCleanupResult(kitsDir, wsDir, &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := loadWorkspaceKits(t, wsDir, "my-ws"); !reflect.DeepEqual(got, []string{"keep-me"}) {
		t.Errorf("workspace kits changed unexpectedly: %v", got)
	}
}

// TestApplyKitCleanupResult_EmptyResultDeletesFile verifies that a result file
// with no entries is silently consumed (file removed, no workspace touched).
func TestApplyKitCleanupResult_EmptyResultDeletesFile(t *testing.T) {
	kitsDir := t.TempDir()
	wsDir := t.TempDir()
	writeWorkspaceYAML(t, wsDir, "my-ws", []string{"keep-me"})
	writeCleanupResult(t, kitsDir, KitCleanupResult{})

	var buf bytes.Buffer
	if err := applyKitCleanupResult(kitsDir, wsDir, &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(kitsDir, cleanupResultFilename)); !os.IsNotExist(err) {
		t.Errorf("result file should have been removed: %v", err)
	}
}

// TestApplyKitCleanupResult_RewritesWorkspacesAndRemovesFile is the integration
// happy path: rename + delete-with-replacement entries get applied across all
// workspaces and the result file is consumed.
func TestApplyKitCleanupResult_RewritesWorkspacesAndRemovesFile(t *testing.T) {
	kitsDir := t.TempDir()
	wsDir := t.TempDir()

	writeWorkspaceYAML(t, wsDir, "alpha", []string{"go", "legacy-old", "legacy-gh"})
	writeWorkspaceYAML(t, wsDir, "beta", []string{"legacy-gh", "node"}) // shared reference

	writeCleanupResult(t, kitsDir, KitCleanupResult{
		Renamed: []KitRename{{From: "legacy-old", To: "old-tools"}},
		Deleted: []KitDelete{{Name: "legacy-gh", ReplacedBy: "github-cli"}},
	})

	var buf bytes.Buffer
	if err := applyKitCleanupResult(kitsDir, wsDir, &buf); err != nil {
		t.Fatalf("apply: %v", err)
	}

	wantAlpha := []string{"go", "old-tools", "github-cli"}
	if got := loadWorkspaceKits(t, wsDir, "alpha"); !reflect.DeepEqual(got, wantAlpha) {
		t.Errorf("alpha kits = %v, want %v", got, wantAlpha)
	}
	wantBeta := []string{"github-cli", "node"}
	if got := loadWorkspaceKits(t, wsDir, "beta"); !reflect.DeepEqual(got, wantBeta) {
		t.Errorf("beta kits = %v, want %v", got, wantBeta)
	}

	if _, err := os.Stat(filepath.Join(kitsDir, cleanupResultFilename)); !os.IsNotExist(err) {
		t.Errorf("result file should have been removed: %v", err)
	}

	// Summary line emitted for each changed workspace.
	if !strings.Contains(buf.String(), "alpha:") || !strings.Contains(buf.String(), "beta:") {
		t.Errorf("summary missing per-workspace lines:\n%s", buf.String())
	}
}

// TestApplyKitCleanupResult_NoMatchLeavesWorkspaceUntouched verifies that
// workspaces whose kits list does not intersect the cleanup result are not
// re-written (avoids needless yaml churn / mtime updates).
func TestApplyKitCleanupResult_NoMatchLeavesWorkspaceUntouched(t *testing.T) {
	kitsDir := t.TempDir()
	wsDir := t.TempDir()

	writeWorkspaceYAML(t, wsDir, "unrelated", []string{"go", "node"})
	writeCleanupResult(t, kitsDir, KitCleanupResult{
		Renamed: []KitRename{{From: "legacy-x", To: "y"}},
	})

	wsPath := filepath.Join(wsDir, "unrelated.yaml")
	preInfo, err := os.Stat(wsPath)
	if err != nil {
		t.Fatalf("stat workspace before: %v", err)
	}

	var buf bytes.Buffer
	if err := applyKitCleanupResult(kitsDir, wsDir, &buf); err != nil {
		t.Fatalf("apply: %v", err)
	}

	postInfo, err := os.Stat(wsPath)
	if err != nil {
		t.Fatalf("stat workspace after: %v", err)
	}
	if !postInfo.ModTime().Equal(preInfo.ModTime()) {
		t.Errorf("workspace.yaml was re-written despite no matching kits")
	}
}

// TestApplyKitCleanupResult_InvalidNameFails verifies that a result file with
// an invalid kit slug is rejected (we never write a malformed name into
// workspace.yaml).
func TestApplyKitCleanupResult_InvalidNameFails(t *testing.T) {
	kitsDir := t.TempDir()
	wsDir := t.TempDir()
	writeWorkspaceYAML(t, wsDir, "my-ws", []string{"legacy-x"})

	// Bypass writeCleanupResult to skip Go-side marshalling, since the bad
	// name would fail ValidKitName but is valid JSON.
	if err := os.WriteFile(
		filepath.Join(kitsDir, cleanupResultFilename),
		[]byte(`{"renamed":[{"from":"legacy-x","to":"BAD NAME"}]}`),
		0o644,
	); err != nil {
		t.Fatalf("write bad result: %v", err)
	}

	var buf bytes.Buffer
	err := applyKitCleanupResult(kitsDir, wsDir, &buf)
	if err == nil || !strings.Contains(err.Error(), "renamed[0].to") {
		t.Errorf("expected validation error mentioning renamed[0].to, got: %v", err)
	}
	// On rejection the workspace must remain unchanged.
	if got := loadWorkspaceKits(t, wsDir, "my-ws"); !reflect.DeepEqual(got, []string{"legacy-x"}) {
		t.Errorf("workspace was modified despite validation failure: %v", got)
	}
}
