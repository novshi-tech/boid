# git gateway + sandbox 内 clone 実装計画

ステータス: 計画 (planned)
作成日: 2026-07-09
親ドキュメント: [container-based-boid.md](container-based-boid.md) — 移行戦略ステップ 2

---

## 目的

git builtin (broker 経由のホスト側 git 実行) と worktree bind mount を廃止し、
**sandbox 内の credential レス素 git + git gateway (認証注入リバースプロキシ) +
job ごとの sandbox 内 clone** に置換する。ワンセットで切り替える
(共有 `.git` のまま sandbox 内 git 実行に変えると hooks 経由のエスケープが開くため、
分割不可 — 親ドキュメント参照)。

これで退役するもの: git builtin の引数ポリシー (`-C`/`-u` 拒否)・
`core.hooksPath=/dev/null`・remote snapshot 機構・brokered `clone --local`・
worktree 割当機構・branch lock / HEAD ガード。

スコープ外: mirror (ローカルはホスト repo 直接 reference のため不要)、
コンテナ enforcement (ステップ 6)、push 粒度ポリシー (receive-pack 検査)。

---

## 前提となる決定事項 (親ドキュメント反映済み)

- gateway は Go 標準 `httputil.ReverseProxy` の自作薄層。ポリシー = boid /
  enforcement = gateway の URL ルーティングに一本化
- token は個人 (user-level)。GitHub = fine-grained PAT (username 慣例
  `x-access-token`)、Bitbucket = API token (username
  `x-bitbucket-api-token-auth`)。失効検知 (上流 401) を設計に含める
- clone 元 URL は常に gateway、`--reference <ホスト repo>` は任意の最適化
- clone 先はホスト側 runtime dir の bind mount、sandbox 内は中立 path `/workspace`
- reopen = 再 clone + branch checkout。保証は commit(+push) 済みのみ
- release 検証は daemon 側 git データに fetch してから検証 /
  branch・fork point 解決は clone 直後に runner / branch lock・HEAD ガード退役
  (2026-07-09 決定 3 点)
- workspace 設定に read-only 追加許可 repo の語彙 (private git 依存用)
- リモート無し project は 1 件 (skill-only metaproject) → private repo 化で対応
- ホスト側 repo の origin は上流のまま変えない (gateway 経由は sandbox 内 clone のみ)

---

## 本計画で確定する設計

### 1. project → 上流 URL の明示マッピング

- **DB (projects テーブル) に `upstream_url` カラムを追加**。project.yaml には
  置かない (URL は repo 自体から導出可能な machine-local 情報で、commit 対象に
  馴染まない。またコンテナ期には work_dir 自体が消えるため DB 保持が終着点)
- capture タイミング: `project add` / `project reload` 時に work_dir の
  `origin` remote を読み、SSH → HTTPS 正規化
  (`git@github.com:o/r.git` → `https://github.com/o/r.git`) して保存
- **origin の無い project は登録拒否** (新しい意味論。既存 project の backfill は
  reload / 起動時 migration で行い、欠落 project は警告 + dispatch 時エラー)

### 2. gateway の認可モデル: 単一サーバ + job token の path prefix

- gateway は daemon 内の単一 HTTP サーバ (`127.0.0.1:0` で listen)。
  sandbox からは `10.0.2.2:<port>` で直接届く (nftables は 10.0.2.2 全ポート
  許可済み・`no_proxy` に 10.0.2.2 が入っており egress proxy を経由しない —
  下調べで確認済み。つまり **gateway の URL ルーティングが唯一の enforcement**)
- URL 形式: `http://10.0.2.2:<port>/j/<job-token>/<host>/<owner>/<repo>.git`。
  job token は broker token と同じ方式 (`crypto/rand`、dispatch 時登録・
  job 終了時失効) の別トークン。clone の origin URL に埋まるが job スコープなので
  漏洩窓は job 寿命と同じ
- token → 許可集合のレジストリ (broker の TokenContext レジストリの鏡写し):
  - 自 project: fetch + push (`task.readonly` / `command.readonly` の場合 fetch のみ)
  - workspace peer: fetch のみ (upload-pack のみ許可)
  - workspace の read-only 追加許可 repo: fetch のみ
