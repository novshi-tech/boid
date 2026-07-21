package skills_test

import (
	"strings"
	"testing"
)

// Phase 5b PR7 (docs/plans/phase5-shim-and-task-context.md「PR 分割案 > 5b」7):
// the boid-task skill's "done" contract (Writing the final report / the
// release sub-step of Reporting your own done) switches from
// `--payload-file` (BoidOpTaskUpdate's top-level shallow merge, no trait
// awareness) to `--payload-patch @-` (BoidOpTaskUpdatePayloadPatch, the
// job_done-equivalent trait-gated merge) as the primary path. The guards
// below mirror the 5b-4 context_cli_test.go pattern: walk the whole deployed
// skill tree (not just the files this PR happened to touch first) so a
// future edit that silently reintroduces the old primary command, or an
// instruction to write the raw fallback file directly, fails CI instead of
// drifting unnoticed.
//
// decision 6/7 (docs/plans/phase5-shim-and-task-context.md) keeps the
// file-based fallback ($HOME/.boid/output/payload_patch.json, plus the
// $HOME/.boid job-scoped tmpfs overlay that isolates it — wiring-seams.md
// #13's PR6 update) alive until this fallback is retired outright in a
// later phase, so its existence is still mentioned in prose for accuracy —
// only the "here is the command an agent should run" recommendation moves.

// legacyFinalReportCommand is the exact command the skill used to recommend
// for writing artifact.report (and its release sub-field) before this PR.
const legacyFinalReportCommand = `--payload-file - <<EOF`

func TestBoidTaskSkill_FinalReportUsesPayloadPatchCLI(t *testing.T) {
	content := deployedSkillFile(t, "boid-task", "SKILL.md")
	if !strings.Contains(content, "boid task update --payload-patch @-") {
		t.Errorf("boid-task/SKILL.md's final-report section should recommend `boid task update --payload-patch @-` as the primary done-reporting command")
	}
}

func TestBoidTaskSkillTree_NoLegacyPayloadFileForFinalReport(t *testing.T) {
	for relPath, content := range deployedSkillTree(t, "boid-task") {
		if strings.Contains(content, legacyFinalReportCommand) {
			t.Errorf("%s still recommends %q for reporting — should use `boid task update --payload-patch @-` instead (--payload-file itself may still be documented as a general-purpose flag elsewhere)", relPath, legacyFinalReportCommand)
		}
	}
}

// TestBoidTaskSkillTree_NoDirectPayloadPatchFileWriteInstruction pins that
// nothing in the deployed boid-task skill tree instructs an agent to write
// the file-based fallback directly (a shell redirection into
// $HOME/.boid/output/payload_patch.json). The fallback's existence may still
// be *described* (it is real, load-bearing infrastructure — see decision 7),
// but it must never be presented as something an agent should write to.
func TestBoidTaskSkillTree_NoDirectPayloadPatchFileWriteInstruction(t *testing.T) {
	forbiddenWriteMarkers := []string{
		`> "$HOME/.boid/output/payload_patch.json"`,
		`>"$HOME/.boid/output/payload_patch.json"`,
		`> $HOME/.boid/output/payload_patch.json`,
	}
	for relPath, content := range deployedSkillTree(t, "boid-task") {
		for _, marker := range forbiddenWriteMarkers {
			if strings.Contains(content, marker) {
				t.Errorf("%s instructs writing payload_patch.json directly (%q) — use `boid task update --payload-patch @-` instead", relPath, marker)
			}
		}
	}
}

// TestBuiltinsDoc_DocumentsPayloadPatchFlag pins that references/builtins.md
// (the flag-reference doc SKILL.md links to) documents --payload-patch
// alongside the existing task-update flags, and explains it in terms of the
// actual merge semantics (trait-gated, MergePayloadPatch) rather than as a
// cosmetic rename of --payload-file — this is the specific skill/
// implementation-drift risk flagged for this PR (mirrors the 5b-4 readonly /
// 5b-5 $HOME-persistence lesson: a skill claim must match what the code
// actually does).
func TestBuiltinsDoc_DocumentsPayloadPatchFlag(t *testing.T) {
	content := deployedSkillFile(t, "boid-task", "references/builtins.md")
	if !strings.Contains(content, "--payload-patch") {
		t.Fatalf("boid-task/references/builtins.md missing --payload-patch documentation")
	}
	if !strings.Contains(content, "MergePayloadPatch") {
		t.Errorf("boid-task/references/builtins.md should name the actual merge function (MergePayloadPatch) to distinguish --payload-patch from --payload-file's simpler top-level merge")
	}
	if !strings.Contains(content, "traits.produces") {
		t.Errorf("boid-task/references/builtins.md should explain that --payload-patch is gated by the firing hook's traits.produces (the job_done-equivalent semantics)")
	}
}
