package skills_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/skills"
)

// Phase 5b PR4 (docs/plans/phase5-shim-and-task-context.md「PR 分割案 > 5b」4):
// the boid-task / boid-orchestrate skills and boid-task's data-model
// reference doc used to instruct the agent to read the dispatch-time context
// files under `~/.boid/context/`. This PR switches all three to the Phase 5b
// task-context broker RPCs (`boid task current` / `instructions` / `env` /
// `payload`) instead — the two guard tests below pin that the switch is
// complete (no leftover file-path reference) and that the new CLI surface is
// actually documented, so a future edit that silently reintroduces a file
// reference (or drops the CLI reference) fails CI instead of drifting
// unnoticed until the 5b-6 cutover retires the file path for good.

// legacyContextFileMarkers are the two on-disk path spellings the skill
// prose used before this PR (`~/.boid/context/...` in most places,
// `$HOME/.boid/context/...` where the shell examples need a real
// expansion). Both must be entirely gone from the three files below —
// the file-materialization mechanism itself (sandbox_builder.go's
// contextFiles/buildEnvironmentYAML) is untouched and still runs in
// parallel until the 5b-6 cutover, but nothing in these three files should
// tell the agent to read it anymore.
var legacyContextFileMarkers = []string{
	"~/.boid/context/",
	"$HOME/.boid/context/",
}

// taskContextCLICommands are the four Phase 5b PR1 subcommands
// (internal/sandbox/boid_shim_task_context.go's taskContextOps) that replace
// the four context files.
var taskContextCLICommands = []string{
	"boid task current",
	"boid task instructions",
	"boid task env",
	"boid task payload",
}

func deployedSkillFile(t *testing.T, skillName, relPath string) string {
	t.Helper()
	baseDir := t.TempDir()
	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(baseDir, skillName, relPath))
	if err != nil {
		t.Fatalf("read %s/%s: %v", skillName, relPath, err)
	}
	return string(content)
}

func TestBoidTaskSkill_NoLegacyContextFileReferences(t *testing.T) {
	content := deployedSkillFile(t, "boid-task", "SKILL.md")
	for _, marker := range legacyContextFileMarkers {
		if strings.Contains(content, marker) {
			t.Errorf("boid-task/SKILL.md still references %q — should read task context via `boid task ...` RPCs instead", marker)
		}
	}
}

func TestBoidTaskSkill_ReferencesTaskContextCLI(t *testing.T) {
	content := deployedSkillFile(t, "boid-task", "SKILL.md")
	for _, cmd := range taskContextCLICommands {
		if !strings.Contains(content, cmd) {
			t.Errorf("boid-task/SKILL.md missing reference to %q", cmd)
		}
	}
}

// TestBoidTaskSkill_ReadonlyViaTaskCurrent pins that Step 0's mode
// determination reads readonly from `boid task current` (TaskSnapshot.Readonly,
// added in this PR) rather than the retired environment.yaml `readonly` key.
func TestBoidTaskSkill_ReadonlyViaTaskCurrent(t *testing.T) {
	content := deployedSkillFile(t, "boid-task", "SKILL.md")
	if !strings.Contains(content, "boid task current --field readonly") {
		t.Errorf("boid-task/SKILL.md should determine mode via `boid task current --field readonly`")
	}
	if strings.Contains(content, "environment.yaml") {
		t.Errorf("boid-task/SKILL.md should no longer mention environment.yaml (superseded by `boid task env`)")
	}
}

// TestBoidTaskSkill_ExecutorRules_NoFilesystemFieldReferences pins that the
// executor-mode rules no longer point at environment.yaml's retired
// filesystem.project_dir / filesystem.writable fields (decision 4 in the plan
// doc: the container model makes the filesystem "見たまんま" — observable
// directly via pwd/permissions — so there is nothing left to read them from).
func TestBoidTaskSkill_ExecutorRules_NoFilesystemFieldReferences(t *testing.T) {
	content := deployedSkillFile(t, "boid-task", "SKILL.md")
	for _, marker := range []string{"filesystem.project_dir", "filesystem.writable", "network.restricted"} {
		if strings.Contains(content, marker) {
			t.Errorf("boid-task/SKILL.md still references retired environment.yaml field %q", marker)
		}
	}
}

