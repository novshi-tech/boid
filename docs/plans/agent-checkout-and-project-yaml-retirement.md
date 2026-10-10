# agent 主導の checkout と project.yaml の退役

2026-10-06。nose との議論で方向を決めたもの (経緯はこの doc に書かない)。

## 1. ゴール

1. **daemon を forge から切り離す。** daemon は clone も fetch もしないし、mirror も持たない。forge に触れるのは、job の git を中継する git gateway だけ
2. **設定の単位を workspace にする。** behaviors / triggers / signals / card_commands / スキルを workspace が持つ。リポジトリは boid 固有のファイルを持たない、ただのコード置き場になる。メタプロジェクトと default metaproject の特別扱いが消える
3. **今ある境界を保つ。** push できるのは task の主リポジトリだけ、かつ書き込み可の task だけ
4. **clone の転送量を今と同程度に保つ**

この doc が扱わないもの: API gateway (`internal/apigateway`) は変えない。api-gateway-mcp への外部化は別の doc で扱う。

## 2. ゴールの分解

ゴールを成り立たせるために、次ができないといけない。

| # | できないといけないこと | ゴール |
|---|---|---|
| a | job が daemon の助けなしに、主リポジトリの作業ツリーを作れる | 1 |
| b | 2 回目以降の clone で、差分しか転送しない | 4 |
| c | job が workspace に登録されたほかのリポジトリを読める | 2 |
| d | job が push できるのは主リポジトリだけ | 3 |
| e | job が持つ git の権限を、job の中の agent が広げられない | 3 |
| f | behaviors / triggers / signals / card_commands を workspace だけから解決できる | 2 |
| g | 主リポジトリを持たない task が普通に作れる | 2 |
| h | 「workspace に登録されたリポジトリ」と、その権限の一覧がある | 2, 3 |
| i | 既存の project / メタプロジェクト / task が、データを失わずに移れる | 全部 |

## 3. 部品と呼び出し関係

| 部品 | 持ち主 | 満たすもの |
|---|---|---|
| C1 リポジトリ登録 | boid DB | h |
| C2 workspace 設定 | boid DB | f |
| C3 task の所属 | boid DB | g |
| C4 `boid checkout` | sandbox 内で動く boid バイナリ | a, c |
| C5 参照キャッシュ | workspace ごとの volume | b |
| C6 git gateway (縮小) | boid daemon | d, e |

```
[dispatch 時]
  daemon ─ C6 に job トークンを登録 (リポジトリごとの権限)
     │      └─ 読む: C1 (権限) / C3 (主リポジトリ) / job の readonly
     └──(env: BOID_PRIMARY_REPO / BOID_BASE_BRANCH / BOID_FORK_POINT / BOID_GIT_BASE / BOID_GIT_CACHE)──▶ job

[job の中]
  sandbox 内の起動ラッパー ──exec──▶ C4 boid checkout
     C4 ──flock + git fetch──▶ C5
     C4 ──git clone --reference C5 --dissociate──▶ C6 ──▶ forge
  agent ──git switch (base_branch)──▶ 作業ツリー

[設定の解決]
  trigger loop / card command / behavior 解決 ──読む──▶ C2
```

呼び出しの方法:

- daemon → C6 は、今と同じプロセス内の登録 (`GitGateway.Register`)
- job → C4 は sandbox 内でのプロセス実行。broker を経由しない
- C4 → C6 は普通の git smart HTTP。URL のパスに job トークンが入る (今と同じ)

依存は job → C6 → forge の一方向だけで、循環しない。

**部品をまたぐ契約は 2 つだけ。**

1. **job トークンの権限** (dispatcher が C6 に登録して、C6 が判定する)。§4.6 で定義する
2. **C4 の入力** (dispatcher が env に書いて、C4 が読む)。§4.4 で定義する

## 4. 部品ごとの設計

### 4.1 C1 リポジトリ登録

- `projects` の 1 行が「workspace に登録されたリポジトリ」1 つを表す。テーブル名と CLI の名前 (`project`) は変えない
- `upstream_url` を必須にする。ホストのディレクトリで登録する経路はなくす
- `work_dir` はもう使わない。default metaproject と同じく `''` を入れる
- 権限の列 `perm` を足す。値は `fetch` か `push`
  - `push` のリポジトリだけが task の主リポジトリになれる
  - `fetch` のリポジトリは読むだけ。`WorkspaceMeta.ExtraRepos` の各エントリは `perm=fetch` の行に移して、`extra_repos` フィールドは消す
- 登録しても clone はしない。`CloneBareRepo` / `FetchBareRepo` / `boid project fetch` / `boid project reload` はなくなる

### 4.2 C2 workspace 設定

