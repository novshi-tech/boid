package runner

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// envAllowlist is the set of environment variable keys whose values are dumped
// verbatim into runner-state.json. Every other key is recorded by name with a
// "<redacted>" value so tokens / API keys / secrets never hit the diagnostic
// file. LC_* is handled by prefix, not by this exact-match set.
//
// Final list per docs/plans/agent-aware-boid.md §5 (runner-state-dump-design).
var envAllowlist = map[string]struct{}{
	"HOME":                  {},
	"PATH":                  {},
	"USER":                  {},
	"SHELL":                 {},
	"LANG":                  {},
	"TERM":                  {},
	"BOID_JOB_ID":           {},
	"BOID_RUNTIME_ID":       {},
	"BOID_AGENT_SESSION_ID": {},
	"BOID_BROKER_SOCKET":    {},
	"CLAUDE_CONFIG_DIR":     {},
}

// redactEnv returns a copy of env where only allow-listed keys keep their
// value; every other key maps to "<redacted>". The scan is allowlist-only (no
// denylist), so a new secret-bearing env var is redacted by default.
func redactEnv(env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		if envKeyAllowed(k) {
			out[k] = v
		} else {
			out[k] = "<redacted>"
		}
	}
	return out
}

func envKeyAllowed(key string) bool {
	if _, ok := envAllowlist[key]; ok {
		return true
	}
	// LC_* (locale) is safe and variable in count, so match by prefix.
	return strings.HasPrefix(key, "LC_")
}

// specDump is the redacted, JSON-friendly view of a sandbox.Spec written to the
// first line of runner-state.json.
type specDump struct {
	ID           string            `json:"id"`
	Argv         []string          `json:"argv"`
	WorkDir      string            `json:"workdir"`
	RootDir      string            `json:"root_dir"`
	ProxyPort    int               `json:"proxy_port"`
	TTY          bool              `json:"tty"`
	Foreground   bool              `json:"foreground"`
	StopSignal   string            `json:"stop_signal"`
	Cloneflags   []string          `json:"cloneflags"`
	PivotRoot    string            `json:"pivot_root"`
	PastaCmdline []string          `json:"pasta_cmdline"`
	Mounts       []mountDump       `json:"mounts"`
	NFTRules     [][]string        `json:"nft_rules"`
	Env          map[string]string `json:"env"`
}

type mountDump struct {
	Source   string `json:"source,omitempty"`
	Target   string `json:"target"`
	Type     string `json:"type"`
	ReadOnly bool   `json:"ro,omitempty"`
	Slave    bool   `json:"slave,omitempty"`
	Guard    string `json:"guard,omitempty"`
}

// buildSpecDump composes the redacted spec view. pastaCmdline is the resolved
// pasta argv (so the diagnostic shows exactly how the sandbox was launched).
func buildSpecDump(spec sandbox.Spec, pastaCmdline []string) specDump {
	plan := sandbox.BuildPlan(spec)
	mounts := make([]mountDump, 0, len(plan.Mounts))
	for _, m := range plan.Mounts {
		mounts = append(mounts, mountDump{
			Source:   m.Source,
			Target:   m.Target,
			Type:     string(m.Type),
			ReadOnly: m.ReadOnly,
			Slave:    m.Slave,
			Guard:    m.Guard,
		})
	}
	nft := make([][]string, 0, len(plan.NFTRules))
	for _, r := range plan.NFTRules {
		nft = append(nft, r.Args)
	}
	return specDump{
		ID:           spec.ID,
		Argv:         spec.Argv,
		WorkDir:      spec.WorkDir,
		RootDir:      spec.RootDir,
		ProxyPort:    spec.ProxyPort,
		TTY:          spec.TTY,
		Foreground:   spec.Foreground,
		StopSignal:   stopSignalName(spec),
		Cloneflags:   []string{"CLONE_NEWUSER", "CLONE_NEWNS"},
		PivotRoot:    spec.RootDir,
		PastaCmdline: pastaCmdline,
		Mounts:       mounts,
		NFTRules:     nft,
		Env:          redactEnv(spec.Env),
	}
}

// stopSignalName returns the configured stop signal name, defaulting to USR1.
func stopSignalName(spec sandbox.Spec) string {
	if spec.StopSignalName != "" {
		return spec.StopSignalName
	}
	return "USR1"
}

// stateLine is one NDJSON record in runner-state.json. Either Spec (the first,
// launch-time line) or a phase entry (Phase/Status/Detail) is populated.
type stateLine struct {
	Time   string    `json:"time"`
	Stage  string    `json:"stage"`
	Spec   *specDump `json:"spec,omitempty"`
	Phase  string    `json:"phase,omitempty"`
	Status string    `json:"status,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// State appends NDJSON diagnostic records to /tmp/boid-<id>-runner-state.json.
// Each write is a single appended line followed by Sync(), so a panic / kill -9
// still leaves the last reached phase flushed on disk. Every method is nil-safe.
type State struct {
	f *os.File
}

// OpenState opens (creating) the state file for append. A nil *State (returned
// when path is empty or open fails) makes all subsequent methods no-ops so the
// diagnostic path never blocks the run.
func OpenState(path string) *State {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	return &State{f: f}
}

func (s *State) writeLine(line stateLine) {
	if s == nil || s.f == nil {
		return
	}
	if line.Time == "" {
		line.Time = time.Now().UTC().Format(time.RFC3339Nano)
	}
	data, err := json.Marshal(line)
	if err != nil {
		return
	}
	data = append(data, '\n')
	_, _ = s.f.Write(data)
	_ = s.f.Sync()
}

// Spec records the launch-time spec dump (first line of the file).
func (s *State) Spec(stage string, spec sandbox.Spec, pastaCmdline []string) {
	dump := buildSpecDump(spec, pastaCmdline)
	s.writeLine(stateLine{Stage: stage, Spec: &dump})
}

// Phase records a single phase transition with ok/error status.
func (s *State) Phase(stage, phase, status, detail string) {
	s.writeLine(stateLine{Stage: stage, Phase: phase, Status: status, Detail: detail})
}

// OK records a successful phase.
func (s *State) OK(stage, phase string) { s.Phase(stage, phase, "ok", "") }

// Fail records a failed phase with an error detail.
func (s *State) Fail(stage, phase string, err error) {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	s.Phase(stage, phase, "error", detail)
}

// Close closes the underlying file.
func (s *State) Close() {
	if s == nil || s.f == nil {
		return
	}
	_ = s.f.Close()
}

// sortedEnvKeys is a small helper kept for deterministic env iteration in tests.
func sortedEnvKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
