-- 0038: docs/plans/cross-project-issue-triage.md 論点11「代行Goタスク」の前提条件。
-- nose (人間操作) が押した action と代行タスク/workspace push (khi 等) が押した action が
-- actions ログ上で区別できないと、事故ったときに「人間の判断ミスか代行タスクの誤判定か」が
-- 復元不能になる。値は orchestrator.ActorHuman / ActorDaemon / ActorTask(taskID) のいずれか。
-- 既存行 (移行前データ) は空文字のまま残る。
ALTER TABLE actions ADD COLUMN actor TEXT NOT NULL DEFAULT '';
