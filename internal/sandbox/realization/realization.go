// Package realization translates a backend-neutral sandbox.Spec into the
// intermediate representation a container backend needs to build a `docker
// create` call: volumes/binds, tmpfs mounts, environment, working directory
// and argv (docs/plans/phase6-container-backend.md §PR3).
//
// sandbox.Spec is role-neutral but not backend-neutral (see the plan doc's
// 現状棚卸し section: "`sandbox.Spec` は role 非依存だが backend 非依存でない"):
// Mount.Source is always a host path in the current (userns) callers,
// Mount.Guard/DetectType are shell-idiom escape hatches the userns runner's
// generated shell script understands, and `/workspace/<name>` is bound to a
// host runtime directory (`internal/dispatcher/sandbox_builder.go`'s
// cloneMounts). Realize narrows the translation to what a container backend
// actually needs and classifies every Mount's Source into one of three kinds
// (MountSource) so a docker-out-of-docker (DooD) sibling `docker create` call
// can tell a host bind apart from a container-local directory apart from a
// named volume (決定 4's DooD path 境界).
//
// This package is inert scaffolding: Realize is a pure function (no docker
// API calls, no filesystem access, no process execution) and nothing in the
// daemon calls it yet — that wiring lands in PR5 together with the
// containerBackend implementation. It does not import docker/docker/client
// or any docker SDK type. It also does not touch, depend on, or change the
// behaviour of the existing userns realization path (sandbox.BuildPlan /
// internal/sandbox/runner / internal/sandbox/dockerproxy / the sandbox
// package's proxy.go+proxy_manager.go) — see this package's unit tests for
// the pinned translation contract, not that path.
package realization

