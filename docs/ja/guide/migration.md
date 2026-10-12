# Repository 設定の移行

daemon は実行時設定として project 設定ファイルを読みません。`boid project init` と `boid project migrate` は撤去済みです。

1. `boid workspace export <slug>` で移行先 workspace の envelope を取得します。
2. behavior、hook、trigger、signals.sources、card_commands、card_events を `spec` に移します。実行環境設定も同じ workspace に置きます。
3. workspace 内で名前が重複する定義を整理します。repository 側の優先順位や fallback はありません。
4. `boid workspace apply -f workspace.yaml` で適用し、`boid project show <ref> --explain` で確認します。

既存 project ID は維持します。migration 0062 は旧 `.boid/project.yaml` の名前を checkout または mirror HEAD から一度だけ読み、DB に保存します。ファイルがない・読み取れない・不正な場合は、必要に応じて warning を出して mirror の basename を使います。このフォールバック名は固定され、改名コマンドはありません。旧名を参照する envelope の `projects:` は一致しなくなるため、保存された新名に更新するか、登録を remove して `add --name=<旧名>` してください。再登録の URL 由来 ID は legacy ID と異なる場合があり、remove 前に関連 task を export・保全してください。新規登録は `--name` または URL 由来の名前を保存します。

daemon は mirror を作成・fetch しません。migration 0062 と 0063 は一度だけローカル読み込みを行いますが、通常の起動では mirror を読みません。`project add` は必須の `upstream_url` と空の `work_dir` を DB に保存するだけです。`project fetch` / `project reload` は撤去済みです。既存 mirror と `work_dir` の値は rollback のため維持します。migration 0062 と 0063 完了後、旧バイナリへの rollback が不要になったら `<data-home>/repos/` の mirror を手動削除できます。削除前に DB と mirror をバックアップしてください。job は git gateway 経由で自身の checkout を作成します。

repository に所属する card にも組み込み `judge` command と card event の既定値が付きます。workspace の定義で上書きできます。

新規登録の ID は URL から導出します。`base_branch` は `${TASK_REMOTE_ID}` に対応し、`${current_branch}` は撤去済みです。空の branch は agent の checkout 時に origin default branch を使います。既存 task に保存した branch は変えません。

workspace 設定は dispatch 時に解決します。[設定リファレンス](../reference/project-yaml.md) と [オンボーディング](onboarding.md) を参照してください。

migration 0064 は旧 `extra_repos` を、空の `work_dir` を持つ `perm=fetch` の登録に移します。同 workspace または未所属の既存登録は権限を下げず再利用します。別 workspace 所属の登録は付け替えず、この workspace 用の fetch 登録を作ります。名前衝突は決定的な suffix と warning で解決し、不正なエントリは warning を出してスキップします。更新後に workspace を export し、実際の名前を `spec.projects` に保存してください。envelope の空の旧 `extra_repos: []`（または null）は受理して無視し、非空のリストのみ移行案内付きエラーになります。schema version ceiling により旧バイナリは更新済み DB を開けず、rollback には更新前の DB バックアップが必要です。

migration 0063 は NULL または空の `upstream_url` を、旧 mirror または通常 checkout のローカル `remote.origin.url`（`git config --local`）から復旧します。fetch や forge への接続は行いません。有効な forge URL を HTTPS に正規化して既存行に保存し、project ID・task・workspace 所属を維持します。既存 URL は変更しません。mirror/config がない場合や不正・資格情報付き URL は warning を出してスキップし、起動は妨げません。復旧は extra_repos 登録の migration 0064 より先に実行され、復旧した既存登録を再利用します。この migration は一度だけ実行されるため、完了まで mirror を残し、削除前に warning を確認してください。復旧できなかった行は repository job を実行できません。更新前の DB/mirror バックアップを復元してローカル origin を修正してから再更新するか、関連 task を export・保全してから project を再登録してください。
