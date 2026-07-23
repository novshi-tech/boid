package orchestrator

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
)

func TraitMergeMode(trait TraitType) MergeMode {
	switch trait {
	case TraitVerification:
		return MergeModeShared
	default:
		return MergeModeExclusive
	}
}

func ActiveTraitTypes(raw json.RawMessage) ([]TraitType, error) {
	if len(raw) == 0 || string(raw) == "{}" || string(raw) == "null" {
		return nil, nil
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("unmarshal payload: %w", err)
	}

	var traits []TraitType
	for key, value := range payload {
		if string(value) != "null" {
			traits = append(traits, TraitType(key))
		}
	}
	return traits, nil
}

func ValidatePayloadPatch(patch json.RawMessage, allowedTraits []TraitType) error {
	if len(patch) == 0 || string(patch) == "{}" || string(patch) == "null" {
		return nil
	}

	var patchMap map[string]json.RawMessage
	if err := json.Unmarshal(patch, &patchMap); err != nil {
		return fmt.Errorf("unmarshal patch: %w", err)
	}

	allowed := make(map[TraitType]bool, len(allowedTraits))
	for _, trait := range allowedTraits {
		allowed[trait] = true
	}

	for key := range patchMap {
		if !allowed[TraitType(key)] {
			return fmt.Errorf("trait %q not in produces", key)
		}
	}
	return nil
}

// RejectReservedPayloadKeys returns an error if the payload contains writes to
// the artifact.children.* namespace (which is managed by virtual evaluation only).
func RejectReservedPayloadKeys(payload json.RawMessage) error {
	if len(payload) == 0 || string(payload) == "{}" || string(payload) == "null" {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil
	}
	artifactRaw, ok := m["artifact"]
	if !ok {
		return nil
	}
	var artifact map[string]json.RawMessage
	if err := json.Unmarshal(artifactRaw, &artifact); err != nil {
		return nil
	}
	if _, ok := artifact["children"]; ok {
		return fmt.Errorf("artifact.children.* is reserved")
	}
	return nil
}

// mergeObjectsShallow は base / patch が両方 JSON object のときに、 patch の
// top-level key を base に被せた結果を返す。 どちらか一方でも object でない
// (scalar / array / null) ときは ok=false を返し、 呼び出し側に whole-value
// overwrite を促す。 ネストは 1 段のみで、 同名 sub-key は patch 側が勝つ。
func mergeObjectsShallow(base, patch json.RawMessage) (json.RawMessage, bool) {
	var baseObj map[string]json.RawMessage
	if err := json.Unmarshal(base, &baseObj); err != nil || baseObj == nil {
		return nil, false
	}
	var patchObj map[string]json.RawMessage
	if err := json.Unmarshal(patch, &patchObj); err != nil || patchObj == nil {
		return nil, false
	}
	maps.Copy(baseObj, patchObj)
	merged, err := json.Marshal(baseObj)
	if err != nil {
		return nil, false
	}
	return merged, true
}

// mergeClaudeSessions unions two JSON arrays of `{"type","name","id"}`
// session entries (mirrors claude.session in
// internal/adapters/claude/run.go — kept structurally duck-typed here rather
// than importing that package, since orchestrator must stay adapter-agnostic).
// Entries are matched by (type, name); a patch entry wins over a base entry
// with the same key (last write for that specific session slot), but any
// base entry the patch doesn't mention is preserved. Order: base entries
// first (in place), then patch-only entries appended.
//
// ok=false (nil, false) when either side fails to parse as such an array —
// signals the caller to fall back to whole-value overwrite rather than
// silently dropping data it can't safely interpret.
//
// This exists to close PR #821 codex review Blocker 1: two claude hooks
// dispatched in parallel within the same readonly task round each apply
// their own session id via the `boid task update --payload-patch` RPC
// (claude.Adapter.Run, internal/adapters/claude/run.go). Each computes its
// own patch from an independent (possibly stale, pre-sibling-write) read of
// prior sessions, so a naive whole-value replace of
// `artifact.claude_code.sessions` — which is what the generic 1-level
// mergeObjectsShallow does for every other artifact sub-key — silently
// drops whichever session id the OTHER concurrent RPC call already
// persisted. Two DIFFERENT hook instances always have distinct (type, name)
// keys (name is the behaviour-instance name), so this union never actually
// collides in practice; a same-key write is deterministically resolved by
// letting the later-applied patch win, same as any other exclusive-trait
// overwrite.
func mergeClaudeSessions(existing, patch json.RawMessage) (json.RawMessage, bool) {
	if len(existing) == 0 || len(patch) == 0 {
		return nil, false
	}

	type sessionKey struct{ Type, Name string }
	keyOf := func(raw json.RawMessage) (sessionKey, bool) {
		var k struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &k); err != nil {
			return sessionKey{}, false
		}
		return sessionKey{Type: k.Type, Name: k.Name}, true
	}
	parseItems := func(raw json.RawMessage) ([]json.RawMessage, bool) {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, false
		}
		return items, true
	}

	existingItems, ok := parseItems(existing)
	if !ok {
		return nil, false
	}
	patchItems, ok := parseItems(patch)
	if !ok {
		return nil, false
	}

	order := make([]sessionKey, 0, len(existingItems)+len(patchItems))
	byKey := make(map[sessionKey]json.RawMessage, len(existingItems)+len(patchItems))
	for _, item := range existingItems {
		k, ok := keyOf(item)
		if !ok {
			return nil, false
		}
		if _, seen := byKey[k]; !seen {
			order = append(order, k)
		}
		byKey[k] = item
	}
	for _, item := range patchItems {
		k, ok := keyOf(item)
		if !ok {
			return nil, false
		}
		if _, seen := byKey[k]; !seen {
			order = append(order, k)
		}
		byKey[k] = item // patch wins on a matching key
	}

	merged := make([]json.RawMessage, 0, len(order))
	for _, k := range order {
		merged = append(merged, byKey[k])
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return nil, false
	}
	return out, true
}

