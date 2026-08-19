package orchestrator

// docs/plans/ingestion-identity.md PR-1 (B-1): identity 索引の store 層。
//
// 外部キー (`jira:ROOKPF-289` / `slack:1785803139.540489` 等) から task を
// 引く索引で、認証の identity → principal と同じ構造 (I-1)。不変条件は 2 つ:
//
//   - 1 つの task は identity を複数持てる
//   - 1 つの identity は project スコープで高々 1 つの task にしか紐付かない
//     (UNIQUE(project_id, identity)、migration 0041)。2 枚目の link 要求は
//     silent newest-wins ではなく ErrIdentityConflict で拒否する
//
// identity は `<namespace>:<key>` の不透明文字列として扱う (I-2)。ここで
// 行う検証は「空でないこと」だけで、namespace の意味も key の妥当性も
// workspace 側の知識であり daemon は解釈しない。
//
// この PR だけでは挙動は変わらない — 書き手 (workspace の一括投入) も読み手
// (PR-2 の宛先解決) もまだ居ない土台。

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/novshi-tech/boid/internal/db"
)

// ErrIdentityConflict is returned by LinkIdentity when the given
// (projectID, identity) pair is already bound to a DIFFERENT task. Linking
// the SAME task again is idempotent (not a conflict) — see LinkIdentity's
// doc comment.
var ErrIdentityConflict = errors.New("identity already linked to a different task")

// LinkIdentity binds identity (scoped to projectID, I-3) to taskID. Linking
// an identity that already points at taskID is a no-op success (idempotent —
// a workspace resend of the same push must not fail). Linking an identity
// that already points at a DIFFERENT task returns ErrIdentityConflict; the
// existing binding is left untouched (this store never does silent
// newest-wins — see match_card.py's build_key_index() note in the design
// doc for the bug class this replaces).
func LinkIdentity(dbtx db.DBTX, projectID, identity, taskID string) error {
	if identity == "" {
		return fmt.Errorf("link identity: identity must not be empty")
	}
	if projectID == "" {
		return fmt.Errorf("link identity: project id must not be empty")
	}
	if taskID == "" {
		return fmt.Errorf("link identity: task id must not be empty")
	}

	existingTaskID, err := queryIdentityTaskID(dbtx, projectID, identity)
	if err != nil {
		return fmt.Errorf("link identity: query existing: %w", err)
	}
	if existingTaskID != "" {
		if existingTaskID == taskID {
			return nil
		}
		return ErrIdentityConflict
	}

	_, err = dbtx.Exec(
		`INSERT INTO task_identities (identity, project_id, task_id, created_at) VALUES (?, ?, ?, ?)`,
		identity, projectID, taskID, time.Now().UTC(),
	)
	if err == nil {
		return nil
	}

	// Concurrent insert race, mirroring CreateTask's own UNIQUE-constraint
	// fallback (store.go): another goroutine may have inserted the same
	// (project_id, identity) between the SELECT above and this INSERT.
	// Re-check rather than surfacing the raw constraint error.
	if strings.Contains(err.Error(), "UNIQUE constraint failed") {
		raceTaskID, qerr := queryIdentityTaskID(dbtx, projectID, identity)
		if qerr == nil && raceTaskID != "" {
			if raceTaskID == taskID {
				return nil
			}
			return ErrIdentityConflict
		}
	}
	return fmt.Errorf("link identity: insert: %w", err)
}

// UnlinkIdentity removes the binding for (projectID, identity), if any. Not
// finding one is not an error (idempotent, matching UnlinkAllForTask).
func UnlinkIdentity(dbtx db.DBTX, projectID, identity string) error {
	if identity == "" {
		return fmt.Errorf("unlink identity: identity must not be empty")
	}
	if projectID == "" {
		return fmt.Errorf("unlink identity: project id must not be empty")
	}
	if _, err := dbtx.Exec(
		`DELETE FROM task_identities WHERE project_id = ? AND identity = ?`,
		projectID, identity,
	); err != nil {
		return fmt.Errorf("unlink identity: %w", err)
	}
	return nil
}

// UnlinkAllForTask releases every identity bound to taskID (I-6: drop
// releases the task's identities so the same external keys can be re-linked
// to a fresh task later). Called from the service layer immediately after a
// `drop` transition commits — never from done (I-5: done holds identities).
func UnlinkAllForTask(dbtx db.DBTX, taskID string) error {
	if _, err := dbtx.Exec(`DELETE FROM task_identities WHERE task_id = ?`, taskID); err != nil {
		return fmt.Errorf("unlink all identities for task: %w", err)
	}
	return nil
}

// ResolveIdentity looks up the task bound to (projectID, identity). Returns
// ErrTaskNotFound (the same sentinel GetTask/FindTaskByRef use) when no
// binding exists — callers that need to distinguish "not found" from other
// errors should use errors.Is, matching this package's existing convention.
func ResolveIdentity(dbtx db.DBTX, projectID, identity string) (*Task, error) {
	if identity == "" {
		return nil, fmt.Errorf("resolve identity: identity must not be empty")
	}
	if projectID == "" {
		return nil, fmt.Errorf("resolve identity: project id must not be empty")
	}
	taskID, err := queryIdentityTaskID(dbtx, projectID, identity)
	if err != nil {
		return nil, fmt.Errorf("resolve identity: %w", err)
	}
	if taskID == "" {
		return nil, ErrTaskNotFound
	}
	return GetTask(dbtx, taskID)
}

// ListIdentitiesByTask returns every identity string bound to taskID (empty,
// not nil, when the task has none), ordered for determinism.
func ListIdentitiesByTask(dbtx db.DBTX, taskID string) ([]string, error) {
	rows, err := dbtx.Query(
		`SELECT identity FROM task_identities WHERE task_id = ? ORDER BY identity`,
		taskID,
	)
	if err != nil {
		return nil, fmt.Errorf("list identities by task: %w", err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var identity string
		if err := rows.Scan(&identity); err != nil {
			return nil, fmt.Errorf("list identities by task: scan: %w", err)
		}
		out = append(out, identity)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list identities by task: rows: %w", err)
	}
	return out, nil
}

// queryIdentityTaskID returns the task_id bound to (projectID, identity), or
// "" if no binding exists.
func queryIdentityTaskID(dbtx db.DBTX, projectID, identity string) (string, error) {
	var taskID string
	err := dbtx.QueryRow(
		`SELECT task_id FROM task_identities WHERE project_id = ? AND identity = ?`,
		projectID, identity,
	).Scan(&taskID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return taskID, nil
}
