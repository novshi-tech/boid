# workspace 単位のデフォルト project 定義

ステータス: **draft (2026-07-29、 fable レビュー 2 巡目まで反映した第 3 版、 判定 GO)**。 未実装。
PR1 は即着手可。
発端: 旧 khi workspace の container based boid への復元。
関連: [volume-only-daemon.md](volume-only-daemon.md) §論点a/b (project の git URL 化・ bare repo)、
[workspace-db-consolidation.md](workspace-db-consolidation.md) (workspace の DB 化)。

レビュー履歴は末尾の「レビュー履歴」節を参照。

---

## 背景

旧 khi workspace を新アーキテクチャ (container based boid) に復元しようとすると、 現行の
`boid project add <git-url>` が **`.boid/project.yaml` を必須にしている**ため登録できない。
khi は顧客の repository であり、 boid 固有のファイルをコミットしていない (今後もコミットできるとは
限らない)。

同時に、 実運用で見えてきた観察がある: **同一 workspace に属する project は、 ワークフロー
(task_behaviors) と branch policy (base_branch / fork_point / default_task_behavior) を共有している
ことが多く、 project 固有の設定が必要ない場合も多い**。 現状はその共通部分を project ごとの
project.yaml に重複して書くしかない。

この 2 つは同じ改修で解ける。 workspace 定義に「デフォルトの project 定義」を持たせ、 project.yaml は
差分だけを書く (あるいは全く書かない) 形にする。

---

## 目的と非目的

### 目的

- workspace に **デフォルト project 定義**を持たせ、 project.yaml が無い repository でも
  `boid project add <git-url>` が成功するようにする。
- project.yaml がある project では、 workspace default を**ベース**に project.yaml が**差分で上書き**
  する形にして、 共通設定の重複記述を無くす。
- 上記を既存 project の挙動を変えずに導入する (project.yaml を持つ既存 project は、 workspace default
  が空である限り現状と完全に同じ結果になる)。

### 非目的

- project.yaml 経路そのものの廃止。 project 固有の設定が必要なケースは残るので、 project.yaml は
  引き続き第一級の入力とする。
- workspace default で **host_commands / env / allowed_domains / capabilities** を扱うこと。 これらは
  既に workspace 側に存在し (`WorkspaceMeta`)、 `GetWithWorkspace` で hydrate 済み。 本 doc の対象は
  「今 workspace 側に置けないフィールド」に限る。
- project 間で workspace を跨いだ設定共有 (テンプレート継承のような機構)。

---

## 現状の実測 (2026-07-29、 main = 16465f9)

以下は本改修の制約条件。 いずれもコードを読んで確認済み。

### 1. project.yaml が読めないと登録全体が失敗する

`ProjectAppService.CreateProjectFromGitURL` (`internal/api/project_service.go:519-533`) は
clone → `s.Meta.LoadBareRepo(bareRepoPath)` の順で進み、 load が失敗すると **clone した bare repo ごと
ロールバック**して 400 を返す。 「半分登録された degraded な行を作らない」という同期バリデーション契約が
コメントで明示されている。

実際の読み取りは `ReadProjectMetaFromBareRepo` (`internal/orchestrator/project_bare_repo.go:61`) で、
`git show HEAD:.boid/project.yaml` の失敗 (ファイル不在 / HEAD 未解決 / repo 破損) を区別せず 1 つの
エラーに畳んでいる。

### 2. project の DB 主キー `id` は project.yaml の `id:` そのもの

`project := &orchestrator.Project{ID: meta.ID, ...}` (`internal/api/project_service.go:535-539`)。
`projects` テーブルの主キーであり、 in-memory の meta cache のキーでもある
(`ProjectStore.metas[meta.ID]`、 `project_store.go:197`)。 **project.yaml が無いと id の供給源が無い**。

### 3. workspace 側が今持てるフィールド

`WorkspaceEnvelopeSpec` (`internal/orchestrator/workspace_envelope.go:80`) と `WorkspaceMeta`
(`internal/orchestrator/workspace_meta.go:27`) が持つのは:
`host_commands` / `env` / `allowed_domains` / `extra_repos` / `container_image` / `capabilities`
(+ envelope 固有の `projects` / `init_script`)。

**共有したい対象である `task_behaviors` / `base_branch` / `fork_point` / `default_task_behavior` は
どちらにも無い**。 これらは `ProjectMeta` (`internal/orchestrator/spec_types.go:438`) 側のフィールド。

DB では `workspaces` テーブル (`internal/db/migrate/testdata/schema.golden:105`) に対応する列が必要
(現行最新 migration は `0032_add_web_devices_token.sql`)。

### 4. workspace hydrate を通る経路と、 通らない経路がある

