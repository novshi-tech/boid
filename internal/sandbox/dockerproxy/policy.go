package dockerproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"
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
func CheckRequest(method, path string, body []byte, denyHostPortPublish bool) Verdict {
	bare := stripVersion(path)
	method = strings.ToUpper(method)

	// GET and HEAD are always allowed (read-only).
	if method == "GET" || method == "HEAD" {
		return allow()
	}

	// Explicit allow-list for mutating endpoints that don't need body inspection.
	// Everything not listed here is fail-closed (deny).
	if isAllowedMutating(method, bare) {
		return allow()
	}

	// Endpoints that require body inspection.
	if method == "POST" {
		switch {
		case bare == "/containers/create":
			return checkContainersCreate(body, denyHostPortPublish)
		case matchesPattern(bare, "/containers/*/exec"):
			return checkExecCreate(body)
		case matchesPattern(bare, "/containers/*/start"):
			return checkContainerStart(body)
		case bare == "/networks/create":
			return checkNetworksCreate(body)
		case bare == "/volumes/create":
			return checkVolumesCreate(body)
		case bare == "/build" || bare == "/session":
			return deny("image build is not permitted")
		}
	}

	// Fail-closed: unknown mutating endpoint.
	return deny("unknown mutating endpoint: " + method + " " + bare)
}

// isAllowedMutating returns true for mutating endpoints that are explicitly safe
// (no body inspection needed, transparent pass-through allowed).
func isAllowedMutating(method, bare string) bool {
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
			"/volumes/prune",
			"/containers/prune",
			"/images/prune",
			"/networks/prune",
			"/system/prune",
		)
	case "PUT":
		return false
	case "DELETE":
		return matchesAny(bare,
			"/containers/*",
			"/images/*",
			"/networks/*",
			"/volumes/*",
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
}

type mountSpec struct {
	Type          string      `json:"Type"`
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

func checkContainersCreate(body []byte, denyHostPortPublish bool) Verdict {
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

	for _, m := range hc.Mounts {
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
type volumesCreateBody struct {
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

func checkVolumesCreate(body []byte) Verdict {
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