- workspace はもう `task_behaviors` / `base_branch` / `fork_point` / `default_task_behavior` を持っている (`workspace_meta.go:77-94`)。ここに `triggers` / `signals` / `card_commands` / `card_events` を足す。保存先は `task_behaviors` と同じく、`workspaces` テーブルの YAML テキスト列にする
- envelope (`boid workspace apply`) の `spec` にも同じフィールドを足す。git で管理したいなら、envelope の YAML をリポジトリに置いて apply すればいい
- project.yaml との合成 (`ProjectStore.GetWithWorkspace` の `project_store.go:446-525`) は消す。behavior は workspace だけから解決する
- trigger loop は project ではなく workspace を回す。`trigger_runs` のキーは `workspace_id` にする
- card command は card の workspace から引く。`IsCardProject` は「workspace に `card_events` があるか」に置き換える
- `signals.sources` から trigger を導出する処理 (`deriveSignalTriggers`) は、workspace の読み込みで呼ぶ
- スキルは workspace HOME の `~/.claude/skills` と `~/.agents/skills` に置く (どこから入れるかは §6 の D1)

### 4.3 C3 task の所属

- `tasks` に `workspace_id NOT NULL` を足す。既存の行は `project_workspaces` から埋める
- task を作るときに workspace を指定しなかった場合は、default workspace (slug は `default`。daemon の起動時に必ず作られる既存の workspace で、コード上は `orchestrator.DefaultWorkspaceSlug`) に紐づく。`workspace_id` が空の task は作れない
- `tasks.project_id` は nullable にして、意味を「主リポジトリ」とする
  - 空: workspace 直属の task。どのリポジトリにも push できない
  - 空でない: その行の `perm` が `push` でないといけない
- `jobs.project_id` も nullable にして、`workspace_id` を足す
- 子 task は親の主リポジトリを継承しない。作るときに指定する。ほかのリポジトリに書きたいなら、そのリポジトリを主リポジトリにした子 task を作る
- `base_branch` の `${current_branch}` 展開は、daemon が mirror の HEAD を読むので消す。`base_branch` が空なら、agent が clone した時点の origin の default branch を使う
- default metaproject は消す。特別扱いしている場所は付録 A.4 に全部挙げてある

**片道ドア:** `project_id` が NULL の行を 1 行でも書いたら、旧バイナリはそれを読めない。ロールバックの手順は §5 に書く。

### 4.4 C4 `boid checkout`

sandbox の中で動く boid のサブコマンド。broker には送らない。キャッシュを使ってリポジトリを clone するだけで、branch には触らない。

```
boid checkout            # 主リポジトリを clone する
boid checkout <name>     # 登録済みのほかのリポジトリを clone する
```

入力 (dispatcher が env に書く):

| env | 意味 |
|---|---|
| `BOID_PRIMARY_REPO` | 主リポジトリ (`<host>/<owner>/<repo>`)。workspace 直属の task なら空 |
| `BOID_GIT_BASE` | clone URL の前置部分 (`<gw>/j/<job-token>`)。clone URL は `<BOID_GIT_BASE>/<host>/<owner>/<repo>.git` |
| `BOID_GIT_CACHE` | C5 のマウント先 |

`<name>` から `<host>/<owner>/<repo>` を引くのには、既存の `boid project list` を使う。

手順:

1. `BOID_GIT_CACHE/<host>/<owner>/<repo>.git` に flock をかける
   - キャッシュがなければ `git clone --bare` する
   - あれば `git fetch` する
   - 失敗しても止めない (警告だけ出す)
2. `git clone --reference <cache> --dissociate <url> /workspace/<repo>` する。`--reference` を付けて失敗したら、付けずにもう一度 clone する
3. 作業ツリーのパスを標準出力に出す

branch は agent が自分で切り替える。dispatcher は `BOID_BASE_BRANCH` / `BOID_FORK_POINT` を env に書くだけで、`boid-task` スキルには次のことを書く。

- 主リポジトリは sandbox 内の起動ラッパーが `boid checkout` 済みであり、agent の cwd はその出力パス。agent は重ねて checkout しない
- 別リポジトリは `boid checkout <name>` で取得する
- `BOID_BASE_BRANCH` が origin にあれば、その branch に `git switch` する
- なければ、`BOID_FORK_POINT` (空なら origin の default branch) から `git switch -c` で作る

呼ぶのは sandbox 内の起動ラッパー (claude/codex/opencode の adapter run、および command hook / exec の shell adapter run)。`BOID_PRIMARY_REPO` が非空なら起動前に `boid checkout` を 1 回呼び、出力された `/workspace/<repo名>` を cwd にする。runner / daemon は checkout を呼ばず、branch も切り替えない。主リポジトリなしの job は既存の cwd のまま。default metaproject の judge はもう clone なしで HOME で動いている (`planner.go:81-83`) ので、workspace 直属の task は今と同じ形になる。

### 4.5 C5 参照キャッシュ

- workspace ごとの名前付き volume `boid-ws-gitcache-<installID8>-<slug>` を、job の `/var/cache/boid/git` に読み書き可でマウントする
- HOME の volume とは分ける。HOME はバックアップの対象だけど、キャッシュはいつ捨ててもいいから
- 消すのは workspace を消すとき。gc は git の auto gc に任せる。`--dissociate` で clone した作業ツリーはキャッシュに依存しないので、実行中の job がいても安全に gc できる
- readonly の job も書き込める。オブジェクトは内容でアドレスされるので、中身はすり替えられない。壊されても、C4 が `--reference` なしで clone し直す
- workspace をまたいで共有しない。private リポジトリのオブジェクトがほかの workspace から見えてしまうから

