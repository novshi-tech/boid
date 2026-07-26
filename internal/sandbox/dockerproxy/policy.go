package dockerproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/novshi-tech/boid/internal/dockerres"
)

// MaxBodyBytes caps body reads to prevent DoS / memory exhaustion.
const MaxBodyBytes = 32 * 1024 * 1024 // 32 MiB

// Verdict is the result of a policy check.
type Verdict struct {
	Allow  bool
	Reason string // populated when Allow==false
}

func allow() Verdict             { return Verdict{Allow: true} }
func deny(reason string) Verdict { return Verdict{Allow: false, Reason: reason} }

// ReservedVolumes is the set of docker volume names a sandboxed client may
// neither create, delete, nor mount. It has two halves and they are ADDITIVE,
// never alternatives:
//
//   - dockerres.IsReservedVolumeName — the static "boid-ws-" namespace boid
//     names itself and can therefore recognize without asking anyone (PR1,
//     docs/plans/workspace-home-volume-persistence.md 論点 a).
//   - names, the set discovered at daemon startup by inspecting the daemon's
//     OWN container (internal/dispatcher.DetectDaemonStateVolumes). These
//     cannot be recognized by shape: under the compose deploy the daemon's
//     entire persistent state lives in a volume the COMPOSE PROJECT names
//     ("boid_boid_state" — build/container/compose.yml's `name: boid` plus the
//     `boid_state` key), so boid does not choose the string and cannot pattern
//     match it. It shares no prefix with anything boid generates, which is
//     exactly why PR1's prefix rule left the single highest-value volume in
//     the deployment — boid.db, secret.key, tls/ca.key, web_secret, install_id
//     — mountable from inside any `capabilities.docker` job.
//
// Two cheaper-looking alternatives were considered and rejected by the owner
// (2026-07-26): denying a blanket "boid_"/"boid-" prefix (misfires on an
// operator's own `boid_myapp` volume, and stops protecting anything the
// moment the compose project is renamed), and denying every mount whose
// volume is not in the job's own ledger (the tightest rule available, but it
// breaks legitimate sibling use such as testcontainers reusing a fixture
// volume).
//
// The zero value is valid and means "static prefix only" — the exact pre-PR
// behavior. That is deliberate: bare `boid start` has no state volume to
// protect, and a detection failure must not silently swap one rule for
// another (see TestZeroReservedVolumes_behavesExactlyLikePR1).
type ReservedVolumes struct {
	// names is the runtime half. Kept unexported and only ever built by
	// NewReservedVolumes so a caller cannot mutate a Server's live policy
	// input after startup — CheckRequest's purity depends on this value
	// being fixed before the first request, not on it being immutable Go.
	names map[string]struct{}
}

// NewReservedVolumes captures names as the runtime half of the reserved set.
// Empty/blank entries are dropped: an empty Source or Name is how a client
// spells "anonymous volume", and reserving "" would deny every one of those.
func NewReservedVolumes(names []string) ReservedVolumes {
	if len(names) == 0 {
		return ReservedVolumes{}
	}
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		if n == "" {
			continue
		}
		set[n] = struct{}{}
	}
	if len(set) == 0 {
		return ReservedVolumes{}
	}
	return ReservedVolumes{names: set}
}

// IsReserved reports whether a sandboxed docker client is forbidden from
// naming this volume, for any of create / delete / mount.
//
// Exact string match on the runtime half, no prefix semantics: the names come
// from the engine's own answer about this very container, so there is nothing
// to generalize from and every generalization ("anything starting with the
// compose project name") is a way to catch an operator's unrelated volume.
func (r ReservedVolumes) IsReserved(name string) bool {
	if dockerres.IsReservedVolumeName(name) {
		return true
	}
	_, ok := r.names[name]
	return ok
}