**この節は第 1 版で誤っていた。 fable レビュー B1 で訂正。**

`ProjectStore.GetWithWorkspace` (`internal/orchestrator/project_store.go:290`) は workspace → project meta
の hydrate 点で、 capabilities / env / host_commands をマージしている。

**hydrate 済み経路** (`GetWithWorkspace` を通る):

| 経路 | 場所 |
|---|---|
| dispatch 本線 | `TaskWorkflowService.ApplyAction`、 `internal/api/workflow_action.go:35` |
| dispatch planner | `internal/orchestrator/planner.go:210-212` (Hydrator 経由、 wiring は `internal/server/wire.go:1118`) |
| `GET /api/projects/{id}` | `internal/api/project.go:155-165`、 wiring は `wire.go:1234` |
| peer advertise | `internal/api/gitgateway_wire.go:203-211`、 wiring は `wire.go:1034` |

**未 hydrate 経路** (素の `Meta.Get` 直読み):

| 経路 | 場所 | 何を読むか | PR2 の扱い |
|---|---|---|---|
| task 作成 | `internal/api/task_create.go:70` | `ResolveBehavior` に渡る | **切替必須** |
| hook replay | `internal/api/workflow_replay.go:19` | `Coordinator.ReplayHook` に渡る | **切替必須** |
| hook 一覧 | `internal/api/workflow_replay.go:96` | `ListHooksForStatus` に渡る | **切替必須** |
| Web UI project 一覧 | `internal/api/web_service.go:92` | task 作成フォームの behavior ドロップダウン (`web/templates/task_form.templ:32, 50` が `p.Meta.TaskBehaviors` を読む) | **切替必須** |
| Web UI project 単体 | `internal/api/web_service.go:222` | 同上 | **切替必須** |
| job 表示名の補完 | `internal/api/workflow.go:78-82` | `TaskBehaviors[behavior]` | 任意 (planner が `DisplayName` を焼き込むので空のときだけ発火する cosmetic fallback、 `planner.go:108`) |
| `GET /api/projects` (list) | `internal/api/project_service.go:254` (`hydrateProject`) | `boid check` の hook requires 走査 (`cmd/check.go:89`) と `project add` 直後の requires 警告 (`cmd/project.go:283`) | 任意 (warning が出ないだけ) |

第 1 版は `web_service.go` の 2 経路を「表示系と思われる」として PR2 のスコープ外に置いていたが、
これは誤り (fable レビュー 2 巡目 M1)。 **Web UI の task 作成フォームの behavior ドロップダウンは
`p.Meta.TaskBehaviors` から生成される**ため、 切り替えないと no-yaml project ではドロップダウンが
空になる。 CLI からは task を作れるのに Web UI からは作れない、 という B1 と同形の穴になる。

そして `ResolveBehavior` (`internal/orchestrator/behavior_resolve.go`) が読むのは
`meta.TaskBehaviors` (:62, :109, :145) / `meta.DefaultTaskBehavior` (:107) / `meta.BaseBranch`
(:130, :159) — **本改修で workspace 側に置くと決めた 4 フィールドのうち 3 つが、 まさにこの
hydrate を通らない経路で消費される**。 behavior が見つからなければ task 作成は 400
(`behavior_resolve.go:116, 147`)。

つまり `GetWithWorkspace` にマージを足すだけでは、 project.yaml 無し project は **task を 1 つも
作れない**。 §PR 分割の PR2 でこの経路差を先に潰す。

裏面として、 **4 フィールド目の `ForkPoint` は既に hydrate 済み経路でカバーされている**。 唯一の本番
読者は `BuildCloneDeclaration(task, meta.ForkPoint)` (`internal/orchestrator/planner.go:124` →
`head_branch.go:30`) で、 planner の `loadContext` は Hydrator 経由 (`planner.go:210-212`)。
つまり ForkPoint は PR4 のマージだけで効き始め、 PR2 を必要としない。

### 5. daemon 再起動 / fetch でも project.yaml は再読み込みされる

`ProjectStore.LoadAll` (`project_store.go:493-536`) は起動と reload のたびに bare repo project を
`LoadBareRepoExpectingID` → `ReadProjectMetaFromBareRepo` で再ロードし、 **失敗すると
`s.Remove(candidate.ID)` で cache を落として degraded にする** (`project_store.go:522-528`)。
`FetchProject` も同様 (`project_service.go:699-704`)。

登録経路だけを直しても、 project.yaml 無し project は**初回再起動で degraded になって消える**。

---

## 決定事項 (nose 判断 2026-07-29)

### 決定1: フィールド単位マージ (workspace default をベース、 project.yaml が上書き)

