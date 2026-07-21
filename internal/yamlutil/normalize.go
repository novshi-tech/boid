// Package yamlutil holds small, dependency-free helpers shared between
// packages that decode agent/hook-authored YAML into Go values destined for
// json.Marshal. It has no internal boid imports so it can sit underneath
// both internal/orchestrator (business logic) and internal/sandbox (the
// in-sandbox CLI shim, which must not depend on orchestrator) without
// creating a layering cycle.
package yamlutil

import "fmt"

// NormalizeKeys recursively converts any map[interface{}]interface{} node
// (yaml.v3's decode shape for a YAML mapping with a non-string key — bool,
// int, or null) into map[string]interface{} so the result is always
// json.Marshal-able. Non-string keys are stringified with fmt.Sprint
// (true -> "true", 42 -> "42", nil -> "<nil>"). Already-string-keyed maps
// and slices are walked recursively so nested non-string keys anywhere in
// the tree are caught, not just at the top level. Scalars pass through
// unchanged.
//
// This exists because a YAML document can silently acquire a non-string key
// through an external round-trip (the historical incident this guards
// against: an agent wrote `on: verifying`, and a PyYAML round-trip turned it
// into `true: verifying` — YAML's plain scalar resolution treats bareword
// `on`/`off`/`yes`/`no` as booleans). Every caller that turns
// agent-authored YAML into JSON for the payload_patch pipeline must apply
// this normalization identically, or the same input behaves differently
// depending on which path (file-based job_done vs the `boid task update
// --payload-patch` RPC) carried it — see wiring-seams.md #17.
func NormalizeKeys(v interface{}) interface{} {
	switch x := v.(type) {
	case map[interface{}]interface{}:
		m := make(map[string]interface{}, len(x))
		for k, val := range x {
			ks, ok := k.(string)
			if !ok {
				ks = fmt.Sprint(k)
			}
			m[ks] = NormalizeKeys(val)
		}
		return m
	case map[string]interface{}:
		for k, val := range x {
			x[k] = NormalizeKeys(val)
		}
		return x
	case []interface{}:
		for i, val := range x {
			x[i] = NormalizeKeys(val)
		}
		return x
	default:
		return v
	}
}