// explain returns a parenthetical naming WHY the volume is reserved, appended
// to the deny reason. The two halves fail for genuinely different reasons and
// an operator staring at a 403 needs to tell them apart: "you tried to touch
// another workspace's HOME" is a very different report from "you tried to
// mount the daemon's own secrets". Empty for the static half, whose existing
// message ("reserved by boid") is unchanged so PR1's tests still describe the
// behavior they were written for.
func (r ReservedVolumes) explain(name string) string {
	if _, ok := r.names[name]; ok {
		return " — it is this boid daemon's own state volume (boid.db, secret.key," +
			" the internal mTLS CA and the web session key live in it)"
	}
	return ""
}

// apiVersionRe matches /v<major>.<minor> prefix.
var apiVersionRe = regexp.MustCompile(`^/v\d+\.\d+(/.*)?$`)

// stripVersion removes the /vN.N prefix from path so routing can use bare paths.
func stripVersion(path string) string {
	if !apiVersionRe.MatchString(path) {
		return path
	}
	// find second slash
	idx := strings.Index(path[1:], "/")
	if idx < 0 {
		return "/"
	}
	return path[1+idx:]
}

// CheckRequest is the main policy entry point.
// method is the HTTP method, path is the raw request path (may include API version prefix),
// body is the request body bytes (may be nil for GET/HEAD).
//
// denyHostPortPublish gates the HostConfig.PortBindings / PublishAllPorts
// checks in checkContainersCreate (Blocker 2, PR6 codex review). It must
// be true only for the container backend's Server — once
// Server.SetWorkspaceNetwork has configured a real per-job workspace
// network (see its own doc comment) — and false for every other caller,
// most importantly the pre-PR6 userns backend's Server
// (internal/dispatcher.Runner.startDockerProxy, which never calls
// SetWorkspaceNetwork). §決定5's "host への port publish は非サポート" is a
// container-backend-only guarantee: the userns backend has always allowed
// a sandboxed docker client (a TestContainers sibling, `docker run -p`,
// `docker run -P`, ...) to publish a port to the host, and denying it
// unconditionally (the pre-fix behavior) would silently turn every one of
// those existing userns hooks into a 403.
//
// reserved is the volume-name set the three name-dependent branches below
// consult (POST /volumes/create's Name, DELETE /volumes/{name}, and
// POST /containers/create's HostConfig.Mounts[].Source). It is a plain VALUE
// computed once at daemon startup and handed in, never a callback or an
// engine handle: this function stays a pure function of its arguments so it
// remains auditable and so no policy decision can depend on a live query
// whose answer could change (or hang) mid-request. See ReservedVolumes for
// what goes in it and why the daemon's own state volume cannot be recognized
// by shape the way the "boid-ws-" namespace can. The zero value reproduces
// the pre-existing behavior exactly.
func CheckRequest(method, path string, body []byte, denyHostPortPublish bool, reserved ReservedVolumes) Verdict {
	bare := stripVersion(path)
	method = strings.ToUpper(method)

	// GET and HEAD are always allowed (read-only).
	if method == "GET" || method == "HEAD" {
		return allow()
	}

	// Explicit allow-list for mutating endpoints that don't need body inspection.
	// Everything not listed here is fail-closed (deny).
	if isAllowedMutating(method, bare, reserved) {
		return allow()
	}

	// The two volume-destruction shapes isAllowedMutating deliberately
	// refuses, spelled out here so the client gets a reason it can act on
	// rather than the generic "unknown mutating endpoint" fail-closed
	// message (docs/plans/workspace-home-volume-persistence.md 論点 a 経路 4).
	if method == "DELETE" && matchesPattern(bare, "/volumes/*") {
		// Reaching here means isAllowedMutating's own name check refused it.
		_, name := scopeTarget(bare)
		return deny(fmt.Sprintf("volumes/delete: volume name %q is reserved by boid%s", name, reserved.explain(name)))
	}
	if method == "POST" && bare == "/volumes/prune" {
		// The fifth destruction path, and the only one that cannot be
		// name-filtered: `docker volume prune` destroys every volume no
		// container currently references, and the workspace HOME volumes of
		// every OTHER workspace are exactly in that state during any given
		// job. The request carries no name to inspect, so the endpoint is
		// closed outright. This is the only sandbox-visible behavior change
		// PR1 makes; the other prunes (containers/images/networks/system)
		// stay allowed because none of them touches volumes.
		return deny("volumes/prune: pruning volumes is not permitted — it would destroy boid's persistent workspace HOME volumes, which no container in this job references")
	}

	// Endpoints that require body inspection.
	if method == "POST" {
		switch {
		case bare == "/containers/create":
			return checkContainersCreate(body, denyHostPortPublish, reserved)
		case matchesPattern(bare, "/containers/*/exec"):
			return checkExecCreate(body)
		case matchesPattern(bare, "/containers/*/start"):
			return checkContainerStart(body)
		case bare == "/networks/create":
			return checkNetworksCreate(body)
		case bare == "/volumes/create":
			return checkVolumesCreate(body, reserved)
		case bare == "/build" || bare == "/session":
			return deny("image build is not permitted")
		}
	}

	// Fail-closed: unknown mutating endpoint.
	return deny("unknown mutating endpoint: " + method + " " + bare)
}

