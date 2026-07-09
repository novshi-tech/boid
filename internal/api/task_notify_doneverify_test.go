package api

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// doneVerifyProjects is a minimal ProjectWorkDirLookup that points every project
// at a fixed working directory (the boid repo itself in these tests).
type doneVerifyProjects struct{ workdir string }

func (p doneVerifyProjects) GetProject(id string) (*orchestrator.Project, error) {
	return &orchestrator.Project{ID: id, WorkDir: p.workdir}, nil
}

func repoRootForTest(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not in a git repo: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func mustOut(t *testing.T, c *exec.Cmd) string {
	t.Helper()
	out, err := c.Output()
	if err != nil {
		t.Fatalf("cmd %v: %v", c.Args, err)
	}
	return strings.TrimSpace(string(out))
}

func releasePayload(commit, branch string, pushed bool) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"artifact": map[string]any{
			"report": map[string]any{
				"release": map[string]any{
					"commit": commit, "branch": branch, "pushed": pushed,
				},
			},
		},
	})
	return b
}

// TestVerifyDoneClaim's cases with a non-empty commit and non-nil Projects
// (e.g. "real commit passes", "fabricated commit blocked") now also exercise
// the git-gateway-cutover PR1 `gitFetchOrigin` step against this repo's real
// origin. That fetch is best-effort (see gitFetchOrigin), so these cases pass
// the same way whether or not the environment has network access to origin —
// a failed fetch just falls back to whatever objects are already local, which
// is always sufficient here since the test repo already has the object.
func TestVerifyDoneClaim(t *testing.T) {
	root := repoRootForTest(t)
	head := mustOut(t, exec.Command("git", "-C", root, "rev-parse", "HEAD"))
	ctx := context.Background()
	proj := doneVerifyProjects{workdir: root}

	cases := []struct {
		name    string
		task    *orchestrator.Task
		nilProj bool
		wantErr bool
	}{
		{"open children block done", &orchestrator.Task{OpenChildCount: 2}, false, true},
		{"real commit passes", &orchestrator.Task{Payload: releasePayload(head, "", false)}, false, false},
		{"fabricated commit blocked", &orchestrator.Task{Payload: releasePayload("deadbeefcafefeed1234", "", false)}, false, true},
		{"no release field skips", &orchestrator.Task{Payload: json.RawMessage(`{"artifact":{"report":{"summary":"x"}}}`)}, false, false},
		{"nil projects skips commit check", &orchestrator.Task{Payload: releasePayload("deadbeefcafefeed1234", "", false)}, true, false},
		{"empty task passes", &orchestrator.Task{}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &TaskAppService{Projects: proj}
			if tc.nilProj {
				s.Projects = nil
			}
			err := s.verifyDoneClaim(ctx, tc.task)
			if tc.wantErr && err == nil {
				t.Fatalf("expected rejection, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected pass, got rejection: %s", err.Message)
			}
		})
	}
}

func TestReleaseClaim(t *testing.T) {
	c, b, p := releaseClaim(releasePayload("abc1234", "feature/x", true))
	if c != "abc1234" || b != "feature/x" || !p {
		t.Fatalf("got commit=%q branch=%q pushed=%v", c, b, p)
	}
	if c2, _, _ := releaseClaim(json.RawMessage(`{"artifact":{"report":{"release":{"commit":"not-a-hash"}}}}`)); c2 != "" {
		t.Fatalf("non-hash commit should be dropped, got %q", c2)
	}
	if c3, _, _ := releaseClaim(nil); c3 != "" {
		t.Fatalf("empty payload should yield empty commit, got %q", c3)
	}
}

// TestGitFetchOrigin only exercises the graceful-degradation path (no origin
// remote to fetch from): it must return promptly without panicking rather than
// blocking the caller. The "fetch actually pulls new objects" behavior is
// exercised indirectly by TestVerifyDoneClaim's "real commit passes" case,
// which runs against this repo's real origin.
func TestGitFetchOrigin(t *testing.T) {
	ctx := context.Background()
	// t.TempDir() is not a git repository at all, so `git fetch origin` fails
	// immediately (no repo, let alone a remote named origin). gitFetchOrigin
	// must swallow this rather than propagating an error to the caller.
	gitFetchOrigin(ctx, t.TempDir())
}

func TestGitObjectExists(t *testing.T) {
	root := repoRootForTest(t)
	head := mustOut(t, exec.Command("git", "-C", root, "rev-parse", "HEAD"))
	ctx := context.Background()

	if ex, conc := gitObjectExists(ctx, root, head); !ex || !conc {
		t.Fatalf("HEAD should exist conclusively, got exists=%v conclusive=%v", ex, conc)
	}
	if ex, conc := gitObjectExists(ctx, root, "deadbeefcafefeed1234"); ex || !conc {
		t.Fatalf("fake hash should be conclusively absent, got exists=%v conclusive=%v", ex, conc)
	}
	if _, conc := gitObjectExists(ctx, t.TempDir(), "deadbeefcafefeed1234"); conc {
		t.Fatalf("non-repo dir should be inconclusive (so callers skip, not block)")
	}
}
