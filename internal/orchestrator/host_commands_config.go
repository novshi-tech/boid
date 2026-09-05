package orchestrator

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"

	"github.com/novshi-tech/boid/internal/config"
	"gopkg.in/yaml.v3"
)

// DefaultHostCommandsPath returns the default path for the aggregated
// host_commands.yaml config: $XDG_CONFIG_HOME/boid/host_commands.yaml, or
// ~/.config/boid/host_commands.yaml when XDG_CONFIG_HOME is unset (matching
// the behaviour of os.UserConfigDir on Linux). Mirrors DefaultWorkspaceDir's
// resolution strategy (workspace_store.go).
func DefaultHostCommandsPath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("host_commands config: could not determine user config directory: %w", err)
	}
	return filepath.Join(configDir, "boid", "host_commands.yaml"), nil
}

// readKitHostCommandsRaw reads only the `host_commands:` section of a
// kit.yaml file, unexpanded — unlike ReadKitMeta, it does not run
// interpolateHostCommands or any other kit.yaml validation step. Expanding
// ${VAR} placeholders here would bake resolved secret values into the
// aggregated on-disk config and let two kits with differently-named
// placeholders resolving to the same value evade collision detection.
func readKitHostCommandsRaw(kitDir string) (HostCommands, error) {
	yamlPath := filepath.Join(kitDir, "kit.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return nil, fmt.Errorf("read kit.yaml: %w", err)
	}
	var wrapper struct {
		HostCommands HostCommands `yaml:"host_commands"`
	}
	if err := yaml.Unmarshal(data, &wrapper); err != nil {
		return nil, fmt.Errorf("parse kit.yaml: %w", err)
	}
	return wrapper.HostCommands, nil
}

// normalizeHostCommandSpec replaces any zero-length slice/map field with nil,
// so two otherwise-identical specs compare equal under reflect.DeepEqual
// regardless of whether a field was omitted entirely (nil) or spelled out
// empty in YAML (e.g. `env: {}` vs no `env:` key at all). Without this, kit A
// and kit B declaring the same command with the same allow-list would be
// flagged as a definition conflict merely because one kit's YAML happened to
// spell out an empty `env:`/`reject:` and the other's didn't.
func normalizeHostCommandSpec(spec HostCommandSpec) HostCommandSpec {
	if len(spec.Allow) == 0 {
		spec.Allow = nil
	}
	if len(spec.Deny) == 0 {
		spec.Deny = nil
	}
	if len(spec.Env) == 0 {
		spec.Env = nil
	}
	if len(spec.Reject) == 0 {
		spec.Reject = nil
	}
	return spec
}

// LoadHostCommandsFromKits scans kitsDir for installed kits — each a
// subdirectory containing a kit.yaml — and aggregates their host_commands
// sections into a single map keyed by command name. Definitions are read raw
// (readKitHostCommandsRaw): no env-var interpolation or other kit.yaml
// validation runs on this path.
//
// A missing kitsDir (no kits installed yet, or KitsDir unconfigured) is not
// an error: it returns an empty map.
//
// Two kits may declare the same command name only when the definitions are
// identical after normalizeHostCommandSpec and reflect.DeepEqual — that case
// is silently deduped into one entry. If two kits declare the same name with
// different definitions, this returns an error naming both kit directories
// and the conflicting command; callers should treat this as fatal.
func LoadHostCommandsFromKits(kitsDir string) (map[string]HostCommandSpec, error) {
	entries, err := os.ReadDir(kitsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]HostCommandSpec{}, nil
		}
		return nil, fmt.Errorf("host_commands: list kits dir %q: %w", kitsDir, err)
	}

	// Sort subdirectory names so conflict error messages are deterministic
	// regardless of os.ReadDir's (already-sorted, but let's not rely on that
	// implementation detail) or the filesystem's iteration order.
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	aggregated := make(map[string]HostCommandSpec)
	definedBy := make(map[string]string) // command name -> kit dir that first defined it

	for _, name := range names {
		kitDir := filepath.Join(kitsDir, name)
		if _, err := os.Stat(filepath.Join(kitDir, "kit.yaml")); err != nil {
			if os.IsNotExist(err) {
				continue // not a kit directory (no kit.yaml)
			}
			return nil, fmt.Errorf("host_commands: stat %q: %w", filepath.Join(kitDir, "kit.yaml"), err)
		}

		hostCommands, err := readKitHostCommandsRaw(kitDir)
		if err != nil {
			return nil, fmt.Errorf("host_commands: read kit %q: %w", kitDir, err)
		}

		// Sorted so the *reported* collision is deterministic across runs
		// even when a single kit collides on multiple commands (Go map
		// iteration order is randomised).
		cmdNames := make([]string, 0, len(hostCommands))
		for cmdName := range hostCommands {
			cmdNames = append(cmdNames, cmdName)
		}
		sort.Strings(cmdNames)

		for _, cmdName := range cmdNames {
			spec := normalizeHostCommandSpec(hostCommands[cmdName])
			existing, ok := aggregated[cmdName]
			if !ok {
				aggregated[cmdName] = spec
				definedBy[cmdName] = kitDir
				continue
			}
			if reflect.DeepEqual(existing, spec) {
				continue // dedupe: identical definition across kits, ok
			}
			return nil, fmt.Errorf(
				"host_commands: command %q is defined differently by kit %q and kit %q; align the definitions or rename one",
				cmdName, definedBy[cmdName], kitDir,
			)
		}
	}

	return aggregated, nil
}