project.yaml に書かれたフィールドだけが workspace default を上書きする。 「task_behaviors だけ project
固有、 branch policy は workspace 共有」のような部分上書きができる。

「書かれた」の判定基準は §決定5 を参照 (project.yaml 側に field-presence 追跡が無いため、
string フィールドでは「キー不在」と「明示的な空文字」が区別できない)。

### 決定2: 毎回動的に合成する

hydrate 時に都度マージする。 add 時のスナップショットにはしない。 workspace default を変更すれば、
その workspace の全 project に即反映される (= 共有したいという意図そのまま)。

トレードオフ 3 点:
- workspace を触ると全 project の挙動が動くため、 影響範囲が見えにくい (§論点e)。
- `BaseBranch` は task 作成時に task 行へ snapshot される (`task_create.go:199-215`) ので、
  **既存 task には遡って効かない**。 hook / task_behaviors は dispatch 時評価なので効く。
- 合成点が `GetWithWorkspace` 1 箇所ではない (§現状の実測4)。

### 決定3: project.yaml が無い project の id は「正規化した repo slug の sha256」

**生の URL を id にすることはできない**: project id は `/api/projects/{id}/fetch`
(`internal/api/project.go:124`)、 `/api/projects/{id}/sessions` (`internal/api/web.go:114`) のように
HTTP パスへ直接埋め込まれる。 `https://github.com/foo/bar.git` は `/` と `:` でルーティングが割れる。
URL エンコードすれば通るが `boid project fetch https%3A%2F%2F...` は実用に耐えない。

**ハッシュ入力は `NormalizeOriginURL` の出力ではなく repo slug (`host/owner/repo`、 `.git` strip 済み)
とする。** 第 1 版は「`NormalizeOriginURL` を通してからハッシュ」としていたが、 これは誤り
(fable レビュー M1):

- `NormalizeOriginURL` (`internal/dispatcher/upstream_url.go:41-42`) は **`https://` で始まる URL を
  無変換で素通しする**。 `.git` の有無はそのまま残る。
- 一方 scp-like / ssh:// / http:// / git:// は `repoSlugFromOriginURL` を通って
  `https://<slug>.git` に正規化される (`upstream_url.go:55-59`)。
- 結果、 `https://host/o/r` と `https://host/o/r.git` は別文字列のまま。 これをハッシュ入力にすると
  **同一 repo が別 id で 2 回登録できてしまう**。 同一 workspace なら bareRepoPath 衝突の 409
  (`project_service.go:515-517`) に救われるが、 `--name` を変えるか workspace が違えばすり抜ける
  (bareRepoPath は per-workspace、 `project_bare_repo.go:134-136`)。 現行の project.yaml `id:` ベースは
  id の PK 衝突で 2 回目を拒否するので、 **素朴な実装は dedup が現行より弱くなる**。

したがってハッシュ入力は `repoSlugFromOriginURL` 相当 (gitgateway の `NewRepoKey`、
`internal/gitgateway/route.go:45-48` と同じ正規化) の出力とする。 `file://` は slug 化できない
(`upstream_url.go:52-53` で素通し) ので別枝が要る。 host の大文字小文字はどこでも正規化されていない
— 一貫して未正規化なので実害は小さいが、 ハッシュ入力の定義として明記しておく。

id に workspace slug は混ぜない。 「同じ repo は 1 workspace にしか登録できない」制約が付くが、 これは
project.yaml の `id:` ベースの現行と同じ性質なので挙動の変化にならない。

### 決定4: task_behaviors は behavior 名キー単位でマージ

workspace default に `dev` / `supervisor` を置き、 project.yaml で `release` だけ追加 / `dev` だけ上書き、
ができる。 **同名の behavior は project.yaml 側が丸ごと勝つ** (behavior の中の hooks まで潜って
マージはしない)。

マージは **canonical 名前空間で行う** (§論点j)。 alias / mirror を素通しで名前キーマージすると
「同名は project 側が勝つ」が破れる。

### 決定5: 空値 = 未指定 = 継承 (打ち消しは表現しない)

決定1 の「書かれたフィールドだけが上書き」の判定基準。 fable レビュー M3 で曖昧さが指摘され、
nose 判断で確定 (2026-07-29)。

前提: project.yaml は素の `yaml.Unmarshal` で `ProjectMeta` に直接 decode される
(`internal/orchestrator/spec_loader.go:92-95`)。 envelope 側の `FieldsPresent`
(`workspace_envelope.go:151-166`) に相当する presence 追跡が**無い**ため、 plain string である
`base_branch` / `fork_point` / `default_task_behavior` (`spec_types.go:446, 455, 469`) は decode 後に
「キー不在」と「明示的な空文字」を区別できない。

