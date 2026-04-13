-- Safety migration: resolve any duplicate (parent_id, ref) pairs that may
-- exist in databases where the unique index from migration 0010 was not
-- enforced. On databases where 0010 ran successfully, this UPDATE is a no-op.
--
-- For each duplicate group (same parent_id, same ref, ref != ''), all records
-- except the earliest by created_at (tie-broken by id) are renamed by appending
-- a _dup1, _dup2, ... suffix (1-based count of predecessors in the group).
UPDATE tasks
SET ref = ref || '_dup' || (
    SELECT COUNT(*)
    FROM tasks t2
    WHERE t2.ref = tasks.ref
      AND t2.parent_id = tasks.parent_id
      AND t2.ref != ''
      AND (t2.created_at < tasks.created_at
           OR (t2.created_at = tasks.created_at AND t2.id < tasks.id))
)
WHERE ref != ''
  AND (
    SELECT COUNT(*)
    FROM tasks t2
    WHERE t2.ref = tasks.ref
      AND t2.parent_id = tasks.parent_id
      AND t2.ref != ''
      AND (t2.created_at < tasks.created_at
           OR (t2.created_at = tasks.created_at AND t2.id < tasks.id))
  ) > 0;