### 4.6 C6 git gateway (縮小)

今の `internal/gitgateway` を残す。役割は「job の git を forge へ中継して、資格情報を注入する」ことだけにする。

- forge の資格情報は今と同じく daemon の secret store に置く。daemon が自分で forge に接続するのは、この中継だけ
- job トークンの権限は、dispatch のときに次のように決める (今の `buildGatewayRepos` を置き換える):
  - workspace に登録された全リポジトリ (C1) に `PermFetch`
  - 主リポジトリには、`perm=push` で、かつ job が書き込み可のときだけ `PermFetchPush`
  - 主リポジトリがない task には、どこにも `PermFetchPush` を付けない
- job トークンの寿命は今と同じく job の寿命。再発行は要らない
- 消すもの: bare mirror を fetch するための資格情報の経路 (`FetchBareRepo` の `credentialGitArgs`)、`/mnt/refs/*` のマウント、peer の広告にある `reference_path`

## 5. 移行

2 段に分ける。どちらの段だけでも価値があって、次の段なしで止まっても壊れない。

| 段 | PR | 内容 | 採点 |
|---|---|---|---|
| 1 | PR-1 | C5 の volume と C4 の `boid checkout`、env の配線 | Q4-Q8 |
| 1 | PR-2 | sandbox 内の起動ラッパーが主リポジトリの `boid checkout` を呼び、そのパスを cwd にして起動する (runner / daemon は呼ばない)。`PrepareJobCheckout`、runner の clone と branch 解決 (`ResolveCloneBranchRef`)、`/mnt/refs/*` のマウント、dispatch 前の `FetchBareRepo` を消す。`boid-task` スキルに起動時 checkout 済みと agent 自身の branch 切り替えを書く。peer の広告から `reference_path` を消す | Q9-Q12 |
| 2 | PR-3 | C2: workspace に triggers / signals / card_commands / card_events を足す。読むときは project.yaml との和集合にする (名前がぶつかったら project.yaml を優先) | Q13-Q15 |
| 2 | (運用) | 実際の workspace ごとに、project.yaml の定義を workspace に移す | Q16 |
| 2 | PR-4 | C1 + C3 + C6: `perm` 列、`tasks` / `jobs` の `workspace_id`、`project_id` を nullable に、`trigger_runs` の付け替え、default metaproject の撤去、git gateway の権限の決め方の変更 | Q17-Q22 |
| 2 | PR-5 | project.yaml の読み込み、bare mirror、`project fetch` / `reload` を消す | Q23-Q24 |

**PR-4 のロールバック:** デプロイの前に DB をバックアップする。戻すときは、次のどちらかをやる。

- バックアップから戻す。デプロイ後に作った task は失われる
- `project_id` が NULL の行に、旧バイナリが読めるプレースホルダの project を入れてから、旧バイナリに戻す

どちらでも戻せるのは PR-5 の前まで。PR-5 で bare mirror を消したら、旧バイナリは project.yaml を読めない。

## 6. 決定と PR-3 の範囲

- **D1 workspace スキルの入れ方。** workspace 固有スキルは、既存の envelope の `spec.init_script` で workspace HOME に配置する。実体は HOME 内の専用ディレクトリに置き、`~/.claude/skills` と `~/.agents/skills` の両方にリンクする。組み込み・Pack の名前は使わない。現行 Pack 配線は全 workspace 共通で、workspace 別の選択機構を持たない (`internal/dispatcher/skills_overlay.go:128-168`)。Pack の選択機構や新しい skill DB 列は PR-3 に足さない。成立条件と比較は付録 B
- **D2 behavior 名の衝突。** PR-3 は現行の project.yaml 優先を維持する。workspace に同名異定義を上書きして集約しない。異なる用途を残す定義は workspace 内で改名し、同じ用途は owner が選んだ workspace 定義へ揃える。既存 task と呼び出し元を調べてから移す。取得済みの合成結果には `executor` / `supervisor` の異定義がある。全 workspace の DB 生定義との衝突件数は未確定で、0 件とは扱わない。取得範囲・件数・移行条件は付録 C

**PR-3 に入れるもの:** C2 の 4 フィールド (`triggers` / `signals` / `card_commands` / `card_events`) の保存・読み込み、workspace envelope の apply/export、project.yaml 優先の和集合読み、workspace の `signals.sources` からの trigger 導出、workspace 定義だけで動く経路と衝突優先順位のテスト (Q13〜Q15)。behavior の既存の名前単位の合成は維持する (`internal/orchestrator/project_store.go:435-455`)。project/project の定義を workspace に自動集約しない。

**PR-3 に入れないもの:** Pack の workspace フィルタ、新しい skill 配布 API、既存定義の改名・移行、task の behavior 書き換え、project.yaml の refresh/読み込み撤去、default metaproject 撤去、C1/C3/C6、API gateway の変更。schema の実装は PR-3 本体で行い、この決定記録 PR では行わない。