// hostCommandSpecStrict mirrors HostCommandSpec (spec_types.go, field for
// field) for use only by the aggregated host_commands.yaml config's decode
// path (LoadHostCommandsConfig) and its symmetric encode path
// (WriteHostCommandsConfig).
//
// A separate type is needed because dec.KnownFields(true) on the top-level
// Decoder does not propagate into HostCommands.UnmarshalYAML — that method's
// internal value.Decode(&m) always starts a fresh decode with
// KnownFields=false regardless of the outer Decoder, silently dropping
// nested typos. Decoding into a plain map[string]hostCommandSpecStrict never
// enters that custom UnmarshalYAML, so the whole tree stays strict.
//
// IMPORTANT: keep this in sync with HostCommandSpec — if a field is added to
// or removed from HostCommandSpec, mirror the change here and in
// toHostCommandSpec / newHostCommandSpecStrict below.
type hostCommandSpecStrict struct {
	Allow  []string          `yaml:"allow,omitempty"`
	Deny   []string          `yaml:"deny,omitempty"`
	Stdin  bool              `yaml:"stdin,omitempty"` // deprecated, see HostCommandSpec.Stdin
	Path   string            `yaml:"path,omitempty"`
	Env    map[string]string `yaml:"env,omitempty"`
	Reject []RejectRule      `yaml:"reject,omitempty"`
}

// toHostCommandSpec converts the strict decode-only shape to the public
// HostCommandSpec type used everywhere else in the package. The two structs
// share the same field layout (see hostCommandSpecStrict's doc comment for
// why they are kept in sync), so a plain Go type conversion is sufficient —
// yaml/json struct tags are not part of Go's convertibility check.
func (s hostCommandSpecStrict) toHostCommandSpec() HostCommandSpec {
	return HostCommandSpec(s)
}

// newHostCommandSpecStrict converts a HostCommandSpec to the strict
// encode/decode shape, symmetric with toHostCommandSpec.
func newHostCommandSpecStrict(spec HostCommandSpec) hostCommandSpecStrict {
	return hostCommandSpecStrict(spec)
}

// hostCommandsFileStrict is the on-disk shape of the aggregated
// host_commands.yaml config: a single top-level `host_commands:` map, same
// vocabulary as the kit.yaml `host_commands` section — but keyed to a plain
// map type (not HostCommands) so strict decoding reaches every nested field.
// See hostCommandSpecStrict's doc comment for why this matters.
type hostCommandsFileStrict struct {
	HostCommands map[string]hostCommandSpecStrict `yaml:"host_commands"`
}

// WriteHostCommandsConfig serializes spec as YAML (top-level `host_commands:`
// map) and atomically writes it to path — a sibling temp file is written,
// fsynced, and renamed into place (config.WriteFileAtomic), so a reader never
// observes a partially written file and a write failure never corrupts an
// existing config. The parent directory is created if it does not exist.
//
// The file is written 0600 (owner read/write only): definitions aggregated
// from kit.yaml are stored raw/unexpanded (see readKitHostCommandsRaw), but
// path/env values can still look sensitive, so this config is treated like
// any other credential-adjacent file on disk.
func WriteHostCommandsConfig(path string, spec map[string]HostCommandSpec) error {
	strictSpec := make(map[string]hostCommandSpecStrict, len(spec))
	for name, s := range spec {
		strictSpec[name] = newHostCommandSpecStrict(s)
	}
	data, err := yaml.Marshal(hostCommandsFileStrict{HostCommands: strictSpec})
	if err != nil {
		return fmt.Errorf("host_commands config: marshal: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("host_commands config: mkdir %q: %w", filepath.Dir(path), err)
	}
	if err := config.WriteFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("host_commands config: write %q: %w", path, err)
	}
	return nil
}

