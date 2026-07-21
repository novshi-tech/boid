package skills_test

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/skills"
)

// Phase 5b PR4 (docs/plans/phase5-shim-and-task-context.md「PR 分割案 > 5b」4):
// the boid-task / boid-orchestrate skills and boid-task's reference docs used
// to instruct the agent to read the dispatch-time context files under
// `~/.boid/context/`. This PR switches all of them to the Phase 5b
// task-context broker RPCs (`boid task current` / `instructions` / `env` /
// `payload`) instead — the guard tests below pin that the switch is complete
// (no leftover file-path reference anywhere in the deployed skill tree, not
// just the files this PR happened to touch first) and that the new CLI
// surface is actually documented, so a future edit that silently
// reintroduces a file reference (or drops the CLI reference) fails CI
// instead of drifting unnoticed until the 5b-6 cutover retires the file path
// for good.
//
// codex review on this PR (before merge) caught that the first version only
// grepped references/data-model.md, missing the same stale
// `~/.boid/context/{task,instructions,payload}.yaml` guidance still present
// in references/builtins.md and references/state-machine.md — both reachable
// from SKILL.md but outside its own file. Fixed by walking the entire
// deployed skill directory instead of naming files one at a time.

// legacyContextFileMarkers are the two on-disk path spellings the skill
// prose used before this PR (`~/.boid/context/...` in most places,
// `$HOME/.boid/context/...` where the shell examples need a real
// expansion). Both must be entirely gone from the deployed boid-task and
// boid-orchestrate skill trees — the file-materialization mechanism itself
// (sandbox_builder.go's contextFiles/buildEnvironmentYAML) is untouched and
// still runs in parallel until the 5b-6 cutover, but nothing under these two
// skills should tell the agent to read it anymore.
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

// deployedSkillTree deploys every embedded skill to a temp dir and returns
// {relative path -> content} for every regular file under baseDir/skillName,
// relative path always slash-separated and rooted at skillName itself (e.g.
// "boid-task/references/builtins.md") so assertion failure messages are
// unambiguous about which file is at fault.
func deployedSkillTree(t *testing.T, skillName string) map[string]string {
	t.Helper()
	baseDir := t.TempDir()
	if err := skills.DeployAll(baseDir); err != nil {
		t.Fatalf("DeployAll: %v", err)
	}
	root := filepath.Join(baseDir, skillName)
	out := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		content, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(baseDir, path)
		if rerr != nil {
			return rerr
		}
		out[filepath.ToSlash(rel)] = string(content)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(out) == 0 {
		t.Fatalf("no files found under deployed %s", skillName)
	}
	return out
}

func deployedSkillFile(t *testing.T, skillName, relPath string) string {
	t.Helper()
	tree := deployedSkillTree(t, skillName)
	key := skillName + "/" + relPath
	content, ok := tree[key]
	if !ok {
		t.Fatalf("deployed %s not found (have: %v)", key, tree)
	}
	return content
}

// TestBoidTaskSkillTree_NoLegacyContextFileReferences walks every file under
// the deployed boid-task skill (SKILL.md + all of references/*.md) — not
// just the files this PR edited first — so a stale file-path reference
// anywhere in the tree fails CI instead of silently surviving because the
// test only checked a hand-picked subset.
func TestBoidTaskSkillTree_NoLegacyContextFileReferences(t *testing.T) {
	for relPath, content := range deployedSkillTree(t, "boid-task") {
		for _, marker := range legacyContextFileMarkers {
			if strings.Contains(content, marker) {
				t.Errorf("%s still references %q — should read task context via `boid task ...` RPCs instead", relPath, marker)
			}
		}
	}
}