**未解決事項:** 全 workspace の所属と DB 生定義の監査、daemon のロード済み定義と forge HEAD の差分、運用移行時の改名表と既存 task の扱い。監査を完成し、衝突を解消して Q16 の一致を確認するまでは PR-5 に進まない。衝突検出の専用 API/CLI は PR-3 の必須範囲にしない。運用監査で raw 定義と合成結果を別々に保存し、欠測を未確定として扱う。

## 7. 採点表 — レビュワー用 yes/no 判定リスト

**極性は yes = 合格に統一する。根拠を diff か実データから引けない yes は no として扱う。no の解消は「実装を直す」か「先にこの doc の決定を更新する」のどちらかだけ。**

### A. 前提 (この doc 自体の採点)

| # | 問い |
|---|---|
| Q1 | 付録 A の各項目は、引用した file:line から実際に引けるか |
| Q2 | §3 の部品表は、§2 の a〜i を全部カバーしているか (どの部品にも割り当たっていない行がないか) |
| Q3 | §3 の呼び出し関係に、循環がないか |

### B. 段 1 (PR-1, PR-2)

| # | 問い |
|---|---|
| Q4 | `boid checkout` は broker を経由しないで、sandbox の中で完結しているか |
| Q5 | 2 つの job が同じリポジトリを同時に checkout しても、両方成功することをテストが示しているか (flock) |
| Q6 | キャッシュが壊れていても、checkout が `--reference` なしの clone で成功することをテストが示しているか |
| Q7 | clone した作業ツリーに `.git/objects/info/alternates` がないことをテストが示しているか (`--dissociate`) |
| Q8 | キャッシュの volume は workspace ごとに別で、ほかの workspace の job にはマウントされないか |
| Q9 | PR-2 の後に、`PrepareJobCheckout`、runner の clone、`ResolveCloneBranchRef`、`/mnt/refs` のマウントを呼んでいる経路が 1 つも残っていないか (到達可能性で確認する。名前の grep だけで済ませない) |
| Q10 | 2 回目の checkout で、転送量が初回より大きく減ることを実測したか |
| Q11 | readonly の job が `boid checkout` した作業ツリーから push したとき、gateway が拒否することを実機で確認したか |
| Q12 | agent が `boid checkout` を呼ばずに作業を始めたときや、`BOID_BASE_BRANCH` 以外の branch で作業したときに、それが分かるか (スキルに書いただけで終わっていないか) |

### C. 段 2 (PR-3〜PR-5)

| # | 問い |
|---|---|
| Q13 | project.yaml を持たない workspace で、triggers / signals / card_commands がすべて動くことをテストが示しているか |
| Q14 | `signals.sources` から trigger を導出する処理が、workspace の読み込みからも呼ばれているか |
| Q15 | 名前がぶつかったときに project.yaml が優先されることをテストが示しているか |
| Q16 | 移す前と後で、各 workspace の triggers / card_commands / behaviors の一覧が一致することを実データで確認したか |
| Q17 | 主リポジトリが空の task の job トークンに、どのリポジトリへの `PermFetchPush` も付かないことをテストが示しているか |
| Q18 | 主リポジトリがある task でも、主リポジトリ以外のリポジトリへの push が gateway で 403 になることをテストが示しているか |
| Q19 | `perm=fetch` のリポジトリを主リポジトリにした task は、作るときに拒否されるか |
| Q20 | 既存の task の `workspace_id` が、移行で全部埋まっているか (NULL の行が 0 件か) |
| Q21 | 付録 A.4 の default metaproject の特別扱いが、全部消えているか |
| Q22 | §5 のロールバック手順を、実際の DB のコピーで一度通したか |
| Q23 | PR-5 の後に、daemon から forge のホストへ出ていく接続が、git gateway の中継以外に 1 つもないか |
| Q24 | `extra_repos` を持っていた workspace で、そのリポジトリが `perm=fetch` の行として読めるか |

## 付録 A. 現行の事実

A.1〜A.5 は設計時点 (PR-1 / PR-2 前) の基準。D1/D2 の調査基準は main `a09940e00ef60c735ddfcc375721ecbd55393bea` (PR-2 後)。A.6 はこの基準で確認した。

### A.1 clone の経路は 2 本ある

- **git URL で登録した project (container backend)**
  - daemon が dispatch の前に mirror を fetch する (`runner.go:766-776`。失敗は警告だけ)
  - そのあと `PrepareJobCheckout` が `file://` で丸ごとコピーして bind する (`runner.go:786`、`checkout.go:56`)
  - このとき `cloneHostBacked=true` になって、sandbox の中の clone は無効になる (`sandbox_builder.go:1050`)
- **ホストのディレクトリで登録した project**
  - runner が gateway の URL から `git clone --reference /mnt/refs/self.git` する (`clone.go:94-102`)