// LoadHostCommandsConfig reads and parses the aggregated host_commands.yaml
// config at path. A missing or empty file is not an error — it returns an
// empty map. Parsing is strict at every level (top-level keys and each
// command's fields, including nested reject rules — see
// hostCommandSpecStrict) so a typo in a hand-edited file surfaces as a load
// error rather than being silently ignored.
func LoadHostCommandsConfig(path string) (map[string]HostCommandSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]HostCommandSpec{}, nil
		}
		return nil, fmt.Errorf("host_commands config: read %q: %w", path, err)
	}
	if len(data) == 0 {
		return map[string]HostCommandSpec{}, nil
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var parsed hostCommandsFileStrict
	if err := dec.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("host_commands config: parse %q: %w", path, err)
	}
	if parsed.HostCommands == nil {
		return map[string]HostCommandSpec{}, nil
	}
	out := make(map[string]HostCommandSpec, len(parsed.HostCommands))
	for name, s := range parsed.HostCommands {
		out[name] = s.toHostCommandSpec()
	}
	return out, nil
}

// writeHostCommandsConfigIfMissing writes spec to path via
// WriteHostCommandsConfig only when no file exists there yet — never
// clobbers an existing aggregate or hand edit with a freshly regenerated one.
//
// Returns wrote=true when a write actually happened (the file was missing),
// false when an existing file was found and left untouched. A stat error
// other than "not exist" is returned as-is.
func writeHostCommandsConfigIfMissing(path string, spec map[string]HostCommandSpec) (wrote bool, err error) {
	if _, statErr := os.Stat(path); statErr == nil {
		return false, nil
	} else if !os.IsNotExist(statErr) {
		return false, fmt.Errorf("host_commands config: stat %q: %w", path, statErr)
	}
	if err := WriteHostCommandsConfig(path, spec); err != nil {
		return false, err
	}
	return true, nil
}

// CloneHostCommandsMap returns a deep copy of m: a new top-level map, plus
// independent copies of every HostCommandSpec's slice/map fields. Callers
// such as Server.HostCommands() use this to hand out a snapshot rather than
// the live internal map, so a caller mutating the returned value (or a
// slice/map nested inside one of its entries) can never perturb daemon
// state. A nil input returns nil.
func CloneHostCommandsMap(m map[string]HostCommandSpec) map[string]HostCommandSpec {
	if m == nil {
		return nil
	}
	out := make(map[string]HostCommandSpec, len(m))
	for name, spec := range m {
		out[name] = cloneHostCommandSpec(spec)
	}
	return out
}

// ExpandHostCommandsForDispatch returns a clone of raw with each entry's
// Path and Env values expanded via interpolateHostCommands against the
// daemon process's own environment. The aggregated host_commands.yaml
// config on disk stores raw/unexpanded definitions by design (see
// readKitHostCommandsRaw), but a raw `path: ${HOME}/bin/gh` is useless to
// exec.LookPath at dispatch time, so something has to expand it before
// ProjectStore ever hands a HostCommandSpec to the sandbox.
//
// Call this once at daemon startup on the freshly loaded raw config and wire
// the expanded result into ProjectStore.SetHostCommands, while
// Server.HostCommands() keeps handing out the raw CloneHostCommandsMap
// snapshot unchanged.
//
// raw is never mutated: the returned map is an independent deep copy
// (CloneHostCommandsMap) before interpolateHostCommands runs on that copy.
func ExpandHostCommandsForDispatch(raw map[string]HostCommandSpec) map[string]HostCommandSpec {
	cloned := CloneHostCommandsMap(raw)
	if cloned == nil {
		return cloned
	}
	interpolateHostCommands(HostCommands(cloned))
	return cloned
}

// cloneHostCommandSpec deep-copies a single HostCommandSpec's slice/map
// fields; nil fields stay nil.
func cloneHostCommandSpec(spec HostCommandSpec) HostCommandSpec {
	clone := spec
	if spec.Allow != nil {
		clone.Allow = append([]string(nil), spec.Allow...)
	}
	if spec.Deny != nil {
		clone.Deny = append([]string(nil), spec.Deny...)
	}
	if spec.Env != nil {
		env := make(map[string]string, len(spec.Env))
		for k, v := range spec.Env {
			env[k] = v
		}
		clone.Env = env
	}
	if spec.Reject != nil {
		clone.Reject = append([]RejectRule(nil), spec.Reject...)
	}
	return clone
}