- forge token (PAT) は secret store (`internal/dispatcher/secret_store.go`) に置き、
  config.yaml の gateway ブロックは forge 種別と secret key 参照のみ持つ
  (平文 config に PAT を書かせない — kit init の secret 検出と同じ規律)
- 上流 401 は「token 失効」として警告ログ + notify。gateway 自体は落とさない

### 3. readonly の意味論変更: FS-RO → transport-RO

現行の readonly job は project mount 自体が read-only。clone モデルでは
**clone はローカルに常に書ける (自分のコピー) が、push が gateway で拒否される**
形に変わる。「何も書けない」から「書いても境界を越えない」への変更で、
「commit されたものだけが境界を越える」の読み書き対称な適用。
readonly フラグの役割 (boid-task のモード判定・gateway の rw/ro) は不変。
review 系タスクがテスト実行等でワークツリーに書けるようになる副次効果もある。

### 4. 全 project 可視 job が clone になる (session / exec 含む)

`boid agent claude` の対話セッションや `boid exec` も同じ dispatch 経路なので
clone + `/workspace` に揃える。**セッションでの編集はホスト work_dir に直接
反映されなくなり、push して初めて共有される** (dogfood ワークフローの変化。
チェックリスト参照)。exec は task/branch を持たないため
「default branch を clone・branch 作成なし」を既定とする。

### 5. branch 宣言の JobSpec 化

worktree resolver が host repo でやっていた解決 (`worktree_manager.go:35-257`) を
「宣言」と「解決」に分離する:

- dispatcher は JobSpec に宣言のみ載せる: `branch` (作業 branch 名)、
  `base_branch`、fork 規則 (親 branch からの分岐等、現行 resolveCase3 相当)
- runner が clone 完了後に解決: `rev-parse` / `merge-base` / `checkout -B`。
  base branch が上流に無い等の失敗は job 起動エラーとして
  runner-state.json + stderr に残す

---

## e2e 戦略 (cutover より前に必要)

e2e シナリオの project は全てリモート無しのローカル repo のため、
**cutover した瞬間に全シナリオが dispatch 不能になる**。順序制約として先に解く:

- **fixture upstream サーバ**: bare repo 群を smart HTTP で serve する小さな
  テストヘルパ (Go 標準 `net/http/cgi` + `git http-backend`)。
  gateway の上流としてシナリオごとに起動
- **シナリオ harness の対応**: project セットアップ時に bare upstream を作り
  origin に設定して push、project 登録が upstream_url を capture する形に変更。
  cutover 前でも remote があるだけなので既存シナリオは無害に通る
- `git-peer-clone-local` シナリオは動的 peer clone (gateway 経由 fetch-only) の
  シナリオに置換
- 新規シナリオ: (1) clone → commit → push が gateway 経由で通る、
  (2) readonly job の push が拒否される、(3) peer は fetch のみ、
  (4) reopen が再 clone + branch checkout になる、
  (5) 許可外 repo への push/fetch が gateway で 403
- gateway 単体は httptest + cgi でユニット側に置く (packfile 大 POST の
  ストリーミング、`100-continue`、chunked の実機検証はここで)

---

## PR 分割

### PR1: release 検証の fetch 化 (独立・先行可)

- `internal/api/task_notify.go:315` 周辺 (`gitObjectExists` / `gitRemoteTip`) を
  「project work_dir で `git fetch origin` してから cat-file / 検証」に変更。
  **現行世界でも正しく、cutover 後は必須**になる両立変更
- 既知の副産物: `task_notify_doneverify_test.go` は sandbox 内で `git -C` 実打ちの
  ため落ちる (brokered git の -C 拒否)。CI が正
- 意味論変更 (origin に push 済みのみ通る) を docs の hook-contract 等に反映

### PR2: project upstream mapping

- projects テーブルに `upstream_url` カラム (migration)、
  add / reload での capture + SSH→HTTPS 正規化 + リモート無し登録拒否、
  起動時 backfill。`project show` / `list` に表示
- internal/db 層を触るため sandbox ではビルド不可 — 検証は CI

### PR3: `internal/gitgateway` パッケージ (単体・inert)

- ReverseProxy ベースの smart HTTP 転送 (`GET /info/refs`、
  `POST /git-upload-pack` / `/git-receive-pack` のみ)。ボディは無バッファ転送