**決定: 空値を「未指定」とみなして workspace default を継承する。 presence 追跡は足さない。**

代償: 「workspace default の `base_branch` を project.yaml 側で打ち消して未設定
(= `${current_branch}` fallback、 `task_create.go:117-152`) に戻す」ことが表現できなくなる。
打ち消しの需要が具体的に見えていないため、 この制約を受け入れる。 **doc / リファレンスに明記すること。**

なお `task_behaviors` は map なので nil (キー不在) と空 map (明示 `{}`) は区別できる
(`normalizeBehaviorAliases` / `addAliasMirrors` は `len == 0` で入力をそのまま返すため nil 性は parse を
生き延びる、 `spec_loader.go:333-334, 372-373`)。 ただし決定4 の名前キーマージでは「project.yaml に
無い behavior 名は workspace default から来る」だけなので、 この区別は実質使わない。

後から presence 追跡へ移行すると意味論が変わる非互換になるため、 この決定は着手前に固定する。

---

## 未決の論点

### 論点a: workspace default に置けるフィールドの範囲

決定4 の `task_behaviors` と、 branch policy 3 点 (`base_branch` / `fork_point` /
`default_task_behavior`) は確定。 残りの `ProjectMeta` フィールドをどうするか:

| フィールド | 案 | 理由 |
|---|---|---|
| `id` | **置かない** | project ごとに一意でなければならない。 決定3 で導出する |
| `name` | **置かない** | 論点b で扱う |
| `host_commands` | 置かない | 既に `WorkspaceMeta.HostCommands` がある。 二重の供給源を作らない |
| `env` | 置かない | 同上 (`WorkspaceMeta.Env`) |
| `additional_bindings` | 置かない | Phase 4 PR4 で workspace 側は退役済み。 復活させない |

つまり workspace default が持つのは実質 4 フィールド。 スキーマを `ProjectMeta` のサブセットとして
定義するか、 独立した型にするかは実装時の判断。

### 論点b: project.yaml が無い project の `name` をどう決めるか

`ProjectMeta.Name` は `boid workspace apply` の `spec.projects[].name` マッチングと、 表示・ref 解決に
使われる。 project.yaml が無いと供給源が無い。

案: `DeriveProjectNameFromURL` (`internal/orchestrator/project_bare_repo.go:260`) の結果を使う
(bare repo のディレクトリ名と一致するので分かりやすい)。 `--name` 明示時はそちら優先。

この案を採る場合、 add 時に 2 つ保証が要る:

1. **derive した name が空にならないこと**。 `refuseIfAnyRegisteredProjectNameUnavailable`
   (`project_service.go:1443-1489`) が「name の無い project があると apply 全体を拒否する」ため。
2. **name の衝突検査** (fable レビュー m2)。 apply の ambiguity 検査は
   `resolveProjectByNameExact` (`project_service.go:1523`) の結果を **全登録 project 横断**で見ており
   (`snapshotRegisteredProjects` = `ListProjects()` 全件、 `project_service.go:1424-1441`)、 同名 2 件で
   **apply 全体が 409** になる (`project_service.go:1406-1418`)。 `parseProjectMetaBytes` は name の
   一意性を強制していない。 repo basename 由来の name (`api`, `web` 等) は顧客 repo 間で衝突しやすく、
   別 workspace に同 basename の repo を add しただけで**両方の workspace の export/apply 往復が
   壊れる**。 add 時に少なくとも警告、 できれば `--name` を要求する。

### 論点c: 「project.yaml が無い」と「project.yaml が壊れている」の区別

現状の `gitShowHEAD` は両者を同じエラーに畳んでいる (`project_bare_repo.go:88` の doc コメントに
「callers do not need a finer-grained classification here」と明記)。 本改修ではこの前提が崩れる:

- **無い** → workspace default にフォールバックして登録成功させたい
- **壊れている (YAML 構文エラー / バリデーション違反)** → 従来通り登録を失敗させたい (silent に
  default へフォールバックすると、 typo が「デフォルト設定で動く」形で隠れる)
- **HEAD が解決できない (commit が 1 つも無い空 repo)** → repo 自体の異常なので従来通り失敗させる

実装方針 (fable レビュー m5): **stderr 文字列で判別しない**。 git のエラーメッセージは gettext で
ローカライズされ得るため locale / git version 依存になる。 代わりに 2 段で判別する:

1. `git rev-parse --verify HEAD` — HEAD 未解決 (空 repo) の判別
2. `git ls-tree HEAD -- .boid/project.yaml` — 出力が空なら path 不在 (exit 0)

exit code と stdout だけで判別でき、 locale 非依存。

### 論点d: workspace default も project.yaml も無い場合