// isAllowedMutating returns true for mutating endpoints that are explicitly safe
// (no body inspection needed, transparent pass-through allowed).
//
// Two volume endpoints that used to be listed here no longer are
// (docs/plans/workspace-home-volume-persistence.md 論点 a 経路 4):
// POST /volumes/prune is gone entirely, and DELETE /volumes/{name} moved out
// of the pattern list into a name-dependent branch. CheckRequest turns both
// refusals into an explicit deny reason.
//
// reserved is threaded in (rather than the DELETE branch calling
// dockerres.IsReservedVolumeName directly, as it did in PR1) so the static
// namespace and the runtime-detected daemon state volume are consulted
// through ONE predicate. A second, independently-maintained call site is how
// the two halves drift apart — see internal/dockerres' package doc for the
// failure mode that whole package exists to prevent.
func isAllowedMutating(method, bare string, reserved ReservedVolumes) bool {
	switch method {
	case "POST":
		return matchesAny(bare,
			"/containers/*/stop",
			"/containers/*/wait",
			"/containers/*/kill",
			"/containers/*/restart",
			"/containers/*/pause",
			"/containers/*/unpause",
			"/containers/*/attach",
			"/containers/*/resize",
			"/containers/*/rename",
			"/exec/*/start",
			"/exec/*/resize",
			"/images/create", // pull
			"/images/*/tag",
			"/images/*/push",
			"/networks/*/connect",
			"/networks/*/disconnect",
			"/containers/prune",
			"/images/prune",
			"/networks/prune",
			"/system/prune",
		)
	case "PUT":
		return false
	case "DELETE":
		if matchesPattern(bare, "/volumes/*") {
			// Volume names are the one resource id in this API a sandboxed
			// client can choose freely AND that boid also derives
			// deterministically, so the same name can denote boid's own
			// persistent volume. Everything in the reserved namespace is
			// therefore off limits; scopeTarget already does this extraction
			// for the ledger scope check.
			_, name := scopeTarget(bare)
			return !reserved.IsReserved(name)
		}
		// Networks are deliberately NOT subject to the reserved-name rule
		// even though boid's own per-workspace network names share the
		// "boid-ws-" prefix: boid names those itself, they are recreated on
		// demand, and the ledger scope check already stops a sandbox from
		// deleting a network it did not create.
		return matchesAny(bare,
			"/containers/*",
			"/images/*",
			"/networks/*",
		)
	}
	return false
}

// matchesPattern checks a bare path against a glob-style pattern where * matches
// a single path segment (no slashes).
func matchesPattern(path, pattern string) bool {
	pparts := strings.Split(pattern, "/")
	aparts := strings.Split(path, "/")
	if len(pparts) != len(aparts) {
		return false
	}
	for i, p := range pparts {
		if p == "*" {
			continue
		}
		if p != aparts[i] {
			return false
		}
	}
	return true
}