func TestBoidOrchestrateSkillTree_NoLegacyContextFileReferences(t *testing.T) {
	for relPath, content := range deployedSkillTree(t, "boid-orchestrate") {
		for _, marker := range legacyContextFileMarkers {
			if strings.Contains(content, marker) {
				t.Errorf("%s still references %q", relPath, marker)
			}
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

// TestBoidTaskSkillTree_InstructionsIsJobScoped pins the codex-caught Major
// finding: `boid task instructions` returns at most ONE element — the
// firing job's own routed instruction (internal/dispatcher/job_context.go's
// routedInstructionSlice) — never the task's full instruction history. The
// pre-fix SKILL.md and references/*.md described it as a growing array where
// reopen "appends" and earlier elements "remain as context", which does not
// match the RPC and would send an agent looking for prior-turn context that
// was never there. Every deployed file must avoid that specific claim.
func TestBoidTaskSkillTree_InstructionsIsJobScoped(t *testing.T) {
	wrongPhrases := []string{
		"appended to `instructions.yaml`",
		"appended as the last element",
		"Earlier elements remain as context",
		"Earlier elements are context only",
	}
	for relPath, content := range deployedSkillTree(t, "boid-task") {
		for _, phrase := range wrongPhrases {
			if strings.Contains(content, phrase) {
				t.Errorf("%s still describes `boid task instructions` as a growing/history array (%q) — it is job-scoped and returns at most one element", relPath, phrase)
			}
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

// TestDataModelDoc_ReadonlySemanticsAreCorrect pins the codex-caught Minor
// finding: `readonly` does not mean "the local filesystem is read-only" —
// the sandbox clone is *always* a normal read-write filesystem
// (sandbox_builder.go's /workspace bind comment: "under the clone model
// readonly is enforced by the gateway (transport-RO), not the local
// filesystem"). The doc must describe the actual mechanism (git push
// rejected by the gateway) and must not claim a permission check reveals
// writability.
func TestDataModelDoc_ReadonlySemanticsAreCorrect(t *testing.T) {
	content := deployedSkillFile(t, "boid-task", "references/data-model.md")
	if !strings.Contains(content, "git push") {
		t.Errorf("boid-task/references/data-model.md's readonly description should explain the actual mechanism (git push rejected by the gateway)")
	}
	if strings.Contains(content, "file permission checks tell you") {
		t.Errorf("boid-task/references/data-model.md should not claim file permission checks reveal writability — the clone is always read-write; only `boid task current --field readonly` is authoritative")
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

// TestBoidOrchestrateSkill_ModeDetectionVerifiesTaskExists pins the
// codex-caught Major finding: a bare `[ -n "$BOID_TASK_ID" ]` check is not a
// reliable task-less-session signal. Task-less sessions
// (sessionDispatcherAdapter.StartSession, internal/server/wire.go) pass
// project/workspace-configured `meta.Env` straight through as `spec.Env`,
// and BuildSandboxSpec's `setIfNonEmpty(env, "BOID_TASK_ID", spec.TaskID)`
// only ever *sets* the key when spec.TaskID is non-empty — it never clears a
// pre-existing "BOID_TASK_ID" that arrived via spec.Env when spec.TaskID ==
// "". A project/workspace `env:` block that happens to declare its own
// BOID_TASK_ID (e.g. a copy-pasted example) would leak into every task-less
// job for that project, misrouting boid-orchestrate into task management
// mode against a stale task. The mode-detection code must also confirm the
// task actually resolves (`boid task current`), not just that the env var is
// non-empty.
func TestBoidOrchestrateSkill_ModeDetectionVerifiesTaskExists(t *testing.T) {
	content := deployedSkillFile(t, "boid-orchestrate", "SKILL.md")
	if !strings.Contains(content, `boid task current`) {
		t.Fatalf("boid-orchestrate/SKILL.md missing `boid task current` reference at all")
	}
	// The mode-detection conditional must combine the env check with an
	// actual RPC success check, not rely on BOID_TASK_ID alone.
	if !strings.Contains(content, `-n "$BOID_TASK_ID"`) || !strings.Contains(content, "boid task current >") {
		t.Errorf("boid-orchestrate/SKILL.md's mode detection should check both `-n \"$BOID_TASK_ID\"` and that `boid task current` actually succeeds, not the env var alone")
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