- どちらの経路でも、hook / session / `boid exec` / trigger / card command の job は全部 clone される。例外は default metaproject の judge で、clone なしで HOME で動く (`planner.go:81-83`)
- cwd は `/workspace/<project-name>` (`sandbox_builder.go:789-797`)
- branch の解決は `sandbox.ResolveCloneBranchRef` を、runner の clone と `PrepareJobCheckout` の両方が使っている

### A.2 git gateway の権限

`buildGatewayRepos` (`gitgateway_wire.go:56-114`) が job ごとに権限を付ける。

- 自分の project: 書き込み可の job なら `PermFetchPush`、それ以外は `PermFetch`
- workspace のほかの project: `PermFetch`
- `extra_repos`: `PermFetch`

readonly は gateway だけで強制していて、`/workspace` の bind は常に読み書き可 (`sandbox_builder.go:1001-1006`)。

### A.3 project.yaml を読むところ

- `ReadProjectMetaFromBareRepo` が `git show HEAD:.boid/project.yaml` で読む (`project_bare_repo.go:82-100`)
- 呼んでいるのは `LoadBareRepo` / `LoadBareRepoExpectingID` / `LoadAll` (`project_store.go:212, 315, 706`)
- session の base branch も mirror の HEAD から決めている (`session_job.go:253`)
- 各フィールドを使っているところ:
  - `task_behaviors`: `behavior_resolve.go`、`coordinator.go`
  - `triggers`: `trigger_loop.go:535, 634`
  - `signals.sources`: `spec_loader.go:97-103` で trigger を導出する
  - `card_commands`: `card_command_launcher.go:223`、`card_request_auto_dispatch.go:56`
  - `card_events`: `project_store.go:644-650`

### A.4 default metaproject の特別扱い

`IsDefaultMetaproject` / `DefaultMetaprojectID` を使っている場所:

- `project_store.go:446, 517, 520, 699-703`
- `planner.go:81-83`
- `project_catalog.go:97`
- `workspace_repository.go:489-500`
- `workspace_apply.go:198`
- `project_service.go:664-666, 919, 1626, 1914`
- `wire.go:309`
- `web_card_create.go:34, 54`
- `task_form.templ:15, 31, 67`

`EnsureDefaultMetaprojects` を呼ぶのは Web のカード作成フォームだけ (`wire.go:1724-1728`)。

### A.5 DB

- `projects(id, work_dir NOT NULL, upstream_url, ...)`。workspace への所属は `project_workspaces` に持つ
- `tasks.project_id NOT NULL` (card も `tasks` の行)
- `jobs.project_id NOT NULL`
- `trigger_runs.project_id NOT NULL`
- `signals` / `signal_cursors` はもう `workspace_id` がキー

出典は `internal/db/migrate/testdata/schema.golden`。

### A.6 スキルの届き方

- 組み込みスキルは image の `/opt/boid/skills` にある (`build/container/Dockerfile:300-301`)。HOME の `~/.claude/skills` と `~/.agents/skills` にリンクする。Integration Pack のスキルも同じ経路でリンクする (`internal/dispatcher/skills_overlay.go:75-78,139-168`)
- Pack は全 workspace に同じものが届く。HOME volume が workspace 別でも、リンクの選択集合は変わらない (`internal/dispatcher/workspace_home.go:374,391-405`)
- nvt-tasks の HEAD `ecb3a9e9a257` には `.claude/skills/nvt-intake/SKILL.md`、`.agents/skills/nvt-judge/SKILL.md`、`.claude/skills/nvt-judge/SKILL.md` がある。boid の HOME 配線はこれらを列挙しない
- 同 HEAD の `.boid/project.yaml:145,209` は、card command `discuss` と behavior `judge` から `.agents/skills/nvt-judge/SKILL.md` を cwd 相対で参照する。`sweep` は `/nvt-intake` を呼ぶ (`:177,183`)
- PR-2 後も主リポジトリのある job は起動ラッパーの checkout 先を cwd にする (§4.4)。主リポジトリなしの workspace task へ移すと、この発見と相対参照を前提にできない。運用移行で 2 スキルを HOME へ配置し、`discuss` / `judge` の参照を `~/.agents/skills/nvt-judge/SKILL.md` に変える。両 discovery root に置いてから workspace 定義を有効にする
- nvt-judge は `workspace.md` / `projects/` / `company/themes.md` も参照する (同 HEAD `.agents/skills/nvt-judge/SKILL.md:68-70`)。スキルを HOME に置くだけでは資料は届かない。資料を使う job は `boid checkout nvt-tasks` して出力パスから読むか、運用者が HOME へ別途配布する。資料まで envelope が自動配布するとは扱わない

## 付録 B. D1 の根拠と成立条件

### B.1 Pack のモデルとロード経路