- forge 別 Basic 認証注入 (github / bitbucket の username 慣例 + token 種別)、
  secret store 参照、上流 401 の検知
- job token レジストリ (登録 / 失効 / rw・ro 判定)。パスルーティングは
  `<host>/<owner>/<repo>.git` 完全一致のみ
- httptest + cgi の単体テストで転送細部 (chunked / 100-continue) を検証

### PR4: gateway のライフサイクル + dispatch 配線 (inert)

- `internal/server/server.go` の 4 点セット (field / New / Start / Stop) に組み込み
- config.yaml に gateway ブロック (forge 種別・secret key 参照)。
  UnmarshalYAML の影 struct 三点セットに注意
- dispatch 時に job token を発行して gateway に登録、
  `SandboxRuntimeInfo` に `GatewayURL` / token を追加 (`sandbox_builder.go:22`)、
  job 終了時失効 (`UnregisterJob` の並び)
- workspace.yaml に read-only 追加許可 repo の語彙 (`extra_repos` 等) を追加し
  レジストリの許可集合に合流
- この時点では誰も使わない (環境変数等での advertise は cutover PR で)

### PR5: runner の clone 実行機構 + branch 宣言 (inert)

- JobSpec に branch 宣言フィールド追加、runner が spec を受けて
  clone (`--reference` 付き、gateway URL) → branch 解決 → `/workspace` 完成、
  の起動シーケンスを実装。reopen = 同シーケンス再実行
- ホスト repo `.git` の RO bind (reference 用) と peer bare の RO bind を
  mount 列に追加できるようにする (`/mnt/refs/<project>.git` 等の中立 path)
- 失敗時の診断: runner-state.json に clone / 解決の結果を残す

### PR6: cutover (一気切替)

- sandbox_builder: worktree / project bind (`projectVisibilityMounts` step1/3/6/7)
  → runtime dir の `/workspace` bind + PR5 の clone シーケンス起動に切替。
  cd 先も `/workspace`
- git shim の退役 (PATH overlay から除去 — sandbox の git は実バイナリに)。
  `core.hooksPath` ハック・remote snapshot capture (`broker.go:76`) の停止
- worktree 割当 (`resolveWorktree`) の停止、branch lock (`AcquireForTask`) と
  HEAD ガード (`EnforceHeadOnBaseBranch`) の退役
- broker の cwd 検証 root を `/workspace` に変更 (`broker.go:531-546`)、
  hook argv remap (`sandbox_builder.go:304-313`) の worktree 前提を除去
- peer advertise を `{name, clone URL, reference path}` に変更
  (当面 environment.yaml 経由のまま。RPC 化はステップ 5)
- environment.yaml の notes 書き換え: brokered git の癖 (-C/-u 拒否・
  remote snapshot) の記述を削除し、gateway 経由 transport の説明に置換。
  boid-task スキル等の worktree 文言も追随
- **PR7a (e2e harness) が先に main に入っていることが前提**

### PR7: e2e

- **PR7a (cutover 前)**: fixture upstream サーバ + シナリオ harness の
  upstream 付与 (既存シナリオはこの時点でも green)
- **PR7b (cutover 後)**: 新規シナリオ 5 本 (e2e 戦略の節) +
  `git-peer-clone-local` の置換

### PR8: 大掃除 (cutover 安定後)

- `internal/sandbox/git_builtin.go` (~930 行) / `GitOpCloneLocal` /
  `TokenContext.WorktreeDir`・`WorkspacePeers` の path 検証 /
  protocol の GitOp 群 / policy 登録 (`policy.go:122`・`policy_ops.go:29`) を削除。
  **policy_test の wantOps + drift test の更新を忘れない**
- worktree_manager / store / resolver (4 ファイル) と worktrees テーブル
  (drop migration)、GC の `cleanWorktrees` (`repository.go:327-393`) と
  Worktrees カウント表示、`project_lock.go` を削除
- 関連メモリ・docs の棚卸し (sandbox-gh-git-quirks の git 記述等は陳腐化)

依存関係: PR1 / PR2 は独立先行可。本線は PR2 → PR3 → PR4 → PR5 → PR7a → PR6 →
PR7b → PR8。PR3〜PR5 は inert なので個別に安全に land できる。

