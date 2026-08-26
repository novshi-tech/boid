package packconformance

import (
	"testing"

	"github.com/novshi-tech/boid/internal/skills"
)

// TestBoidCommandPattern pins the exact calibration boidCommandPattern was
// tuned against — the real corpus of 13 reference skills in boid-api-skills
// (grepped while designing this check; see boidCommandPattern's own doc
// comment). Every "should match" case here is a real, verbatim boid CLI
// invocation found in that corpus; every "should NOT match" case is a real
// phrase from the same corpus that must NOT be flagged (the env-var
// contract carve-out, and two literal "boid-xxx" strings that are not skill
// references at all).
func TestBoidCommandPattern(t *testing.T) {
	cases := []struct {
		text      string
		wantMatch bool
	}{
		// Real violations (verbatim from boid-api-skills as of 2026-08-27).
		{"boid workspace services add board-api", true},
		{"boid config get                        # services: ブロック全体", true},
		{"boid workspace services list <slug>", true},
		{"boid secret set", true},
		{"boid secret oauth login freee", true},
		{"ホスト側からなら `boid job log <id>` で確認できる", true},
		{"`boid task create --project fixture` を実行する", true},

		// The env-var / gateway-mechanism carve-out: describing $BOID_API_BASE
		// is knowledge a Pack skill is explicitly allowed to have (docs/plans/
		// signal-ingest-detailed-design.md §7.1) — never a command.
		{"認証は環境変数 BOID_API_BASE 経由で行う", false},
		{"$BOID_API_BASE/<service>/rest/api/3/...", false},
		{"BOID_API_CA_FILE が未設定であれば", false},
		{"BOID_SIGNAL_SERVICE / BOID_SIGNAL_CONNECTOR", false},

		// "boid" glued directly onto a Japanese particle or another word
		// with no space — common in this corpus's prose, never a command.
		{"boidのAPIゲートウェイ経由で呼び出す", false},
		{"boid経由で呼び出す", false},
		{"boidサンドボックス内からは", false},
		{"boidリポジトリ", false},

		// "boid-xxx" literal strings that are NOT command/skill references
		// (a hostname placeholder and a User-Agent value, both taken
		// verbatim from the real corpus).
		{"https://boid-gateway:<port>/api/<job-token>", false},
		{`"User-Agent": "boid-job"`, false},

		// Known accepted residual false-positive class (documented on
		// boidCommandPattern itself): English prose with a verb glued
		// directly after "boid " reads exactly like a subcommand. The real
		// (Japanese) corpus this pattern was calibrated against never hits
		// this shape — "boidの..."/"boid経由で..." glue with no space
		// instead (see the "glued" cases above) — so this is pinned as a
		// known true-positive-shaped false positive, not a bug to fix.
		{"boid is a personal AI orchestrator", true},
	}

	for _, c := range cases {
		got := boidCommandPattern.MatchString(c.text)
		if got != c.wantMatch {
			t.Errorf("boidCommandPattern.MatchString(%q) = %v, want %v", c.text, got, c.wantMatch)
		}
	}
}

// TestFindBoidReferences_BuiltinSkillNamesKnownFalsePositives pins that the
// builtin-skill-name half of findBoidReferences — keyed off the REAL
// skills.EmbeddedSkillNames() (boid-orchestrate/boid-signal/boid-task/
// boid-web), not a hardcoded/possibly-stale copy — does not fire on
// "boid-gateway"/"boid-job", two "boid-xxx" strings from the real
// boid-api-skills corpus that are a hostname placeholder and a User-Agent
// value, not skill references.
func TestFindBoidReferences_BuiltinSkillNamesKnownFalsePositives(t *testing.T) {
	names := skills.EmbeddedSkillNames()
	for _, n := range names {
		if n == "boid-gateway" || n == "boid-job" {
			t.Fatalf("test fixture assumption broken: %q is now a real builtin skill name", n)
		}
	}
	text := "https://boid-gateway:<port>/api/<job-token>\n\"User-Agent\": \"boid-job\""
	refs := findBoidReferences(text, names)
	if len(refs) != 0 {
		t.Errorf("findBoidReferences(%q) = %+v, want none", text, refs)
	}
}

// TestFindBoidReferences_BuiltinSkillNameMatch pins the positive case: a
// document literally naming a builtin skill (e.g. "see the boid-signal
// skill") is flagged even though it contains no "boid <subcommand>" shaped
// text at all.
func TestFindBoidReferences_BuiltinSkillNameMatch(t *testing.T) {
	refs := findBoidReferences("see the boid-signal skill for details", []string{"boid-signal"})
	if len(refs) != 1 || refs[0].match != "boid-signal" {
		t.Errorf("findBoidReferences(...) = %+v, want one match on \"boid-signal\"", refs)
	}
}