func matchesAny(path string, patterns ...string) bool {
	for _, p := range patterns {
		if matchesPattern(path, p) {
			return true
		}
	}
	return false
}

// --- body inspection structs ---

// containerCreateBody mirrors the Docker API POST /containers/create body.
// Only dangerous fields are listed; unknown safe fields are ignored.
type containerCreateBody struct {
	HostConfig hostConfig `json:"HostConfig"`
}

type hostConfig struct {
	Binds             []string          `json:"Binds"`
	Mounts            []mountSpec       `json:"Mounts"`
	Privileged        bool              `json:"Privileged"`
	NetworkMode       string            `json:"NetworkMode"`
	PidMode           string            `json:"PidMode"`
	IpcMode           string            `json:"IpcMode"`
	UsernsMode        string            `json:"UsernsMode"`
	CgroupnsMode      string            `json:"CgroupnsMode"`
	SecurityOpt       []string          `json:"SecurityOpt"`
	CapAdd            []string          `json:"CapAdd"`
	Devices           []interface{}     `json:"Devices"`
	DeviceCgroupRules []string          `json:"DeviceCgroupRules"`
	Runtime           string            `json:"Runtime"`
	Sysctls           map[string]string `json:"Sysctls"`
	CgroupParent      string            `json:"CgroupParent"`
	// PortBindings / PublishAllPorts: publishing a sibling container's port
	// to the host would let it be reached directly by host IP, bypassing
	// the workspace internal network entirely (docs/plans/
	// phase6-container-backend.md §PR6, §決定5: "host への port publish は
	// 非サポート (internal network から host published port へは届かない)").
	// PublishAllPorts (docker CLI's `-P` — auto-publish every EXPOSEd port
	// to a random host port) is the exact same host-escape shape as an
	// explicit PortBindings entry. Both are gated together on
	// denyHostPortPublish (Blocker 2, PR6 codex review) — see
	// CheckRequest's doc comment for why this is not an unconditional deny.
	PortBindings    map[string]interface{} `json:"PortBindings"`
	PublishAllPorts bool                   `json:"PublishAllPorts"`

	// VolumesFrom inherits ANOTHER container's entire mount set
	// (docker run --volumes-from). It is denied outright whenever it is
	// non-empty — docs/plans/workspace-home-volume-persistence.md 論点 a
	// 経路 7, PR1 codex review Blocker 2.
	//
	// The reserved-name rule that guards Mounts/Binds/volumes-create cannot
	// be applied here, because the request never names a volume: it names a
	// CONTAINER, and what that container mounts is not knowable from the
	// request body. Answering "does this inherit a boid volume?" would mean
	// the policy issuing its own inspect call against the upstream engine and
	// then trusting a TOCTOU-prone answer — CheckRequest is a pure function
	// over (method, path, body) precisely so it is auditable, and it has no
	// engine handle to do that with. So the check is "is it non-empty", the
	// only question the body can answer.
	//
	// Denying costs a sandboxed client nothing real: it can mount its own
	// named volumes into as many sibling containers as it likes via Mounts,
	// which is the modern spelling of the same thing and stays allowed. The
	// only capability lost is inheriting mounts from a container whose mount
	// set this policy never got to inspect — including, once PR6 lands and
	// the job container itself mounts the workspace HOME volume, that very
	// container.
	VolumesFrom []string `json:"VolumesFrom"`
}

