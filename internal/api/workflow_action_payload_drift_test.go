package api

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestSideEffectConsumesPayload_MatchesMetaprojectClient ties the daemon's set
// to the copy the boid-metaproject skill's python client carries.
//
// The copy is not avoidable: BoidCLI.send_action refuses to attach a payload to
// an action type the daemon would merge rather than consume, and it decides
// that before the call, with no way to ask. What IS avoidable is the copy going
// stale unnoticed — which is exactly what happened when boid #982 added
// `child_dropped` here: the client's set stayed at six, so its `drop-child`
// verb was rejected locally and never reached boid at all. Nothing failed; the
// verb simply did nothing for weeks.
//
// Reading the python source with a regex rather than importing it is the whole
// point: the check has to work from `go test` with no python toolchain, and the
// constant is a flat literal by construction, so a parser would buy nothing. If
// the literal's SHAPE ever changes, the anchor below stops matching and this
// test fails loudly rather than silently comparing nothing — the "0 scanned
// looks like 0 violations" trap.
func TestSideEffectConsumesPayload_MatchesMetaprojectClient(t *testing.T) {
	path := filepath.Join("..", "skills", "data", "boid-metaproject", "scripts", "boidmeta", "boid_store.py")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the metaproject client: %v", err)
	}

	block := regexp.MustCompile(`(?s)PAYLOAD_CONSUMING\s*=\s*frozenset\(\{(.*?)\}\)`).FindSubmatch(src)
	if block == nil {
		t.Fatalf("could not find PAYLOAD_CONSUMING's literal in %s — if its shape changed, update this test rather than deleting it: an unparsed constant compares as empty and would pass silently", path)
	}
	quoted := regexp.MustCompile(`"([a-z_]+)"`).FindAllSubmatch(block[1], -1)
	if len(quoted) == 0 {
		t.Fatalf("PAYLOAD_CONSUMING parsed to an empty set from %s", path)
	}

	client := map[string]bool{}
	for _, m := range quoted {
		client[string(m[1])] = true
	}

	// `answered` is deliberately in the client's set and NOT in the daemon's:
	// applyAnswered redirects it before ApplyAction's generic merge is ever
	// reached (see workflow_action.go's own note), so the client attaching a
	// payload to it is safe even though this map does not list it. Every OTHER
	// difference is drift.
	const clientOnlyByDesign = "answered"
	delete(client, clientOnlyByDesign)

	var missingFromClient, extraInClient []string
	for verb := range SideEffectConsumesPayload {
		if !client[verb] {
			missingFromClient = append(missingFromClient, verb)
		}
	}
	for verb := range client {
		if !SideEffectConsumesPayload[verb] {
			extraInClient = append(extraInClient, verb)
		}
	}
	sort.Strings(missingFromClient)
	sort.Strings(extraInClient)

	if len(missingFromClient) > 0 {
		t.Errorf("the metaproject client does not know these consume their payload: %s\n"+
			"It will refuse to attach a payload to them, so the verb that needs one fails locally and never reaches boid — silently, exactly as `drop-child` did after #982. Add them to PAYLOAD_CONSUMING in %s",
			strings.Join(missingFromClient, ", "), path)
	}
	if len(extraInClient) > 0 {
		t.Errorf("the metaproject client believes these consume their payload, but the daemon merges them into task.Payload: %s\n"+
			"Remove them from PAYLOAD_CONSUMING in %s, or add them here if the daemon really should consume them",
			strings.Join(extraInClient, ", "), path)
	}
}
