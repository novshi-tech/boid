# Repository 設定の移行

daemon は project 設定ファイルを読みません。`boid project init` と `boid project migrate` は撤去済みです。

1. `boid workspace export <slug>` で移行先 workspace の envelope を取得します。
2. behavior、hook、trigger、signals.sources、card_commands、card_events を `spec` に移します。実行環境設定も同じ workspace に置きます。
3. workspace 内で名前が重複する定義を整理します。repository 側の優先順位や fallback はありません。
4. `boid workspace apply -f workspace.yaml` で適用し、`boid project show <ref> --explain` で確認します。

既存 project ID は維持します。新規登録の ID は URL から導出します。`base_branch` は `${TASK_REMOTE_ID}` に対応し、`${current_branch}` は撤去済みです。空の branch は agent の checkout 時に origin default branch を使います。既存 task に保存した branch は変えません。

`project fetch` / `project reload` は設定を取り込みません。workspace 設定は dispatch 時に解決します。[設定リファレンス](../reference/project-yaml.md) と [オンボーディング](onboarding.md) を参照してください。