両方無いと `task_behaviors` が空になり、 タスクを作っても発火する hook が無い。 案:

1. add の時点で拒否する (「この workspace には default project 定義が無いので、 project.yaml の無い
   repository は登録できない」)。 失敗が早く、 メッセージで次の一手を示せる。
2. 登録は通して degraded 扱いにする。

案1 を推す。 §現状の実測1 の同期バリデーション契約と一貫する。

**判定条件は「default 定義が存在するか」では狭い** (fable レビュー 2 巡目 m2)。 behavior 未指定の
task 作成は `meta.DefaultTaskBehavior` → 無ければ `supervisor` という名の behavior の存在 → どちらも
無ければ 400、 という順で解決される (`behavior_resolve.go:104-117`)。 workspace default が
`task_behaviors` を持っていても `default_task_behavior` 未設定かつ `supervisor` が無ければ、 add は
通ったのに behavior 未指定の task 作成が 400 になる。 add 時の検査は **合成結果で default behavior の
解決が成立するか**まで見るのが一貫する (behavior を明示指定する task には影響しない)。

### 論点e: workspace default が効いていることの可視化

決定2 (動的合成) の副作用として、 `boid project show <id>` が返す meta が「project.yaml に書いてある
内容」と一致しなくなる。 どのフィールドがどこ由来かを出せないと、 デバッグで確実に混乱する。

案: `boid project show` に `--explain` 相当を足して、 フィールドごとに `project.yaml` /
`workspace default` / `(unset)` を表示する。 最低限、 workspace default 由来のフィールドがあることを
1 行で示す。

併せて扱うべき副作用 (fable レビュー m4):

- dispatch 経路の `GetWithWorkspace` は**失敗時に黙って素の `Meta.Get` へ fallback する**
  (`workflow_action.go:31-43`)。 既存 project ではこの劣化は「kit env が乗らない」程度で済んでいたが、
  project.yaml 無し project では **task_behaviors が丸ごと消えた meta で dispatch が進む**ため、
  原因の見えないエラーになる。 fallback 時の警告を強めるか、 no-yaml project では fallback を
  禁止するかを決める必要がある。
- `BaseBranch` の snapshot 性 (決定2 のトレードオフ参照) も `--explain` の表示対象にする。

### 論点f: export/apply の往復と version skew

`WorkspaceEnvelopeSpec` に新フィールドを足すと、 `boid workspace export` / `apply` の round trip に
乗る。 既存の「missing = 触らない / present-but-empty = クリア」のセマンティクス
(`WorkspaceEnvelopeApply.FieldsPresent`) に従わせる必要がある。 `omitempty` を付けないという既存の
規約 (`workspace_envelope.go:62-79`) もそのまま適用される。

`workspaceEnvelopeSpecFields` の allow-list への追加を忘れると `unknown field` で弾かれる
(`workspace_envelope.go:490-493`)。

**前方非互換に注意** (fable レビュー m3): `omitempty` を付けない規約のため、 新フィールドを足した
瞬間に**全 workspace の export 出力 shape が変わる**。 その export を旧バイナリで apply すると
`unknown field` で拒否される。 backup の互換性として doc / リリースノートに明記する。

### 論点g: sha256 id の形式

- 長さ: hex 64 文字は CLI で扱いづらい。 既存 id は UUID (36 文字) や `demo` のような短い文字列。
- 既存 id と区別できる prefix を付けるか (例: `url-` + 先頭 16 文字)。 付けると「この project は
  project.yaml を持たない」が id から読めるが、 後で project.yaml を足したときに id が変わる問題が
  出る (→ 論点h)。
- 短縮する場合の衝突耐性: 先頭 16 hex 文字 = 64 bit。 個人環境の project 数なら十分。

### 論点h: 後から project.yaml をコミットしたときの id

URL 由来 id で登録した project に、 後日 `.boid/project.yaml` (`id: khi` 等) がコミットされたら
どうなるか。 id-drift を拒否する経路は **2 箇所**ある (fable レビュー M2 後段):

- `FetchProject` (`internal/api/project_service.go:712-724`) — 再登録を要求して拒否
- `LoadAll` (`internal/orchestrator/project_store.go:520-522`) — **daemon 再起動のたびに degraded**

候補:
1. **project.yaml の `id:` を optional にする**。 存在すれば従来通り、 無ければ URL 由来。 一度 URL 由来で
   登録した project は、 後から `id:` が現れても id を変えない (既存タスクとの紐付けが切れるため)。
2. 従来通り `id:` 必須のままにして、 project.yaml が無い場合だけ URL 由来にする。

