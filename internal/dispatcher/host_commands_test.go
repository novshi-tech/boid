package dispatcher

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestResolveHostCommands_RejectRulesPassthrough guards the resolve seam:
// ResolveHostCommands rewrites Name/Path on a struct copy, so every other
// field (including reject rules) must survive untouched. If the copy is ever
// replaced with field-by-field construction, this test catches dropped fields.
func TestResolveHostCommands_RejectRulesPassthrough(t *testing.T) {
	in := map[string]orchestrator.CommandDef{
		"gh": {
			AllowedSubcommands: []string{"pr"},
			RejectRules: []orchestrator.RejectRule{
				{Match: "*--body-file*", Reason: "sandbox paths are not visible on the host"},
			},
		},
	}
	lookPath := func(name string) (string, error) { return "/usr/bin/" + name, nil }
	getOriginURL := func(string) (string, error) { return "", fmt.Errorf("not used in this test") }

	out, _, err := ResolveHostCommands(nil, in, "/proj", lookPath, getOriginURL)
	if err != nil {
		t.Fatalf("ResolveHostCommands: %v", err)
	}
	def, ok := out["/usr/bin/gh"]
	if !ok {
		t.Fatalf("missing resolved gh entry: %+v", out)
	}
	if def.Name != "gh" || def.Path != "/usr/bin/gh" {
		t.Fatalf("unexpected resolved identity: %+v", def)
	}
	if len(def.RejectRules) != 1 || def.RejectRules[0] != in["gh"].RejectRules[0] {
		t.Fatalf("reject rules dropped across resolve: %+v", def.RejectRules)
	}
}

func alwaysLookPath(name string) (string, error) { return "/usr/bin/" + name, nil }

// TestResolveHostCommands_RepoSlugExpansionHTTPS covers the https origin URL
// form.
func TestResolveHostCommands_RepoSlugExpansionHTTPS(t *testing.T) {
	in := map[string]orchestrator.CommandDef{
		"gh": {Env: map[string]string{"GH_REPO": "${boid:repo_slug}"}},
	}
	getOriginURL := func(dir string) (string, error) {
		if dir != "/proj" {
			t.Fatalf("getOriginURL called with dir=%q, want /proj", dir)
		}
		return "https://github.com/owner/repo.git", nil
	}

	out, _, err := ResolveHostCommands(nil, in, "/proj", alwaysLookPath, getOriginURL)
	if err != nil {
		t.Fatalf("ResolveHostCommands: %v", err)
	}
	def := out["/usr/bin/gh"]
	if got, want := def.Env["GH_REPO"], "github.com/owner/repo"; got != want {
		t.Errorf("GH_REPO = %q, want %q", got, want)
	}
}

// TestResolveHostCommands_RepoSlugExpansionSSH covers both the scp-like
// (git@host:owner/repo.git) and ssh:// origin URL forms.
func TestResolveHostCommands_RepoSlugExpansionSSH(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"scp-like", "git@github.com:owner/repo.git"},
		{"ssh-scheme", "ssh://git@github.com/owner/repo.git"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := map[string]orchestrator.CommandDef{
				"gh": {Env: map[string]string{"GH_REPO": "${boid:repo_slug}"}},
			}
			getOriginURL := func(string) (string, error) { return tc.url, nil }

			out, _, err := ResolveHostCommands(nil, in, "/proj", alwaysLookPath, getOriginURL)
			if err != nil {
				t.Fatalf("ResolveHostCommands: %v", err)
			}
			def := out["/usr/bin/gh"]
			if got, want := def.Env["GH_REPO"], "github.com/owner/repo"; got != want {
				t.Errorf("GH_REPO = %q, want %q", got, want)
			}
		})
	}
}

