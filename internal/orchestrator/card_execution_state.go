package orchestrator

import (
	"fmt"
	"strings"

	"github.com/novshi-tech/boid/internal/db"
)

// CardExecutionState summarizes a card's shared execution slot and input
// requests anywhere below it. Occupied includes launching and attached Go
// and command requests, but excludes queued requests.
type CardExecutionState struct {
	Occupied     bool
	NeedsInput   bool
	CommandLabel string
}

// CardExecutionStatesByIDs reads one page in a single consistent SQL snapshot.
// UNION (not UNION ALL) bounds traversal even if parent links contain a cycle.
// A command's task target may not have parent_id set, so it is also a seed.
func CardExecutionStatesByIDs(dbtx db.DBTX, cardIDs []string) (map[string]CardExecutionState, error) {
	out := map[string]CardExecutionState{}
	for start := 0; start < len(cardIDs); start += existingTaskIDsChunk {
		end := min(start+existingTaskIDsChunk, len(cardIDs))
		if err := scanCardExecutionStates(dbtx, cardIDs[start:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func scanCardExecutionStates(dbtx db.DBTX, cardIDs []string, out map[string]CardExecutionState) error {
	args := make([]any, len(cardIDs))
	for i, id := range cardIDs {
		args[i] = id
	}
	query := `WITH RECURSIVE
		cards AS (SELECT id FROM tasks WHERE type = 'card' AND id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(cardIDs)), ",") + `)),
		slots AS (
			SELECT r.* FROM card_requests r JOIN cards ON cards.id = r.card_id
			WHERE r.status IN ('launching', 'attached')
		),
		descendants(card_id, task_id) AS (
			SELECT cards.id, t.id FROM cards JOIN tasks t ON t.parent_id = cards.id
			UNION
			SELECT card_id, target_id FROM slots WHERE target_kind = 'task'
			UNION
			SELECT d.card_id, t.id FROM descendants d JOIN tasks t ON t.parent_id = d.task_id
		),
		awaiting AS (
			SELECT DISTINCT d.card_id FROM descendants d JOIN tasks t ON t.id = d.task_id
			WHERE t.status = 'awaiting'
		)
		SELECT cards.id, slots.id IS NOT NULL, awaiting.card_id IS NOT NULL,
			CASE WHEN slots.command_key = '__go__' THEN ''
			ELSE COALESCE(NULLIF(slots.launched_label, ''), slots.command_key, '') END
		FROM cards LEFT JOIN slots ON slots.card_id = cards.id
		LEFT JOIN awaiting ON awaiting.card_id = cards.id`
	rows, err := dbtx.Query(query, args...)
	if err != nil {
		return fmt.Errorf("read card execution states: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var state CardExecutionState
		if err := rows.Scan(&id, &state.Occupied, &state.NeedsInput, &state.CommandLabel); err != nil {
			return fmt.Errorf("scan card execution state: %w", err)
		}
		out[id] = state
	}
	return rows.Err()
}

func (r *TaskRepository) CardExecutionStatesByIDs(cardIDs []string) (map[string]CardExecutionState, error) {
	return CardExecutionStatesByIDs(r.db, cardIDs)
}