// mergeClaudeCodeArtifact merges an incoming `artifact.claude_code` object
// into the existing one, giving the nested `sessions` array
// mergeClaudeSessions' union semantics instead of mergeObjectsShallow's
// whole-key replace. Every other sub-key of claude_code (there are none
// today, but this keeps the contract obvious for future additions) keeps
// ordinary shallow-replace semantics. ok=false when either side isn't a
// JSON object, signalling the caller to fall back further.
func mergeClaudeCodeArtifact(existing, patch json.RawMessage) (json.RawMessage, bool) {
	if len(existing) == 0 || len(patch) == 0 {
		return nil, false
	}
	var existingObj, patchObj map[string]json.RawMessage
	if err := json.Unmarshal(existing, &existingObj); err != nil || existingObj == nil {
		return nil, false
	}
	if err := json.Unmarshal(patch, &patchObj); err != nil || patchObj == nil {
		return nil, false
	}
	if mergedSessions, ok := mergeClaudeSessions(existingObj["sessions"], patchObj["sessions"]); ok {
		patchObj["sessions"] = mergedSessions
	}
	maps.Copy(existingObj, patchObj)
	merged, err := json.Marshal(existingObj)
	if err != nil {
		return nil, false
	}
	return merged, true
}

// mergeArtifactPatch merges an incoming `artifact` patch object into the
// existing `artifact` object with mergeObjectsShallow's ordinary 1-level
// shallow-replace semantics for every sub-key EXCEPT `claude_code`, which
// recurses one level further via mergeClaudeCodeArtifact so its `sessions`
// array gets append/union semantics rather than being replaced wholesale.
// ok=false when either side isn't a JSON object (mirrors mergeObjectsShallow's
// contract so the caller's existing non-object fallback still applies).
func mergeArtifactPatch(existing, patch json.RawMessage) (json.RawMessage, bool) {
	var existingObj, patchObj map[string]json.RawMessage
	if err := json.Unmarshal(existing, &existingObj); err != nil || existingObj == nil {
		return nil, false
	}
	if err := json.Unmarshal(patch, &patchObj); err != nil || patchObj == nil {
		return nil, false
	}
	if mergedCC, ok := mergeClaudeCodeArtifact(existingObj["claude_code"], patchObj["claude_code"]); ok {
		patchObj["claude_code"] = mergedCC
	}
	maps.Copy(existingObj, patchObj)
	merged, err := json.Marshal(existingObj)
	if err != nil {
		return nil, false
	}
	return merged, true
}