type mountSpec struct {
	Type string `json:"Type"`

	// Source is the named volume (or host path, for Type=bind) the mount
	// attaches. checkContainersCreate rejects any Source in boid's reserved
	// volume namespace, REGARDLESS of Type
	// (docs/plans/workspace-home-volume-persistence.md 論点 a 経路 6, PR1
	// codex review Blocker 2). Before this field existed, a mount was only
	// inspected via VolumeOptions, so a sandboxed client could attach the
	// real workspace HOME volume to a throwaway container and `rm -rf` its
	// mountpoint: the volume object survives every containment 経路 4 added
	// (nothing is created, nothing is deleted) while the credentials and
	// toolchain inside it are gone.
	//
	// This is also the branch that closes the daemon's OWN state volume
	// (docs/plans/workspace-home-volume-persistence.md, "未解決" → resolved):
	// mounting it needs no write access at all, so the other two name paths
	// (create/delete) were never what stood between a `capabilities.docker`
	// job and `cat /state/.local/share/boid/secret.key`. See ReservedVolumes
	// for why that name has to be discovered at runtime rather than matched.
	//
	// Why the check ignores Type rather than firing only on
	// strings.ToLower(Type)=="volume": the policy would otherwise have to
	// model the engine's own Type normalization exactly — what an omitted
	// Type defaults to, how a future or vendor-specific type name is
	// resolved, whether docker and podman agree — and any divergence becomes
	// a bypass. This file already treats that class of parser differential as
	// the threat model to design against (see the
	// TestParserDifferential_* cases and the case-insensitive field matching
	// they pin). Checking Source unconditionally makes the rule independent
	// of that modelling: for Type=bind a reserved Source is already denied by
	// the blanket bind rule, and for every other type a Source inside boid's
	// namespace has no legitimate meaning for a sandboxed client, so nothing
	// legitimate is lost by refusing it.
	Source string `json:"Source"`

	VolumeOptions *volumeOpts `json:"VolumeOptions"`
}

type volumeOpts struct {
	DriverConfig *driverConfig `json:"DriverConfig"`
}

type driverConfig struct {
	Name    string            `json:"Name"`
	Options map[string]string `json:"Options"`
}

// execCreateBody mirrors POST /containers/{id}/exec body.
type execCreateBody struct {
	Privileged bool `json:"Privileged"`
}

// containerStartBody mirrors POST /containers/{id}/start body (legacy HostConfig).
type containerStartBody struct {
	HostConfig *json.RawMessage `json:"HostConfig"`
}

func readBody(body []byte) ([]byte, bool) {
	if body == nil {
		return nil, true
	}
	r := io.LimitReader(bytes.NewReader(body), MaxBodyBytes+1)
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, false
	}
	if len(b) > MaxBodyBytes {
		return nil, false // exceeded limit
	}
	return b, true
}