func TestDataModelDoc_NoLegacyContextFileReferences(t *testing.T) {
	content := deployedSkillFile(t, "boid-task", "references/data-model.md")
	for _, marker := range legacyContextFileMarkers {
		if strings.Contains(content, marker) {
			t.Errorf("boid-task/references/data-model.md still references %q", marker)
		}
	}
}

func TestDataModelDoc_ReferencesTaskContextCLI(t *testing.T) {
	content := deployedSkillFile(t, "boid-task", "references/data-model.md")
	for _, cmd := range taskContextCLICommands {
		if !strings.Contains(content, cmd) {
			t.Errorf("boid-task/references/data-model.md missing reference to %q", cmd)
		}
	}
}

// TestDataModelDoc_EnvironmentSectionIsReduced pins decision 4 in the plan
// doc: `boid task env`'s schema is the 2-field reduced WorkspaceEnvView
// (internal/dispatcher/workspace_env_view.go) — allowed_domains and
// host_commands only. The legacy environment.yaml sections that used to
// document sandbox.*/filesystem.*/worktree/tools/session.* must be gone from
// the reference doc, since none of them survive in `boid task env`'s output.
func TestDataModelDoc_EnvironmentSectionIsReduced(t *testing.T) {
	content := deployedSkillFile(t, "boid-task", "references/data-model.md")
	for _, want := range []string{"allowed_domains", "host_commands"} {
		if !strings.Contains(content, want) {
			t.Errorf("boid-task/references/data-model.md missing %q in the `boid task env` schema section", want)
		}
	}
	for _, retired := range []string{"filesystem.project_dir", "filesystem.writable", "sandbox.kind", "session.harness", "worktree:", "tools:\n"} {
		if strings.Contains(content, retired) {
			t.Errorf("boid-task/references/data-model.md still documents retired environment.yaml field %q", retired)
		}
	}
}

func TestBoidOrchestrateSkill_NoLegacyContextFileReferences(t *testing.T) {
	content := deployedSkillFile(t, "boid-orchestrate", "SKILL.md")
	for _, marker := range legacyContextFileMarkers {
		if strings.Contains(content, marker) {
			t.Errorf("boid-orchestrate/SKILL.md still references %q", marker)
		}
	}
}

func TestBoidOrchestrateSkill_ReferencesTaskCurrentCLI(t *testing.T) {
	content := deployedSkillFile(t, "boid-orchestrate", "SKILL.md")
	if !strings.Contains(content, "boid task current") {
		t.Errorf("boid-orchestrate/SKILL.md missing reference to `boid task current`")
	}
	if strings.Contains(content, "environment.yaml") {
		t.Errorf("boid-orchestrate/SKILL.md should no longer mention environment.yaml")
	}
}

// repoRootForTest resolves the repository root via git, mirroring the
// pattern internal/api/task_notify_doneverify_test.go already established
// for tests that need to read non-embedded files by repo-relative path.
func repoRootForTest(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not in a git repo: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestHookContractDoc_EnvironmentYAMLReplacedByCLI pins the narrower Phase 5b
// PR4 scope for docs/{ja,en}/reference/hook-contract.md: only the
// environment.yaml row (L19) moves to `boid task env` — the doc's other
// context-file references (task.yaml/instructions.yaml/payload.json) are
// intentionally untouched in this PR, since they describe the still-current
// contract for `hooks[].command` inline shell hooks (which this PR's scope
// excludes; see docs/plans/phase5-shim-and-task-context.md「PR 分割案 > 5b」4).
func TestHookContractDoc_EnvironmentYAMLReplacedByCLI(t *testing.T) {
	root := repoRootForTest(t)
	for _, relPath := range []string{
		"docs/ja/reference/hook-contract.md",
		"docs/en/reference/hook-contract.md",
	} {
		content, err := os.ReadFile(filepath.Join(root, relPath))
		if err != nil {
			t.Fatalf("read %s: %v", relPath, err)
		}
		s := string(content)
		if strings.Contains(s, "environment.yaml") {
			t.Errorf("%s still references environment.yaml — should point at `boid task env` instead", relPath)
		}
		if !strings.Contains(s, "boid task env") {
			t.Errorf("%s missing reference to `boid task env`", relPath)
		}
	}
}