func MergePayloadPatch(base, patch json.RawMessage, handlerID string, allowedTraits []TraitType) (json.RawMessage, error) {
	if len(patch) == 0 || string(patch) == "{}" || string(patch) == "null" {
		if len(base) == 0 {
			return json.RawMessage("{}"), nil
		}
		return base, nil
	}
	if err := RejectReservedPayloadKeys(patch); err != nil {
		return nil, err
	}

	var allowed map[TraitType]bool
	if allowedTraits != nil {
		allowed = make(map[TraitType]bool, len(allowedTraits))
		for _, trait := range allowedTraits {
			allowed[trait] = true
		}
	}

	var baseMap map[string]json.RawMessage
	if len(base) == 0 || string(base) == "{}" || string(base) == "null" {
		baseMap = make(map[string]json.RawMessage)
	} else if err := json.Unmarshal(base, &baseMap); err != nil {
		return nil, fmt.Errorf("unmarshal base: %w", err)
	}

	var patchMap map[string]json.RawMessage
	if err := json.Unmarshal(patch, &patchMap); err != nil {
		return nil, fmt.Errorf("unmarshal patch: %w", err)
	}

	for key, value := range patchMap {
		trait := TraitType(key)
		if allowed != nil && !allowed[trait] {
			slog.Warn("dropping payload_patch trait not in produces", "trait", key, "handler_id", handlerID)
			continue
		}
		switch TraitMergeMode(trait) {
		case MergeModeShared:
			var shared map[string]json.RawMessage
			if existing, ok := baseMap[key]; ok && string(existing) != "null" {
				if err := json.Unmarshal(existing, &shared); err != nil {
					shared = make(map[string]json.RawMessage)
				}
			} else {
				shared = make(map[string]json.RawMessage)
			}
			shared[handlerID] = value
			merged, err := json.Marshal(shared)
			if err != nil {
				return nil, fmt.Errorf("marshal shared trait %q: %w", key, err)
			}
			baseMap[key] = merged
		default:
			// MergeModeExclusive: base/patch が両方 object のときは sub-key 単位で
			// merge する。 別フェーズや別 handler が同じ trait の異なる sub-key を
			// 書く合法ケースを守るため。 一方が scalar/array なら whole-value 上書き。
			if existing, ok := baseMap[key]; ok && len(existing) > 0 {
				// trait == "artifact" gets an extra level of care:
				// mergeArtifactPatch recurses into claude_code.sessions for
				// append/union semantics (PR #821 codex review Blocker 1 —
				// concurrent claude hooks silently losing a session id under
				// plain shallow replace). Every other exclusive trait, and
				// every other artifact sub-key, keeps the historical 1-level
				// mergeObjectsShallow behavior.
				if trait == TraitArtifact {
					if merged, ok := mergeArtifactPatch(existing, value); ok {
						baseMap[key] = merged
						continue
					}
				}
				if merged, ok := mergeObjectsShallow(existing, value); ok {
					baseMap[key] = merged
					continue
				}
			}
			baseMap[key] = value
		}
	}

	result, err := json.Marshal(baseMap)
	if err != nil {
		return nil, fmt.Errorf("marshal merged: %w", err)
	}
	return result, nil
}

// FilterPayloadByTraits returns a payload containing only the top-level keys
// matching the listed traits. If consumes is empty, an empty payload is returned.
func FilterPayloadByTraits(payload json.RawMessage, consumes []TraitType) json.RawMessage {
	if len(consumes) == 0 {
		return json.RawMessage("{}")
	}
	if len(payload) == 0 || string(payload) == "{}" || string(payload) == "null" {
		return json.RawMessage("{}")
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return json.RawMessage("{}")
	}

	allowed := make(map[TraitType]bool, len(consumes))
	for _, t := range consumes {
		allowed[t.Base()] = true
	}

	filtered := make(map[string]json.RawMessage, len(consumes))
	for key, val := range m {
		if allowed[TraitType(key)] {
			filtered[key] = val
		}
	}

	result, err := json.Marshal(filtered)
	if err != nil {
		return json.RawMessage("{}")
	}
	return result
}

func MergePayload(base, update json.RawMessage) (json.RawMessage, error) {
	if len(update) == 0 || string(update) == "{}" || string(update) == "null" {
		if len(base) == 0 {
			return json.RawMessage("{}"), nil
		}
		return base, nil
	}
	if len(base) == 0 || string(base) == "{}" || string(base) == "null" {
		return update, nil
	}

	var baseMap map[string]json.RawMessage
	if err := json.Unmarshal(base, &baseMap); err != nil {
		return nil, fmt.Errorf("unmarshal base: %w", err)
	}

	var updateMap map[string]json.RawMessage
	if err := json.Unmarshal(update, &updateMap); err != nil {
		return nil, fmt.Errorf("unmarshal update: %w", err)
	}

	for key, value := range updateMap {
		if string(value) == "null" {
			// null は既存 base 値を変えない (delete しない)
			continue
		}
		// base/update が両方 object なら 1 段 deep merge して sub-key を共存させる。
		// これにより artifact.report (agent 書込) と artifact.claude_code (runner patch)
		// が MergePayload 後も両方残る。
		if existing, ok := baseMap[key]; ok && len(existing) > 0 {
			if merged, ok := mergeObjectsShallow(existing, value); ok {
				baseMap[key] = merged
				continue
			}
		}
		baseMap[key] = value
	}

	result, err := json.Marshal(baseMap)
	if err != nil {
		return nil, fmt.Errorf("marshal merged: %w", err)
	}
	return result, nil
}