func checkContainersCreate(body []byte, denyHostPortPublish bool, reserved ReservedVolumes) Verdict {
	b, ok := readBody(body)
	if !ok {
		return deny("body exceeds maximum size limit")
	}
	if len(b) == 0 {
		return allow()
	}

	var req containerCreateBody
	if err := json.Unmarshal(b, &req); err != nil {
		return deny("invalid JSON body")
	}

	hc := req.HostConfig

	if len(hc.Binds) > 0 {
		return deny("HostConfig.Binds: bind mounts are not permitted")
	}

	if len(hc.VolumesFrom) > 0 {
		return deny("HostConfig.VolumesFrom: inheriting another container's mounts is not permitted " +
			"(the request names a container, not a volume, so this proxy cannot verify what would be inherited — " +
			"it could include boid's persistent workspace HOME volume; mount your own named volumes via HostConfig.Mounts instead)")
	}

	for _, m := range hc.Mounts {
		// Type-independent by design — see mountSpec.Source's doc comment.
		if reserved.IsReserved(m.Source) {
			return deny(fmt.Sprintf("HostConfig.Mounts: mount source %q is reserved by boid%s",
				m.Source, reserved.explain(m.Source)))
		}
		switch strings.ToLower(m.Type) {
		case "bind":
			return deny("HostConfig.Mounts: type=bind mount is not permitted")
		case "volume":
			if v := m.VolumeOptions; v != nil {
				if dc := v.DriverConfig; dc != nil && dc.Options != nil {
					if _, hasDevice := dc.Options["device"]; hasDevice {
						return deny("HostConfig.Mounts: volume with device option (system 3 bind) is not permitted")
					}
					if o := dc.Options["o"]; strings.Contains(o, "bind") {
						return deny("HostConfig.Mounts: volume with o=bind option (system 3 bind) is not permitted")
					}
				}
			}
		}
	}

	if hc.Privileged {
		return deny("HostConfig.Privileged: privileged containers are not permitted")
	}

	if v := hc.NetworkMode; v != "" {
		if isDangerousMode(v) {
			return deny("HostConfig.NetworkMode: " + v + " is not permitted")
		}
	}

	if denyHostPortPublish {
		if len(hc.PortBindings) > 0 {
			return deny("HostConfig.PortBindings: publishing ports to the host is not permitted")
		}
		if hc.PublishAllPorts {
			return deny("HostConfig.PublishAllPorts: publishing ports to the host is not permitted")
		}
	}

	if v := hc.PidMode; v != "" {
		if isDangerousMode(v) {
			return deny("HostConfig.PidMode: " + v + " is not permitted")
		}
	}

	if v := hc.IpcMode; v != "" {
		if isDangerousMode(v) {
			return deny("HostConfig.IpcMode: " + v + " is not permitted")
		}
	}

	if hc.UsernsMode == "host" {
		return deny("HostConfig.UsernsMode: host is not permitted")
	}

	if hc.CgroupnsMode == "host" {
		return deny("HostConfig.CgroupnsMode: host is not permitted")
	}

	if len(hc.SecurityOpt) > 0 {
		return deny("HostConfig.SecurityOpt: security options are not permitted")
	}

	if len(hc.CapAdd) > 0 {
		return deny("HostConfig.CapAdd: adding capabilities is not permitted")
	}

	if len(hc.Devices) > 0 {
		return deny("HostConfig.Devices: device access is not permitted")
	}

	if len(hc.DeviceCgroupRules) > 0 {
		return deny("HostConfig.DeviceCgroupRules: device cgroup rules are not permitted")
	}

	if hc.Runtime != "" && hc.Runtime != "runc" {
		return deny("HostConfig.Runtime: only runc runtime is permitted, got " + hc.Runtime)
	}

	if len(hc.Sysctls) > 0 {
		return deny("HostConfig.Sysctls: sysctl settings are not permitted")
	}

	if hc.CgroupParent != "" {
		return deny("HostConfig.CgroupParent: custom cgroup parent is not permitted")
	}

	return allow()
}

// networksCreateBody mirrors the Docker API POST /networks/create body.
type networksCreateBody struct {
	Driver string `json:"Driver"`
}

// volumesCreateBody mirrors the Docker API POST /volumes/create body.
//
// Name is inspected because VolumeCreate is IDEMPOTENT: for an existing
// name docker returns the existing volume with 201 rather than an error. A
// sandboxed client naming boid's own (deterministic, therefore guessable)
// workspace HOME volume would thus "create" the real one, get it recorded
// in the job's ledger, and have Reap delete it when the job ends —
// docs/plans/workspace-home-volume-persistence.md 論点 a 経路 4.
type volumesCreateBody struct {
	Name       string            `json:"Name"`
	DriverOpts map[string]string `json:"DriverOpts"`
}

func checkNetworksCreate(body []byte) Verdict {
	b, ok := readBody(body)
	if !ok {
		return deny("body exceeds maximum size limit")
	}
	if len(b) == 0 {
		return allow()
	}
	var req networksCreateBody
	if err := json.Unmarshal(b, &req); err != nil {
		return deny("invalid JSON body")
	}
	// "host" driver gives the container full access to the host network stack.
	if req.Driver == "host" {
		return deny("networks/create: Driver=host is not permitted")
	}
	return allow()
}