案1 のほうが「共通設定は workspace、 project.yaml は差分だけ」という本改修の思想と一貫する
(project.yaml に書くのが `task_behaviors` の 1 個だけ、 というケースで `id:` を書かせる必然性が無い)。
ただし `ParseProjectMeta` のバリデーションで `id` 必須を外す影響範囲の確認が要る。 **どちらを採るにせよ
上記 2 経路の両方を対象にすること。**

### 論点j: workspace default 側 task_behaviors のバリデーションと alias 正規化

project.yaml 経路の task_behaviors は load 時に以下を通る:

- `validateHookKind` (`spec_loader.go:110-116`)
- `normalizeBehaviorAliases` (alias 重複検出を含む、 `spec_loader.go:137-141, 332-359`)
- `addAliasMirrors` (`spec_loader.go:148`) — cache 済み meta には **alias mirror エントリが含まれた
  状態**で入る

既存の `GetWithWorkspace` のマージは mirror の二重処理を避けるため `stripAliasMirrors` → merge →
`addAliasMirrors` を毎回やっている (`project_store.go:414-419, 431-436`)。

workspace default 側 (envelope decode / DB 由来) が同じ正規化を通らないと、 決定4 が破れる具体例
(fable レビュー M4):

- workspace default に legacy 名 `dev`、 project.yaml に canonical `executor` を書いた場合、 素朴な
  名前キーマージは両者を**別名**として共存させる。 `dev` で lookup する legacy task 行は project
  override ではなく workspace 版に当たる (`LookupBehaviorWithAlias` は要求名の exact match を最優先、
  `behavior_resolve.go:62`)。
- mirror を strip せずマージすると、 project 側の mirror エントリが workspace default の canonical
  エントリを「project 定義」として潰す。

決定: **workspace default の入口 (envelope decode / DB save) で project.yaml と同一のバリデーション +
正規化を掛け、 マージは canonical 名前空間で行う。** これが無いと malformed な hook が
dispatch planner まで素通りする (project.yaml 経路の load-time 検証は defense-in-depth の片翼、
`spec_loader.go:100-109` のコメント参照)。

---

## PR 分割案 (draft)

各 PR は独立して main に入れられる粒度を目指す。

- **PR1**: `gitShowHEAD` の失敗分類 (論点c)。 「path 不在」「HEAD 未解決」「その他」を型で区別する。
  この時点では呼び出し側の挙動は変えない (全部これまで通り失敗させる) — 純粋な足場。
  どの論点からも独立しているので、 本 doc の残りの改訂と並行して着手できる。
- **PR2**: **hydrate 経路の統一** (§現状の実測4)。 切替必須は 5 経路 — `task_create.go:70` /
  `workflow_replay.go:19, 96` / `web_service.go:92, 222`。 `workflow.go:78` と
  `project_service.go:254` は任意 (同表参照)。

  この切り替えは挙動不変にできるはず。 根拠: 現行の hydration は `TaskBehaviors` map 自体には触る
  (mirror の strip/add と `behavior.Env` / `HostCommands` の書き換え、 `project_store.go:411-437`) が、
  **`ResolveBehavior` が読む `Traits` / `DefaultInstruction` / `Readonly` / 3 つの string フィールドは
  無傷**。

  ただし不変性テストは正常系だけでなく **3 つの失敗系**を固定する必要がある (fable レビュー 2 巡目 M2)
  — bare `Get` と `GetWithWorkspace` は失敗の様式が違う:

  | 失敗系 | 現行 (bare `Get`) | `GetWithWorkspace` |
  |---|---|---|
  | meta 未ロード | `CreateTask` は**許容**して nil meta で続行 → hardcoded supervisor に落ちる (`task_create.go:69-73` → `behavior_resolve.go:104-106`) | error (`project_store.go:296-297`) |
  | workspace load 失敗 (ErrNotExist 以外) | 起きない | error (`project_store.go:324`) |
  | host_commands 衝突 / reject rule 違反 | 起きない | error (`project_store.go:404-409`) |

  特に `CreateTask` の「meta 不在 → nil meta として続行」は、 `GetWithWorkspace` の error を nil に
  写像しないと再現できない。 hook replay / 一覧は現状も meta 不在で 500 なので方向は揃うが、
  workspace 側の破損だけで「今まで見えていた hook 一覧が 500 になる」変化が入り得る。
  既存の hydrate 消費者どうしでも方針は割れている (`workflow_action.go:35-43` は silent fallback、
  `planner.go:210-215` は fail) ので、 経路ごとに現行の挙動を写すのが正解。