// TestResolveHostCommands_NoPlaceholderNeverInvokesGetOriginURL guards the
// "don't shell out to git for nothing" perf contract: commands with no
// ${boid:...} usage in Env must never trigger the origin URL lookup.
func TestResolveHostCommands_NoPlaceholderNeverInvokesGetOriginURL(t *testing.T) {
	in := map[string]orchestrator.CommandDef{
		"gh": {Env: map[string]string{"GH_TOKEN": "static-value"}},
	}
	called := false
	getOriginURL := func(string) (string, error) {
		called = true
		return "", fmt.Errorf("should not be called")
	}

	out, _, err := ResolveHostCommands(nil, in, "/proj", alwaysLookPath, getOriginURL)
	if err != nil {
		t.Fatalf("ResolveHostCommands: %v", err)
	}
	if called {
		t.Error("getOriginURL was invoked despite no ${boid:...} placeholder in Env")
	}
	if got := out["/usr/bin/gh"].Env["GH_TOKEN"]; got != "static-value" {
		t.Errorf("GH_TOKEN = %q, want unchanged", got)
	}
}

// TestResolveHostCommands_MissingOriginExpandsToEmptyStringNoError ensures a
// missing/unresolvable origin degrades gracefully: the placeholder expands to
// "" and the dispatch is never failed by this.
func TestResolveHostCommands_MissingOriginExpandsToEmptyStringNoError(t *testing.T) {
	in := map[string]orchestrator.CommandDef{
		"gh": {Env: map[string]string{"GH_REPO": "${boid:repo_slug}"}},
	}
	getOriginURL := func(string) (string, error) {
		return "", fmt.Errorf("no such remote 'origin'")
	}

	out, _, err := ResolveHostCommands(nil, in, "/proj", alwaysLookPath, getOriginURL)
	if err != nil {
		t.Fatalf("ResolveHostCommands: %v", err)
	}
	if got := out["/usr/bin/gh"].Env["GH_REPO"]; got != "" {
		t.Errorf("GH_REPO = %q, want empty string on missing origin", got)
	}
}

// TestResolveHostCommands_UnknownBoidVarLeftUntouched is the forward-compat
// check: an unrecognized ${boid:...} variable must survive resolve verbatim
// (not fail, not get blanked), only logged.
func TestResolveHostCommands_UnknownBoidVarLeftUntouched(t *testing.T) {
	in := map[string]orchestrator.CommandDef{
		"gh": {Env: map[string]string{"SOMETHING": "${boid:future_var}"}},
	}
	called := false
	getOriginURL := func(string) (string, error) {
		called = true
		return "", fmt.Errorf("should not be called for an unrelated placeholder")
	}

	out, _, err := ResolveHostCommands(nil, in, "/proj", alwaysLookPath, getOriginURL)
	if err != nil {
		t.Fatalf("ResolveHostCommands: %v", err)
	}
	if called {
		t.Error("getOriginURL was invoked for a non-repo_slug placeholder")
	}
	if got, want := out["/usr/bin/gh"].Env["SOMETHING"], "${boid:future_var}"; got != want {
		t.Errorf("SOMETHING = %q, want untouched %q", got, want)
	}
}

// TestResolveHostCommands_CallerEnvMapNotMutated guards against aliasing:
// the input CommandDef.Env map must not be mutated in place, since the
// caller (orchestrator spec parsing) may reuse or compare it afterwards.
func TestResolveHostCommands_CallerEnvMapNotMutated(t *testing.T) {
	callerEnv := map[string]string{"GH_REPO": "${boid:repo_slug}"}
	in := map[string]orchestrator.CommandDef{
		"gh": {Env: callerEnv},
	}
	getOriginURL := func(string) (string, error) {
		return "https://github.com/owner/repo.git", nil
	}

	_, _, err := ResolveHostCommands(nil, in, "/proj", alwaysLookPath, getOriginURL)
	if err != nil {
		t.Fatalf("ResolveHostCommands: %v", err)
	}
	if got, want := callerEnv["GH_REPO"], "${boid:repo_slug}"; got != want {
		t.Errorf("caller's Env map was mutated: GH_REPO = %q, want unchanged %q", got, want)
	}
}