func checkVolumesCreate(body []byte, reserved ReservedVolumes) Verdict {
	b, ok := readBody(body)
	if !ok {
		return deny("body exceeds maximum size limit")
	}
	if len(b) == 0 {
		return allow()
	}
	var req volumesCreateBody
	if err := json.Unmarshal(b, &req); err != nil {
		return deny("invalid JSON body")
	}
	// Reserved namespace: see volumesCreateBody.Name's doc comment. An empty
	// Name is an anonymous volume — docker generates the id, so it can never
	// collide with boid's.
	if reserved.IsReserved(req.Name) {
		return deny(fmt.Sprintf("volumes/create: volume name %q is reserved by boid%s",
			req.Name, reserved.explain(req.Name)))
	}
	// DriverOpts with device= or o=bind is a host bind mount via local driver (system 3).
	if _, hasDevice := req.DriverOpts["device"]; hasDevice {
		return deny("volumes/create: DriverOpts.device (host bind mount) is not permitted")
	}
	if o := req.DriverOpts["o"]; strings.Contains(o, "bind") {
		return deny("volumes/create: DriverOpts.o=bind (host bind mount) is not permitted")
	}
	return allow()
}

// isDangerousMode returns true for host/container:/ns: namespace sharing modes.
func isDangerousMode(mode string) bool {
	if mode == "host" {
		return true
	}
	if strings.HasPrefix(mode, "container:") {
		return true
	}
	if strings.HasPrefix(mode, "ns:") {
		return true
	}
	return false
}

func checkExecCreate(body []byte) Verdict {
	b, ok := readBody(body)
	if !ok {
		return deny("body exceeds maximum size limit")
	}
	if len(b) == 0 {
		return allow()
	}

	var req execCreateBody
	if err := json.Unmarshal(b, &req); err != nil {
		return deny("invalid JSON body")
	}

	if req.Privileged {
		return deny("exec Privileged: privileged exec is not permitted")
	}

	return allow()
}

// scopeTarget extracts the resource type and ID that must be scope-checked for
// the given bare path (version prefix already stripped).  Returns ("", "") for
// paths that do not carry a resource ID (creation endpoints, list endpoints,
// and paths that do not belong to a tracked resource type).
func scopeTarget(bare string) (resourceType, id string) {
	trimmed := strings.TrimPrefix(bare, "/")
	slash := strings.IndexByte(trimmed, '/')
	var resource, rest string
	if slash < 0 {
		resource = trimmed
	} else {
		resource = trimmed[:slash]
		rest = trimmed[slash+1:]
	}

	// Extract the first segment after the resource type (the ID or a keyword).
	seg := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		seg = rest[:i]
	}
	if seg == "" {
		return "", ""
	}

	switch resource {
	case "containers":
		switch seg {
		case "create", "json", "prune":
			return "", ""
		}
		return "container", seg
	case "networks":
		switch seg {
		case "create", "prune":
			return "", ""
		}
		return "network", seg
	case "volumes":
		switch seg {
		case "create", "prune":
			return "", ""
		}
		return "volume", seg
	case "exec":
		return "exec", seg
	}
	return "", ""
}

// creationResourceType returns the resource type name and the JSON field that
// holds the new resource's ID in the upstream response, for endpoints that
// create new Docker resources.  Returns ("", "") for non-creation paths.
func creationResourceType(method, bare string) (resourceType, idField string) {
	if method != "POST" {
		return "", ""
	}
	switch {
	case bare == "/containers/create":
		return "container", "Id"
	case bare == "/networks/create":
		return "network", "Id"
	case bare == "/volumes/create":
		return "volume", "Name"
	case matchesPattern(bare, "/containers/*/exec"):
		return "exec", "Id"
	}
	return "", ""
}

func checkContainerStart(body []byte) Verdict {
	b, ok := readBody(body)
	if !ok {
		return deny("body exceeds maximum size limit")
	}
	if len(b) == 0 {
		return allow()
	}

	// Empty object or whitespace-only is fine.
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("{}")) {
		return allow()
	}

	var req containerStartBody
	if err := json.Unmarshal(b, &req); err != nil {
		return deny("invalid JSON body")
	}

	if req.HostConfig != nil {
		// HostConfig present — check it's not null and not empty.
		raw := bytes.TrimSpace([]byte(*req.HostConfig))
		if len(raw) > 0 && !bytes.Equal(raw, []byte("null")) && !bytes.Equal(raw, []byte("{}")) {
			return deny("POST /containers/{id}/start with HostConfig is not permitted")
		}
	}

	return allow()
}
