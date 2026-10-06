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
  agent か command hook ──exec──▶ C4 boid checkout
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

- 最初に `boid checkout` して、出力されたパスに cd する
- `BOID_BASE_BRANCH` が origin にあれば、その branch に `git switch` する
- なければ、`BOID_FORK_POINT` (空なら origin の default branch) から `git switch -c` で作る

呼ぶのは agent か command hook。runner は呼ばない。job は空の `/workspace` で始まる。default metaproject の judge はもう clone なしで HOME で動いている (`planner.go:81-83`) ので、workspace 直属の task は今と同じ形になる。

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
| 1 | PR-2 | job を空の `/workspace` で始める。`PrepareJobCheckout`、runner の clone と branch 解決 (`ResolveCloneBranchRef`)、`/mnt/refs/*` のマウント、dispatch 前の `FetchBareRepo` を消す。`boid-task` スキルに checkout と branch 切り替えを書く。peer の広告から `reference_path` を消す | Q9-Q12 |
| 2 | PR-3 | C2: workspace に triggers / signals / card_commands / card_events を足す。読むときは project.yaml との和集合にする (名前がぶつかったら project.yaml を優先) | Q13-Q15 |
| 2 | (運用) | 実際の workspace ごとに、project.yaml の定義を workspace に移す | Q16 |
| 2 | PR-4 | C1 + C3 + C6: `perm` 列、`tasks` / `jobs` の `workspace_id`、`project_id` を nullable に、`trigger_runs` の付け替え、default metaproject の撤去、git gateway の権限の決め方の変更 | Q17-Q22 |
| 2 | PR-5 | project.yaml の読み込み、bare mirror、`project fetch` / `reload` を消す | Q23-Q24 |

**PR-4 のロールバック:** デプロイの前に DB をバックアップする。戻すときは、次のどちらかをやる。

- バックアップから戻す。デプロイ後に作った task は失われる
- `project_id` が NULL の行に、旧バイナリが読めるプレースホルダの project を入れてから、旧バイナリに戻す

どちらでも戻せるのは PR-5 の前まで。PR-5 で bare mirror を消したら、旧バイナリは project.yaml を読めない。

## 6. 先に決めること

- **D1 workspace スキルの入れ方。** 第一候補は、既存の Integration Pack のスキル配線 (`skills_overlay.go:139-168`、HOME に symlink を張る) に乗せること。pack は daemon のローカルディレクトリなので、daemon は forge に触らなくて済む。実装の前に、pack の仕組みで workspace ごとにスキルを出し分けられるかを確かめる
- **D2 behavior 名の衝突。** 今は project ごとに `task_behaviors` を持っている。1 つの workspace に、同じ名前で中身の違う behavior を持つ project が複数あったら、workspace に移すときに衝突する。PR-3 の前に、実際の workspace ごとに数えて、どう解消するかを決める

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

- 組み込みスキルは image の `/opt/boid/skills` に焼き込んであって、HOME の `~/.claude/skills` と `~/.agents/skills` に symlink を張る。Integration Pack のスキルも同じ仕組みで張る (`skills_overlay.go:139-168`)
- リポジトリの中のスキル (nvt-tasks の `.claude/skills/nvt-intake` など) は、cwd がリポジトリなので Claude Code が見つけている。boid はこれを配線していない
- nvt-tasks の card command `discuss` は、`.agents/skills/nvt-judge/SKILL.md` を相対パスで参照している
