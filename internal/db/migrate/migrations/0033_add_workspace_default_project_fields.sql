-- docs/plans/workspace-default-project.md §PR分割案 PR3: workspace 単位の
-- デフォルト project 定義 (task_behaviors / base_branch / fork_point /
-- default_task_behavior) を workspaces テーブルに追加する。この migration の
-- 時点ではまだ誰も読まない — GetWithWorkspace のマージに組み込まれるのは
-- PR4 から (§PR分割案 PR3 冒頭)。
--
-- task_behaviors だけ他の JSON 列と違い YAML で保存する: TaskBehavior.Hooks
-- は `json:"-"` (API レスポンスから意図的に除外、spec_types.go) のため、
-- encoding/json で marshal すると hooks の定義が黙って消える。yaml.v3 は
-- `yaml:"hooks,omitempty"` を持つため hooks を正しく保存できる
-- (decodeWorkspaceMetaColumns / marshalWorkspaceMetaColumns 参照)。
ALTER TABLE workspaces ADD COLUMN task_behaviors TEXT NOT NULL DEFAULT '';
ALTER TABLE workspaces ADD COLUMN base_branch TEXT NOT NULL DEFAULT '';
ALTER TABLE workspaces ADD COLUMN fork_point TEXT NOT NULL DEFAULT '';
ALTER TABLE workspaces ADD COLUMN default_task_behavior TEXT NOT NULL DEFAULT '';
