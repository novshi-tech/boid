# Repository 設定の移行

daemon は実行時設定として project 設定ファイルを読みません。`boid project init` と `boid project migrate` は撤去済みです。

1. `boid workspace export <slug>` で移行先 workspace の envelope を取得します。
2. behavior、hook、trigger、signals.sources、card_commands、card_events を `spec` に移します。実行環境設定も同じ workspace に置きます。
3. workspace 内で名前が重複する定義を整理します。repository 側の優先順位や fallback はありません。
4. `boid workspace apply -f workspace.yaml` で適用し、`boid project show <ref> --explain` で確認します。

既存 project ID と名前は維持します。schema migration 0062 は旧 `.boid/project.yaml` の name のみを checkout または mirror HEAD から一度だけ読み、DB に保存します。以後の起動・reload・fetch は DB の名前を使います。そのファイルがない登録は mirror の basename を維持します。既存ファイルが読み取れない・不正な YAML の場合は、意図せず名前を変えないよう migration を停止します。ファイルを修正して再起動してください。新規登録は明示した `--name` または URL 由来の名前を保存します。

repository に所属する card にも組み込み `judge` command と card event の既定値が付きます。workspace の定義で上書きできます。

新規登録の ID は URL から導出します。`base_branch` は `${TASK_REMOTE_ID}` に対応し、`${current_branch}` は撤去済みです。空の branch は agent の checkout 時に origin default branch を使います。既存 task に保存した branch は変えません。

`project fetch` / `project reload` は設定を取り込みません。workspace 設定は dispatch 時に解決します。[設定リファレンス](../reference/project-yaml.md) と [オンボーディング](onboarding.md) を参照してください。