// TestResolveHostCommands_ByNameKeyedByDeclaredName covers the new (5a-1)
// byName return value: every resolved entry must also be reachable by its
// short, user-declared name — the "policy 用" key that
// docs/plans/phase5-shim-and-task-context.md decision 2 wants broker
// registration and BOID_HOST_COMMAND_RULES to use instead of the absolute
// bind-mount path.
func TestResolveHostCommands_ByNameKeyedByDeclaredName(t *testing.T) {
	in := map[string]orchestrator.CommandDef{
		"gh": {AllowedSubcommands: []string{"pr"}},
	}
	byPath, byName, err := ResolveHostCommands([]string{"jq"}, in, "/proj", alwaysLookPath, fakeGetOriginURLForHostCommandsTest)
	if err != nil {
		t.Fatalf("ResolveHostCommands: %v", err)
	}

	ghByName, ok := byName["gh"]
	if !ok {
		t.Fatalf("byName missing %q entry: %+v", "gh", byName)
	}
	if !reflect.DeepEqual(ghByName, byPath["/usr/bin/gh"]) {
		t.Errorf("byName[%q] = %+v, want identical to byPath[%q] = %+v", "gh", ghByName, "/usr/bin/gh", byPath["/usr/bin/gh"])
	}

	jqByName, ok := byName["jq"]
	if !ok {
		t.Fatalf("byName missing builtin entry %q: %+v", "jq", byName)
	}
	if !reflect.DeepEqual(jqByName, byPath["/usr/bin/jq"]) {
		t.Errorf("byName[%q] = %+v, want identical to byPath[%q] = %+v", "jq", jqByName, "/usr/bin/jq", byPath["/usr/bin/jq"])
	}

	if len(byName) != len(byPath) {
		t.Errorf("byName has %d entries, byPath has %d; both views must cover the same resolved set", len(byName), len(byPath))
	}
}

// TestResolveHostCommands_ReservedNamesExcludedFromByName is the short-name
// counterpart of the reserved-name guard validateBuiltinHostConflict already
// enforces at config-load time (internal/orchestrator/spec_loader.go): even
// if a "boid"/"git"/"fetch" entry somehow reaches ResolveHostCommands (e.g. a
// future caller that skips validateBuiltinHostConflict), neither returned
// view may expose it — a short-name-keyed policy table entry for one of
// these names would be just as dangerous as a byPath one, since it is what
// the broker now checks first.
func TestResolveHostCommands_ReservedNamesExcludedFromByName(t *testing.T) {
	in := map[string]orchestrator.CommandDef{
		"boid":  {},
		"git":   {},
		"fetch": {},
		"gh":    {},
	}
	byPath, byName, err := ResolveHostCommands([]string{"boid", "git", "fetch"}, in, "/proj", alwaysLookPath, fakeGetOriginURLForHostCommandsTest)
	if err != nil {
		t.Fatalf("ResolveHostCommands: %v", err)
	}

	for _, reserved := range []string{"boid", "git", "fetch"} {
		if _, ok := byName[reserved]; ok {
			t.Errorf("byName must not contain reserved name %q, got %+v", reserved, byName[reserved])
		}
	}
	if _, ok := byName["gh"]; !ok {
		t.Error("byName must still contain the non-reserved gh entry")
	}
	if len(byPath) != len(byName) {
		t.Errorf("byPath has %d entries, byName has %d; reserved-name exclusion must match in both views", len(byPath), len(byName))
	}
}

func fakeGetOriginURLForHostCommandsTest(string) (string, error) {
	return "", fmt.Errorf("getOriginURL should not be called")
}

// TestRepoSlugFromOriginURL_NonGithubHostKeptAsIs covers the "non-github
// hosts are kept as-is" normalization rule.
func TestRepoSlugFromOriginURL_NonGithubHostKeptAsIs(t *testing.T) {
	got, err := repoSlugFromOriginURL("https://gitlab.example.com/group/proj.git")
	if err != nil {
		t.Fatalf("repoSlugFromOriginURL: %v", err)
	}
	if want := "gitlab.example.com/group/proj"; got != want {
		t.Errorf("slug = %q, want %q", got, want)
	}
}