| 項目 | 事実と根拠 (main `a09940e0`) |
|---|---|
| 設定 | `IntegrationsConfig` は daemon 共通の `dir` だけを持つ。既定は `/opt/boid/integrations` (`internal/config/config.go:54-72`) |
| Pack の単位 | `<dir>/<pack>/<version>/integration.yaml`。`Pack` は name/version/dir/manifest を持ち、workspace ID を持たない (`internal/integrationpack/pack.go:9-26,41-62`) |
| manifest | `Manifest` は metadata/serviceProfiles/connectors/skills、`Skill` は name/path/requiresServiceProfile。workspace 選択フィールドはない (`internal/integrationpack/manifest.go:78-91,172-179`) |
| daemon のロード | startup に `LoadPacks` を 1 回呼び、同じ集合を `runner.Packs` に渡す (`internal/server/wire.go:1304-1319`) |
| HOME の選択 | `resolveWorkspaceHome(workspaceID)` は installID + slug の volume を選ぶ。一方、`skillLinks("", r.Packs)` に workspace 引数はない (`internal/dispatcher/workspace_home.go:335,374,392`) |
| リンク | 組み込みの後に全 Pack の全 skill を追加。`requiresServiceProfile` はフィルタにならない (`internal/dispatcher/skills_overlay.go:128-168`) |
| init → job | init container が各 root に rm + ln を行い、その HOME volume を job が mount する (`internal/dispatcher/workspace_init.go:549-565`、`internal/dispatcher/sandbox_builder.go:837-880`) |
| 内容の可視性 | リンク先は image 内の絶対パス。現行 image は Pack を build 時にコピーする (`build/container/Dockerfile:253-260`、`internal/dispatcher/workspace_init.go:205-242`)。daemon ローカルに置くだけでは sibling container に見えない |

**現行 Pack をそのまま workspace 固有スキルの配布には使えない。** daemon が forge に接続しないという条件は満たすが、workspace 別の選択と非公開スキルの分離は満たさない。新しいフィルタだけでも image 内の他 workspace 向け内容は隠せない。

### B.2 制約と更新

- Pack の追加・版変更は配置と daemon 再起動が必要。init と job の image に同じ絶対パスの内容を用意する。workspace の `container_image` override でもリンク先を用意する (init は backend の既定 image を使う: `internal/dispatcher/container_backend_workspace_init.go:138-148`)
- リンクの name/target 集合が変わると HOME を再初期化する。同じパスの内容だけの変更はこの比較では検知しない (`internal/dispatcher/skills_overlay.go:170-189`、`internal/dispatcher/workspace_home.go:118-149,403-406`)
- 組み込みと同名の Pack skill は除外する。Pack 同士の同名は最後のリンクが勝つ。版・pack の列挙順に依存するので、運用では同名を禁止する (`internal/dispatcher/skills_overlay.go:115-138,153-165`)
- prep は同名の HOME ディレクトリも消してリンクを張る。HOME 固有スキルは組み込み・全 Pack と名前を共有しない。user init は prep の後なので上書きは技術的に可能だが、推奨運用では禁止する (`internal/dispatcher/workspace_init.go:521-527,558-571`)
- Pack を外しても旧名のリンクは自動削除されない。管理していた旧名を運用で削除する (`internal/dispatcher/workspace_home.go:133-142`)

### B.3 選択肢

| 方式 | workspace 分離・再現性 | 追加実装 | 判断 |
|---|---|---|---|
| 現行 Pack をそのまま使う | HOME は別だが全 workspace に同じ名前・image の内容が届く | なし | 固有スキルには不採用。共通 API スキルには継続 |
| workspace 設定に Pack/skill 選択を足す | 選択は表せる。配布・削除・版・内容の分離を別途設計する | DB/envelope/dispatcher と image の契約 | PR-3 には入れない |
| job で HOME に手置きする | workspace 内で永続するが、初回準備・復元・更新の根拠が残らない | なし | 一時確認に限定 |
| 既存 envelope の `init_script` で HOME に配置する | script は workspace 別、export/apply で復元できる。実体も workspace HOME 内 | スキル専用の新規実装なし | **採用** |

### B.4 採用方式の契約

- `spec.init_script` を git 管理し、`boid workspace apply` する。実体を `$HOME/.local/share/boid/workspace-skills/<name>` に配置し、両 discovery root にリンクする。内容と必要な references/scripts も一緒に配置する。リポジトリの `/workspace/...` や init container の `/tmp/...` へリンクしない
- script を変えると次の dispatch で hash の変化により再実行する。再実行しても成功する配置にする。更新・削除対象は script が管理する名前に限定する。HOME は workspace 別だが、同じ workspace の全 job は同じ実体を見る。稼働 job が読み込む版を変える場合は、その workspace の job を止めて更新する (`internal/dispatcher/workspace_home.go:295-316,374,381-405`)
- 小さいスキルは script 内に内容を持たせる。外部配布物を使うなら運用者が用意した version/hash 固定の artifact を init container が取得する。init は job token / broker / git gateway を持たない。private repo の clone をこの経路の前提にしない。daemon 自身は forge に触れない (`internal/dispatcher/container_backend_workspace_init.go:119-136`)
- init_script は 128 KiB が上限 (`internal/api/workspace_init_script.go:56`)。大きい配布物は外部 artifact に分ける。envelope は script を運ぶのであって、任意の別ファイルを自動配布しない (`internal/orchestrator/workspace_envelope.go:75-81`)
- 組み込み/Pack と同名の配置、二重の nvt-judge 実体、旧名の放置を避ける。移行前に両 root から同じ `SKILL.md` を読めること、主リポジトリなしの job で呼べることを確認する。Q16 の運用確認に併記する。今回の調査でその移行が済んだとは扱わない

