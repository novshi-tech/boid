package apiwire

import "time"

// docs/plans/signal-ingest-detailed-design.md §3.1/§3.2 (PR-2/PR-3): the
// daemon↔client wire contract for `GET /api/signals` / `POST
// /api/signals/ack` (PR-2's internal/api/signal_handler.go) AND the
// sandbox-side `boid signal list`/`boid signal ack` shim response
// (PR-3's internal/server/boid_executor.go) — both must reply with the SAME
// envelope shape regardless of which surface a caller uses (M3, PR #1014
// review: an earlier version of the sandbox side marshaled
// orchestrator.Signal directly, which has no json tags and so replied with
// raw Go field names — a different, undocumented shape from this one).
//
// PR-1 already shipped the store layer (internal/orchestrator/signal_store.go)
// and its SignalStore interface (internal/api/store.go) — this file is the
// wire shape the CLI (cmd/signal.go), internal/api/signal_handler.go, and
// internal/server/boid_executor.go all share, following the same apiwire
// split every other daemon↔client payload in this package uses (see
// doc.go): apiwire may import only the standard library and
// internal/orchestrator, so the CLI stays buildable for GOOS=windows/darwin.

// SignalSource is the provenance block of the Signal envelope
// (docs/plans/signal-driven-review.md §5.2: `source.pack` / `source.connector`
// / `source.service`). The signals table stores pack and connector together
// as a single "<pack>/<connector>" composite string (signal-ingest-detailed-
// design.md §2's `connector` column, and orchestrator.Signal.Connector) —
// SignalSource is what splits that back into the envelope's separate fields
// for the wire response. It is NOT what `--source` filters on: the CLI flag
// and SignalFilter.Connector both keep the "<pack>/<connector>" composite
// form, matching the stored column exactly (no split needed there).
type SignalSource struct {
	Pack      string `json:"pack"`
	Connector string `json:"connector"`
	Service   string `json:"service"`
}

// Signal is one inbox row shaped as the v0 envelope (signal-driven-review.md
// §5.2: id / occurred_at / source / identity / url / author / title) plus
// the daemon-side bookkeeping fields (received_at / attempts / acked_at) an
// operator needs when listing --state dead/acked/all. This is the wire
// contract `GET /api/signals` and `boid signal list -o json` (host) /
// `boid signal list` (sandbox) are all pinned to — deliberately NOT a
// straight mirror of orchestrator.Signal (whose Connector field is still
// the raw composite, and which has no envelope concept at all).
type Signal struct {
	ID         string       `json:"id"`
	OccurredAt time.Time    `json:"occurred_at"`
	Source     SignalSource `json:"source"`
	Identity   string       `json:"identity"`
	URL        string       `json:"url,omitempty"`
	Author     string       `json:"author,omitempty"`
	Title      string       `json:"title,omitempty"`
	ReceivedAt time.Time    `json:"received_at"`
	Attempts   int          `json:"attempts"`
	// AckedAt is nil while the signal is unacked.
	AckedAt *time.Time `json:"acked_at,omitempty"`
}

// ListSignalsResponse is GET /api/signals' response body, and also what
// `boid signal list`/`boid signal list --claim` reply with from inside the
// sandbox (PR-3) — same envelope on both surfaces (M3 above).
type ListSignalsResponse struct {
	Signals []Signal `json:"signals"`
}

// AckSignalsRequest is POST /api/signals/ack's request body.
// WorkspaceID defaults to orchestrator.DefaultWorkspaceSlug ("default")
// server-side when omitted, mirroring the CLI's --workspace default
// (signal-ingest-detailed-design.md §3.1).
//
// Ack is idempotent (signal-driven-review.md §14 Q14): re-acking an id
// that is already acked is not an error, matching
// orchestrator.AckSignals' own contract.
type AckSignalsRequest struct {
	WorkspaceID string   `json:"workspace_id,omitempty"`
	IDs         []string `json:"ids"`
}

// AckSignalsResponse is POST /api/signals/ack's response body: the
// (de-duplicated) ids that are now acked, whether this call is what acked
// them or an earlier call already had.
type AckSignalsResponse struct {
	Acked []string `json:"acked"`
}