---

## dogfood チェックリスト (ホスト側・PR6 マージ前)

1. GitHub fine-grained PAT の発行と `boid secret set` での登録
   (対象: novshi-tech の全 repo + read-only 依存があればそれも)
2. skill-only metaproject (khi) の private repo 化と `project reload`
   (全 project の upstream_url capture 確認は PR2 の backfill ログで)
3. **host 側 daemon からの fetch 疎通確認** (release 検証の前提条件):
   全 project について daemon 実行ユーザで `git -C <work_dir> fetch origin` が
   成功することを確認する。private origin の SSH key / HTTPS token を持たない構成では
   PR1 で fetch 化した `verifyDoneClaim` が cutover 後に永久失敗 → `notify --done`
   永久ブロック (「落とし穴・注意」節参照)。cutover 前は fetch 失敗が benign なので
   顕在化せず、PR6 マージ時にまとめて刺さる — 事前に必ず踏む
4. **ワークフロー変化の周知**: cutover 後は対話セッション (`boid agent claude`)
   の編集もホスト work_dir に直接反映されず、push して初めて共有される。
   「done 前に push」の現行規律がそのまま前提になる
5. ロールバック手段の確認: cutover (PR6) は Phase 3-a と同じ
   「並走なし一気切替 + git revert 猶予」。PR6 単体 revert で旧経路に戻れる状態を
   PR8 (削除) まで維持する — **PR8 は PR6 の安定稼働を数日確認してから**

---

## 落とし穴・注意

- **gateway への直続は egress 機構の完全な外**: nftables は 10.0.2.2 の全ポートを
  許可済みで proxy allowlist も経由しない。認可はすべて gateway 内の
  job token + URL ルーティングに閉じる (設計どおりだが、テストで
  「許可外 repo 403」を必ず踏む)
- **dockerproxy を手本にしすぎない**: 既存 dockerproxy はボディを
  `io.ReadAll` している。packfile の大 POST はストリーミング必須
- **clone の URL に job token が入る**: sandbox 内 `.git/config` に残るが
  job スコープで失効する。ログ・診断出力 (runner-state.json) への
  token 混入は redact 対象に含める
- `--reference` はホスト repo の `gc --prune` に対して脆弱 (借用オブジェクトが
  刈られうる)。job は短命なので許容だが、失敗時は reference 無し clone に
  フォールバック (graceful degradation は契約どおり)
- exec / ProfileInit 経路: ProfileInit (kit init / workspace configure) は
  project を clone しない特殊分岐のまま変更しない
- `${current_branch}` 展開 (`branch_var.go:45`) は host work_dir 読みで存置
  (worktree 非依存)。ただし host HEAD と job branch の意味の乖離が
  混乱を生まないか instruction 文言を確認
- sqlite 依存層 (PR2) と sandbox runner の E2E (PR6/7) は sandbox 内で
  検証不可 — CI (blackbox-e2e.yml) が正
- **release 検証 fetch は cutover 後 host 側 daemon で走る** (PR6 でのみ顕在化・PR1 レビューで指摘):
  PR1 で `verifyDoneClaim` に足した `git fetch origin` は daemon プロセスで実行される
  (gateway は sandbox 側専用のため経由できない)。cutover 前の shared-worktree 世界では
  fetch 失敗は benign (commit がローカル `.git` に既にある) だが、cutover と同時に
  「fetch → cat-file」の順が意味論の中核になるため、host ユーザが private origin の
  credentials (SSH key / HTTPS token) を持たない構成では fetch 永久失敗 →
  `notify --done` 永久ブロックが起きうる。dogfood チェックリスト (PR6 マージ前) に
  「host 側 daemon から各 project の origin に fetch できること」の確認手順を含める。
  gateway 経由 fetch (daemon → gateway → 上流) にする案は architectural には整合するが、
  daemon が自分の gateway の client になる循環になるため、初期スコープでは
  host credentials 前提で回避する

---

## post-cutover 改善候補 (動作確認後に対応)

PR6 マージ後、現行 config surface で dogfood を続けつつ、以下 3 件は
cutover 本体と切り離して個別 PR で改善候補として残す (2026-07-10 nose 判断)。
実装優先度は「n=1 の個人利用では顕在化しないが、顧客展開・複数 organization
運用で必須になる」順。