- **PR3**: `WorkspaceMeta` / `workspaces` テーブル (migration `0033`) / `WorkspaceEnvelopeSpec` に
  default project 定義フィールドを追加 (論点a, f, j)。 入口のバリデーション + 正規化もここ。
  DB/hydrate 面ではまだ誰も読まないが、 **export の出力 shape はこの時点で変わる** (論点f)。

  新フィールドを手で通す箇所が **約 11 箇所**ある (fable レビュー 2 巡目 m1): repository 側 6
  (`workspace_repository.go` の Load :35 / LoadWithRevision :84 / UPDATE list :176 /
  `decodeWorkspaceMetaColumns` :218 / INSERT list ×2 :278, :314)、 envelope 側 5 (struct field /
  allow-list :132-142 / per-field decode block / `MergeInto` :217-241 /
  `NewWorkspaceEnvelopeFromMeta` :187-203)。 **reflection で追加を強制する drift test は
  `bareMetaKnownFieldNames` の 1 つだけ** (`workspace_envelope_test.go:463-476`)。

  PR3 は「まだ誰も読まない」ため、 column 1 箇所の漏れ (例: INSERT にあるが SELECT に無い) は
  **無症状のまま PR4 で「default を設定したのに効かない」として発症する**。 Save → Load → DeepEqual の
  round-trip テストを新フィールド込みで必ず置く。

  併せて: `expandWorkspaceRuntimeForDispatch` (`workspace_meta.go:142-150`) は shallow copy で Env しか
  複製しない。 新フィールドが reference 型 (behavior map) だと clone 間で共有されるため、 PR4 の
  マージは既存パターンどおり **新しい map を割り当てて書き戻す** (in-place 変異は禁止)。
- **PR4**: hydrate にマージを実装 (決定1, 2, 4, 5、 論点j)。 この時点で「project.yaml を持つ project が
  workspace default を継承する」が動く。 既存 project は workspace default が空なので挙動不変 —
  その不変性をテストで固定する。
- **PR5**: URL 由来 id の導出 (決定3, 論点g) と、 project.yaml 不在時の経路 (論点b, d)。
  **登録経路だけでなく `LoadAll` / `FetchProject` / reload の fallback と、 合成 meta の ID / Name
  供給規則を含める** (§現状の実測5)。 ここで初めて project.yaml 無しの `project add` が通り、
  再起動を跨いでも生き残る。
- **PR6**: `project show --explain` 相当 (論点e)。 dispatch fallback 時の警告強化もここ。
- **PR7**: project.yaml の `id:` optional 化 (論点h)。 案1 を採る場合のみ。 `FetchProject` と `LoadAll`
  の両経路を対象にする。

PR4 と PR5 の間で一度 dogfood を挟むのが安全 (PR4 までは既存 project の挙動不変が保証なので、
退行が出たらそこで気付ける)。

PR3 ↔ PR4 の間で daemon が再起動しても無害 (PR3 は `LoadAll` / `GetWithWorkspace` に触れないので、
migration `0033` が走って新 column は誰にも読まれず座っているだけ。 旧バイナリへ rollback しても
DB 読み出しは明示 column list なので新列は無視される)。 **ただし PR3 の時点で default を「設定できて
しまう」**点に注意 — PR4 が land した瞬間にその workspace の挙動が動く。 運用手順として
**PR4 まで workspace default を設定しない**こと。

---

## 検証

- 既存 project の挙動不変: workspace default が空のとき、 hydrate の出力が改修前と一致すること
  (PR2 と PR4 のテストで固定)。
- project.yaml 無し project が **daemon 再起動を跨いで生き残る**こと (§現状の実測5)。
- 旧 khi workspace の実機復元 (host 側 dogfood)。 project.yaml 無しで add → task 作成 → タスク実行まで。
  task 作成が通ることを明示的に確認する (§現状の実測4 の経路差)。
- `workspace export` → `apply` の round trip で default project 定義が失われないこと (論点f)。

---

## レビュー履歴

### 1 巡目: fable (2026-07-29)

判定 **NO-GO** (doc 改訂で解消可能、 方向性を覆す指摘は無し)。 指摘 Blocker 1 / Major 4 / Minor 5。
本第 2 版で全件を反映済み:

| 指摘 | 内容 | 反映先 |
|---|---|---|
| B1 | 実測4 の一般化が誤り。 task 作成 / hook replay は `GetWithWorkspace` を通らず、 共有対象 4 フィールドのうち 3 つがその経路で消費される | §現状の実測4 を全面訂正、 PR2 を新設 |
| M1 | `NormalizeOriginURL` は `https://` を素通しするので `.git` 有無を同一化しない。 素朴な実装は dedup が現行より弱くなる | 決定3 のハッシュ入力を repo slug に変更 |
| M2 | PR4 の影響範囲抜け。 `LoadAll` / `FetchProject` の再ロードにも fallback が要る (無いと初回再起動で degraded) | §現状の実測5 を新設、 PR5 のスコープに追加、 論点h に `LoadAll` 経路を追記 |
| M3 | project.yaml 側に field-presence 追跡が無く、 決定1 の「書かれたフィールド」が string 3 つで曖昧 | 決定5 として確定 (nose 判断 2026-07-29、 「空値 = 継承」を採用) |
| M4 | workspace default 側 task_behaviors の検証 / alias 正規化が未定義。 決定4 が破れる具体例あり | 論点j を新設、 決定4 に補足 |
| m1 | apply の name matching は `ResolveProjectRef` ではなく `resolveProjectByNameExact` | 論点b の記述を訂正 |
| m2 | URL 由来 name は衝突しやすく、 衝突すると apply が全 workspace で 409 | 論点b に衝突検査を追加 |
| m3 | PR2 (現 PR3) の「まだ誰も読まない」は export shape 面では不正確 (前方非互換) | 論点f に version skew を追記、 PR3 の記述を訂正 |
| m4 | 決定2 の副作用 2 件 (dispatch の silent fallback / BaseBranch の snapshot 性) が未記載 | 決定2 のトレードオフと論点e に追記 |
| m5 | 論点c の実装は stderr 文字列で判別すると locale 依存になる | 論点c に `rev-parse` + `ls-tree` の 2 段方式を明記 |

指摘のうち B1 / M1 / M2 および m1 の file:line は、 レビュー後に本 doc の作成者が実コードで再確認済み。

### 2 巡目: fable (2026-07-29)

判定 **GO** (PR1 は即着手可)。 1 巡目 10 件の反映はすべて正確と確認された上で、 棚卸しの続きとして
Major 2 / Minor 3。 Blocker は無し。 本第 3 版で全件を反映済み:

| 指摘 | 内容 | 反映先 |
|---|---|---|
| M1 | 実測4 の棚卸しが未了。 bare `Meta.Get` で `TaskBehaviors` を読む経路が 2 つ欠落しており、 さらに `web_service.go:92, 222` は「表示系」ではなく **Web UI の task 作成フォームの behavior ドロップダウンの供給源** | §現状の実測4 を hydrate 済み / 未 hydrate の 2 表に再構成。 `web_service` の 2 経路を PR2 の切替必須に昇格。 `ForkPoint` が既に hydrate 済みである旨も追記 |
| M2 | PR2 の「挙動不変」は、 call site ごとに異なる失敗系セマンティクスを移植しないと成立しない (bare `Get` と `GetWithWorkspace` で失敗様式が違う) | PR2 に失敗系 3 つの表と、 経路ごとに現行挙動を写す方針を追加 |
| m1 | 新フィールドのスレッディングは手作業 約 11 箇所に対し drift test は 1 つだけ。 `expandWorkspaceRuntimeForDispatch` の shallow copy 注意 | PR3 に round-trip テストの必須化と in-place 変異禁止を追記 |
| m2 | 論点d の判定条件が狭い (「default 定義の有無」ではなく「default behavior の解決が成立するか」まで見るべき) | 論点d に追記 |
| m3 | 実測4 の「dispatch 本線だけ」は hydrate 済み経路の under-count | hydrate 済み 4 経路の表を追加 |

2 巡目で確認が取れ、 指摘にならなかった事項 (doc の前提として有効):

- **git gateway は特別扱い不要**: repo key は `proj.UpstreamURL` (DB 列、 add 時に正規化 URL を格納) から
  導出され (`internal/api/gitgateway_wire.go:237-247`)、 project.yaml を一切参照しない。 no-yaml project も
  同じ経路で grant される。 なお `file://` upstream は `repoSlugFromOriginURL` が error になるため gateway
  grant を得られないが、 これは yaml 有り project でも同じ既存性質で、 本改修で新たに壊れるものではない。
- **secret namespace も特別扱い不要**: `GetWithWorkspace` が workspaceID を無条件注入するため
  (`project_store.go:309`)、 meta の出自に依存しない。
- **決定5 で既存 project の挙動不変は成立する**: workspace default が空なら継承元も空なので、 4 フィールドとも
  decode 結果がそのまま残る。
- **PR 順序は成立する**: PR3 ↔ PR4 間の daemon 再起動は無害、 PR5 が PR1 と PR4 の両方に依存する順序も正しい。

M1 (web_service → `task_form.templ:32, 50` の behavior ドロップダウン経路) と M2 (`CreateTask` の
meta 不在許容 `behavior_resolve.go:104-106` / `GetWithWorkspace` の error)、 および `ForkPoint` の
hydrate 済み経路 (`planner.go:210-212` + `wire.go:1118`) は、 反映前に本 doc の作成者が実コードで
再確認済み。