import (
	"fmt"
	"strings"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// sandboxWorkspaceRoot mirrors internal/dispatcher's unexported
// sandboxCloneTargetDir constant ("/workspace", sandbox_builder.go). It is
// duplicated here rather than imported: internal/dispatcher already imports
// internal/sandbox (and will end up importing this package once PR5 wires
// containerBackend in), so importing internal/dispatcher from here would be
// a cycle. The string itself is a fixed, well-known sandbox-internal
// contract path (docs/plans/git-gateway-cutover.md), not a runtime value, so
// duplicating it as a constant is safe.
const sandboxWorkspaceRoot = "/workspace"

// isWorkspaceLocalTarget reports whether target is the sandbox-internal
// clone parent dir or a path under it — i.e. the mount the caller-supplied
// `/workspace`/`/workspace/<name>` bind (cloneMounts' host-runtime-dir bind
// in the userns backend) targets. Realize classifies every mount with such a
// target as MountSourceContainerLocal regardless of its Source, per 決定 4 /
// the PR3 plan text ("`/workspace/<name>` は container-local に着地 (host
// bind にしない)"): the container backend does not bind and does not need to
// share this directory with the host at all — it's the scratch dir the
// in-container clone lands in.
func isWorkspaceLocalTarget(target string) bool {
	return target == sandboxWorkspaceRoot || strings.HasPrefix(target, sandboxWorkspaceRoot+"/")
}

// MountSourceKind classifies where a translated mount's content actually
// comes from — the three shapes a container backend's bind source can take
// (決定 4, DooD path 境界).
type MountSourceKind int

const (
	// MountSourceNamedVolume is a docker-managed named volume, e.g.
	// "boid-home-workspace-foo". Nothing in the current (Phase 6) dispatcher
	// produces this yet — workspace HOME stays a host bind through Phase 6
	// (決定 4, confirmed 2026-07-22: the #813 draft's "named volume 化" call
	// was a correction, reverted). Named-volume HOME is a Phase 7 follow-up
	// (docs/plans/phase6-container-backend.md スコープ節「含まない」). The
	// kind exists now so Realize's classification is complete and so PR5 /
	// Phase 7 can opt a mount into it without changing this package.
	MountSourceNamedVolume MountSourceKind = iota

	// MountSourceHostPath is a host absolute path bind. Per the DooD path
	// 境界 (決定 4, codex Blocker 2): the sibling `docker create` bind
	// source is resolved by the *host* docker daemon, not the daemon
	// container's own mount namespace, so this must be a path that resolves
	// identically inside the daemon container and on the host (the daemon
	// container's persistent volumes — workspace HOME, clone reference
	// `.git` dirs — are deliberately laid out at matching absolute paths for
	// exactly this reason; see 決定 4's "起動時に検証" note, enforced
	// elsewhere, not by this package).
	MountSourceHostPath

	// MountSourceContainerLocal has no host-side counterpart at all: either
	// it is created fresh inside the container (the `/workspace/<name>`
	// clone target — 決定 4/10) or the caller supplied a mount with no
	// Source to back it.
	MountSourceContainerLocal
)

// String renders k for diagnostics and test failure messages.
func (k MountSourceKind) String() string {
	switch k {
	case MountSourceNamedVolume:
		return "named-volume"
	case MountSourceHostPath:
		return "host-path"
	case MountSourceContainerLocal:
		return "container-local"
	default:
		return fmt.Sprintf("MountSourceKind(%d)", int(k))
	}
}

// MountSource is the classified form of a sandbox.Mount's Source field: Kind
// says which of the three shapes it is, Value carries the volume name / host
// absolute path / container-local path accordingly.
type MountSource struct {
	Kind  MountSourceKind
	Value string
}

// VolumeMount is a single bind-style mount translated for a container
// backend's `docker create` call (docker Binds / Mounts entry).
type VolumeMount struct {
	Source MountSource
	Target string // absolute path inside the container

	ReadOnly bool
	// IsFile carries sandbox.Mount.IsFile through unchanged — docker infers
	// file-vs-directory from the host path itself for a bind mount, so this
	// is informational for PR5 (e.g. to decide whether a container-local
	// target needs `touch` instead of `mkdir -p` before use), not consumed
	// by Realize.
	IsFile bool

	// Guard and DetectType are carried through from sandbox.Mount unchanged
	// but are not evaluated by Realize (決定 the PR3 plan text asks to
	// "固定": Guard/DetectType の扱いを固定). Both are userns-runner shell
	// idioms — Guard renders a `[ <expr> ]`-gated shell block, DetectType
	// picks bind-vs-rbind and file-vs-dir at runtime via test(1) — that make
	// sense for a generated mount shell script but not for a `docker
	// create` Binds/Mounts list, which either mounts a path or doesn't.
	// Fixed contract for PR5: a mount with a non-empty Guard needs the guard
	// condition evaluated by the container backend itself, on the host side,
	// before deciding whether to include the entry in the docker create call
	// at all (docker does not skip missing bind sources the way the userns
	// runner's generated `if`-guard does) — Realize does not perform host
	// filesystem checks, so it leaves Guard on the struct rather than
	// silently dropping or silently applying it. DetectType similarly stays
	// informational: docker bind mounts do not distinguish file vs directory
	// the way the userns rbind+remount idiom needs to.
	Guard      string
	DetectType bool
}

// TmpfsMount is a tmpfs mount translated for a container backend (docker
// --tmpfs / Mounts entry with Type: "tmpfs"). sandbox.Mount never sets
// Source for a tmpfs entry (see sandbox.Mount's doc comment: "host path
// (empty for tmpfs)"), so there is no MountSource to classify here.
//
// Guard is carried through from sandbox.Mount unchanged (same fixed
// contract as VolumeMount.Guard — see below): userns evaluates it before
// every mount, tmpfs or bind alike, so a tmpfs entry whose Guard is
// non-empty is skipped when the guard expression is false. Losing Guard
// here would let the container backend materialize a tmpfs mount that
// userns would have suppressed, silently diverging behavior.
type TmpfsMount struct {
	Target   string
	ReadOnly bool
	Guard    string
}

// Realization is the container-backend-neutral intermediate representation
// Realize produces from a sandbox.Spec. It is deliberately not a docker SDK
// type (no docker/docker/client import in this package — that wiring is
// PR5's job): a future containerBackend converts a Realization into the
// concrete docker create/start call.
type Realization struct {
	// ID mirrors sandbox.Spec.ID (the job id), carried through unchanged so
	// callers can label the eventual container/volume/network resources
	// (`boid.job_id`, 決定 6/9) without re-threading the Spec.
	ID string

	Volumes []VolumeMount
	Tmpfs   []TmpfsMount

	// Env is spec.Env carried through verbatim — including secrets such as
	// BOID_BROKER_TOKEN. This intentionally does NOT apply the
	// allowlist-redact pattern internal/sandbox/runner/state.go's redactEnv
	// uses for the diagnostic runner-state.json dump: that redaction exists
	// only for a diagnostic artifact, never for the value actually handed to
	// the running process. The userns backend hands spec.Env to the runner
	// unredacted for the same reason (it needs the real broker token to
	// register); a container backend needs the same real values on `docker
	// create`'s Env. A future container-backend diagnostic dump (PR7 territory,
	// 決定 8) should apply the same allowlist-redact convention as
	// state.go's redactEnv — but that is a separate, not-yet-existing
	// artifact, not this field.
	Env map[string]string

	// Workdir is spec.WorkDir carried through unchanged (docker create's
	// WorkingDir).
	Workdir string

	// Argv is spec.Argv carried through unchanged (docker create's Cmd).
	Argv []string

	// TTY mirrors spec.TTY (docker create's Tty).
	TTY bool
}

// Realize translates spec into a Realization. It is a pure function: no
// docker API calls, no filesystem access, no side effects. Returns an error
// if spec contains a Mount with an empty Target — every other field on Spec
// this package reads is copied through as-is.
//
// Deliberately out of scope (left for the container entrypoint / PR5 to
// consume from Spec directly, unchanged, or for a later PR — see the plan
// doc's 決定 section for where each lives):
//   - spec.Files / spec.Symlinks: already backend-neutral (sandbox-internal
//     path + content/target, no host dependency) — 決定 2 has the container
//     entrypoint apply these directly from Spec, no translation needed.
//   - spec.ProxyPort: userns-specific (nft + pasta). Container egress is an
//     L3 network-topology concern (internal network + egress proxy, 決定 5),
//     not a per-mount/env translation.
//   - spec.RootDir / spec.CleanupPaths / spec.Profile: userns runner
//     bookkeeping with no container-backend equivalent (rootfs + cleanup are
//     the container runtime's job once the container is removed).
//   - spec.Clone: consumed directly by the container entrypoint's in-container
//     clone step (決定 2/10), not part of the docker create call this
//     package targets.
func Realize(spec sandbox.Spec) (Realization, error) {
	r := Realization{
		ID:      spec.ID,
		Env:     copyEnv(spec.Env),
		Workdir: spec.WorkDir,
		Argv:    append([]string(nil), spec.Argv...),
		TTY:     spec.TTY,
	}

	for _, m := range spec.Mounts {
		if m.Target == "" {
			return Realization{}, fmt.Errorf("realization: mount has empty Target (Source=%q, Type=%q)", m.Source, m.Type)
		}

		if m.Type == sandbox.MountTmpfs {
			r.Tmpfs = append(r.Tmpfs, TmpfsMount{
				Target:   m.Target,
				ReadOnly: m.ReadOnly,
				Guard:    m.Guard,
			})
			continue
		}

		r.Volumes = append(r.Volumes, VolumeMount{
			Source:     classifySource(m),
			Target:     m.Target,
			ReadOnly:   m.ReadOnly,
			IsFile:     m.IsFile,
			Guard:      m.Guard,
			DetectType: m.DetectType,
		})
	}

	return r, nil
}

// classifySource implements the 3-way Mount.Source classification (決定 4):
// container-local when the mount targets the sandbox-internal
// `/workspace`/`/workspace/<name>` clone dir or has no Source at all; a host
// absolute path bind when Source starts with "/" (every host-path Source in
// the current codebase is already absolute — cloneMounts/homeMounts always
// build these from resolved absolute directories); a named volume name
// otherwise (Phase 7 forward-compat, see MountSourceNamedVolume's doc
// comment — nothing produces this today).
func classifySource(m sandbox.Mount) MountSource {
	if isWorkspaceLocalTarget(m.Target) {
		return MountSource{Kind: MountSourceContainerLocal, Value: m.Target}
	}
	if m.Source == "" {
		return MountSource{Kind: MountSourceContainerLocal, Value: m.Target}
	}
	if strings.HasPrefix(m.Source, "/") {
		return MountSource{Kind: MountSourceHostPath, Value: m.Source}
	}
	return MountSource{Kind: MountSourceNamedVolume, Value: m.Source}
}

// copyEnv returns an independent copy of env (nil in, nil out) so a
// Realization never aliases the Spec's map.
func copyEnv(env map[string]string) map[string]string {
	if env == nil {
		return nil
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = v
	}
	return out
}