### 1. workspace-scoped PAT namespace 方式

**問題**: 現行の `gateway.hosts[]` は daemon global に `host → secret_key` を
mapping する方式で、**同じ github.com に対して workspace ごとに token を
切り替えられない**。顧客展開で複数 organization を 1 daemon で束ねると、
どの workspace も同一 PAT で認証注入され、service account の identity 分離が
できない。

**解決方針**: workspace = 顧客 = secret namespace = identity として扱う。

- **config.yaml (全 workspace 共通、キー名の契約)**:

  ```yaml
  gateway:
    hosts:
      - host: github.com
        forge: github
        secret_key: gh-pat        # workspace 横断で共通のキー名
  ```

- **workspace.yaml (顧客ごとの identity)**:

  ```yaml
  secret_namespace: customer-a
  ```

- **登録**:

  ```bash
  boid secret set --namespace customer-a gh-pat <PAT-A>
  boid secret set --namespace customer-b gh-pat <PAT-B>
  boid secret set gh-pat <PAT-personal>   # namespace = default (fallback)
  ```

- **解決**: dispatcher が workspace 情報を gateway resolver に渡し、
  `secretStore.Get(<workspace_namespace>, <secret_key>)` で lookup。
  `secret_namespace` 指定なしの workspace は `default` fallback で
  backward-compatible (PR4 の現行実装がそのまま動く)。

**config 直積 (host + owner glob matching) を採らない理由**: config surface に
workspace concept を持ち込むと「workspace = identity、config = 接続契約」の
layering が崩れる。namespace = identity の 1 軸で分離する方が symmetric で、
かつ secret store には既に namespace 概念が存在する (`boid secret set` の CLI が
既にサポート) — 新軸を足すのでなく既存軸の露出。

**実装スコープ**: dispatcher の resolver 呼出しに workspace scope を渡す配線、
workspace.yaml に `secret_namespace` フィールド追加、対応する e2e シナリオ。

### 2. config surface 圧縮 — forge → host 導出

**問題**: `gateway.hosts[]` の各エントリで `host` と `forge` を両方書いているが、
SaaS Cloud 前提 (github.com / bitbucket.org) では forge が host を一意に決めるため
**冗長**。現行 boid のスコープでは Enterprise 対応は入っていない。

**解決方針**: forge を primary key にし、host は forge から default 導出
(Enterprise 対応時のみ override 可)。

```yaml
gateway:
  forges:
    github:
      secret_key: gh-pat            # host: github.com が default
    bitbucket:
      secret_key: bb-token           # host: bitbucket.org が default
    github-enterprise:                # 将来 Enterprise を足すとき
      host: github.corp.example.com
      secret_key: ghe-pat
```

**後方互換**: 現行の `hosts: [...]` を deprecation warning 付きで 1 リリース残し、
`forges:` map に自動変換 (host 名で forge 判定できる)。

**実装スコープ**: `internal/config/config.go` の UnmarshalYAML と test、
docs (`docs/*/reference/config-yaml.md`) 更新。

### 3. `.boid/` gitignore contract の user-facing 明文化

**問題**: post-cutover は `.boid/` (project.yaml、hooks/*.sh 等) が sandbox 内
clone 経由でしか届かない。`.gitignore` で `.boid/` を除外している project は
cutover 後の dispatch で hook スクリプトが消える (PR6 Opus レビュー finding #7)。
boid 本体は `.boid/project.yaml` を tracked にしているので dogfood は動くが、
user 側の project は個別確認が要る。

**解決方針**: dogfood チェックリスト (本 doc の上節) に以下を明示追加 (計画書
更新のみ、コード変更なし)。cutover 後に既存 project へ配布する onboarding
文書 (CLAUDE.md 等) にも「`.boid/` は commit 対象」を明記する。

```bash
# 各 project で
git check-ignore .boid          # 何も出力されなければ tracked (OK)
git check-ignore .boid/hooks    # 同上
```

出力があった project は `.gitignore` から `.boid/` を除外するか、
`.boid/hooks/*.sh` だけ tracked にする。

**依存**: 実装 (コード変更) 不要、本 doc の反映と onboarding 文書更新のみ。