## 付録 C. D2 の実データと移行方針

### C.1 取得範囲

2026-10-10 の sandbox RPC (`boid project list`、各行の ID を指定した `boid project behaviors <id>`) と、git gateway で取得した default branch HEAD を使う。合成後の runtime 定義と forge の raw YAML は別の観測値として扱う。HEAD が daemon の mirror/cache と一致するとは限らない。合成後の behavior から workspace DB の生定義を逆算しない。

sandbox の `project list` は job の許可 project 集合だけを列挙し、workspace フィールドを返さない (`internal/server/boid_executor.go:696-747`)。behavior RPC も workspace 境界で制限する (`internal/sandbox/broker.go:477-487`)。全 workspace の DB と所属一覧はこの RPC では取得できない。取得した `Default (default)` は組み込み receiver で、`judge` は workspace DB の一覧ではない。receiver には workspace behaviors を合成しない (`internal/orchestrator/project_store.go:446`、`internal/orchestrator/default_metaproject.go:31`)。

### C.2 一覧

| 観測範囲 | project | forge HEAD | raw `.boid/project.yaml` の behavior | RPC の合成後 behavior |
|---|---|---|---|---|
| 現 job の許可範囲 | boid | [a09940e00ef6](https://github.com/novshi-tech/boid/commit/a09940e00ef60c735ddfcc375721ecbd55393bea) | executor, supervisor | drive, executor, implement, supervisor |
| 現 job の許可範囲 | Default (default) | — | 該当なし (組み込み) | judge |
| 現 job の許可範囲 | sumiron-procurement-tracker | [51b638ccc8ed](https://github.com/novshi-tech/sumiron-procurement-tracker/commit/51b638ccc8ed690a5caa7d98ac224908ae4e832c) | executor, supervisor | drive, executor, implement, supervisor |
| 現 job の許可範囲 | ubs-apps | [94e30db90db7](https://github.com/novshi-tech/ubs-apps/commit/94e30db90db7bfa2ad0e0a24449514a7c00c1de8) | executor, supervisor | drive, executor, implement, supervisor |
| 現 job の許可範囲 | bm-next | [c016fb3cc80c](https://github.com/novshi-tech/bm-next/commit/c016fb3cc80c3bcdee18863ce20f0ea2cf09d5ef) | executor, supervisor | drive, executor, implement, supervisor |
| 現 job の許可範囲 | bm-next-lp | [50a3fcc263c0](https://github.com/novshi-tech/bm-next-lp/commit/50a3fcc263c0447bdfaff260b702c47457c62582) | executor, supervisor | drive, executor, implement, supervisor |
| 現 job の許可範囲 | nvt-tasks | [ecb3a9e9a257](https://github.com/novshi-tech/nvt-tasks/commit/ecb3a9e9a25761ac73e929517a4d1f391b00d3d7) | sweep, judge | drive, implement, judge, sweep |
| 現 job の許可範囲 | harness-dojo | [b65b601c94ef](https://github.com/novshi-tech/harness-dojo/commit/b65b601c94ef4f1d96c2c5a6030eb54bfc3d58f8) | なし | drive, implement |
| 現 job の許可範囲 | kite | [446e32834169](https://github.com/novshi-tech/kite/commit/446e32834169d7ac49f44e83dc4cc1305bc1e15f) | なし | drive, implement |
| 現 job の許可範囲 | boid-api-skills | [f141b4c4a14f](https://github.com/novshi-tech/boid-api-skills/commit/f141b4c4a14f770fd3bf5d813b7b5ad8c6ccec50) | なし | drive, implement |
| 現 job の許可範囲 | api-gateway-mcp | [be6c8b462e51](https://github.com/novshi-tech/api-gateway-mcp/commit/be6c8b462e5107146f92fe01647ae4bc11457081) | なし | drive, implement |
| 現 job の許可範囲 | google-cli | [700d857d113f](https://github.com/novshi-tech/google-cli/commit/700d857d113fd4e241aa517fcbf6311c47f3b837) | なし | drive, implement |
| 現 job の許可範囲 | ms-graph-cli | [d33597be68de](https://github.com/novshi-tech/ms-graph-cli/commit/d33597be68de82b4c30c73128bae70df5c13ffa6) | なし | drive, implement |
| 現 job の許可範囲 | atl-cli | [f2cd9b5d60dd](https://github.com/novshi-tech/atl-cli/commit/f2cd9b5d60dd884d9267ee8cb09a77a81a125010) | executor, supervisor | drive, implement |
| 現 job の許可範囲 | board-cli | [b0bbb92fe0d5](https://github.com/novshi-tech/board-cli/commit/b0bbb92fe0d5289a00281747d893ad9eb775041b) | なし | drive, implement |
| 現 job の許可範囲 | novshi-tech-site | [ee89fc586bb9](https://github.com/novshi-tech/novshi-tech-site/commit/ee89fc586bb9cb72673da1e4586656578979626c) | なし | drive, implement |
| 現 job の許可範囲 | freee-cli | [2321c9404a9f](https://github.com/novshi-tech/freee-cli/commit/2321c9404a9f94767e9d6e1833b0b5c71fe677c6) | なし | drive, implement |
| 現 job の許可範囲 | ubs-bo | — | 取得不可 (403) | drive, implement |

`ubs-bo` の upstream は `github.com/nosen-nvt/bo`。advertised clone URL は 403 を返したため raw 定義は取得できなかった。ほかの 16 リポジトリは HEAD を取得した。raw に behavior を持つのは 7 リポジトリ、持たないのは 9 リポジトリ。`atl-cli` は raw に executor/supervisor があるが runtime にはない。boid の raw executor は PR-2 の起動時 checkout 指示を持つが、RPC は旧指示を返す。project.yaml refresh は PR-5 の範囲であり、今回これを実行して揃えない。

### C.3 衝突の数え方と観測値

同じ workspace の同名を 1 件と数える。異なる定義の project 対の数ではない。raw YAML はコメント・key 順を除いて比較し、明示フィールドの差は保持する。runtime は daemon の正規化結果を比較する。workspace が注入する env/host_commands/network は behavior 固有の差として数えない。DB と project の比較は同じ parser/normalizer を通した定義で行う。

| データ | 名前 | 定義を持つ project 数 | 異なる定義の数 | 衝突の意味 |
|---|---|---|---|---|
| 取得済み forge raw | executor | 6 | 5 | project/project の同名異定義候補 |
| 取得済み forge raw | supervisor | 6 | 5 | project/project の同名異定義候補 |
| RPC runtime | executor | 5 | 4 | 許可範囲内の同名異定義 |
| RPC runtime | supervisor | 5 | 4 | 許可範囲内の同名異定義 |
| RPC runtime | drive / implement | 各 17 | 各 1 | 取得範囲では異定義なし。DB 定義の確認ではない |
| RPC runtime | judge | 2 | 2 | nvt-tasks と組み込み receiver の違い。project.yaml 間の衝突には数えない |
| 全 workspace DB vs project | 全名 | 未取得 | 未確定 | 全 workspace の衝突件数は未確定 |

取得済み raw の同名異定義候補は 2 名 (executor / supervisor)。runtime も project 間は 2 名で、組み込み receiver を含めると judge が加わり 3 名になる。workspace 別の確定件数と DB/project 衝突件数ではない。

raw executor/supervisor の同一定義は bm-next-lp と atl-cli。sumiron-procurement-tracker は同じ本文だが `type: execution` が明示されているため raw では別定義。runtime では sumiron-procurement-tracker と bm-next-lp が同じになり、boid / ubs-apps / bm-next がそれぞれ別定義になる。raw と runtime の件数差を同一性の証明に使わない。

主な意味の違い:

- boid: executor は task branch + PR、supervisor はレビュー後に統合する
- sumiron-procurement-tracker / bm-next-lp / atl-cli の raw: executor は push/PR を禁止し、supervisor はローカルの子 branch を統合する。使い捨て clone の現在の契約とは一致しない
- ubs-apps: executor は PR と CI、supervisor は統合後の deploy まで追う
- bm-next: executor は PR と CI/CD、supervisor は PR merge。ubs-apps の統合後 deploy 確認とは異なる
- nvt-tasks judge: codex、model 指定、nvt-judge の固有判断。組み込み receiver judge は boid-card-judge。receiver 撤去時に同じ名前だからと置換しない

### C.4 解消と移行の条件

1. PR-3 では project.yaml の同名を丸ごと優先する。behavior 内の部分 merge はしない。workspace と異定義でも既存の実行を拒否せず、運用監査に残す
2. 運用で workspace の raw DB 定義と所属 project を一度に取得する。ホストの `boid workspace export --all` は atomic snapshot を返す (`cmd/workspace_export.go:17-42`)。共有に必要なのは metadata.name / spec.projects / spec.task_behaviors だけで、env や資格情報は不要。別 workspace の project は、その workspace の job またはホストから raw 定義・ロード済み合成結果を取得する
3. 同じ用途は owner が選んだ workspace 定義に揃える。違う用途は `<project>-<role>` など一意な名前に改名する。`executor`/`supervisor` の名前から readonly を暗黙に得ていた定義は、改名時に readonly を明示する (`internal/orchestrator/spec_loader.go:399-425`)。alias に依存しない
4. default_task_behavior、trigger の run、card command、card event、子 task 作成指示、保存済み task の behavior を対応表で照合する。既存の queued/awaiting task をどの定義で再開するか決めてから旧名を外す。本文の obsolete な push 禁止はそのまま canonical に採用しない
5. Q16 は名前だけでなく意味と呼び出し先を比較する。改名がある場合は対応表で同じ挙動になることを確認する。PR-5 は全 workspace の監査が揃い、未解消衝突が 0 件、HOME 固有スキルが使える状態になってから行う。今の取得範囲だけで Q16 を yes にしない
