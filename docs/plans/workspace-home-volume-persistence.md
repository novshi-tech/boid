# workspace HOME の named volume 化 (永続性退行の修復)

**状態**: 実装中 (2026-07-26 作成。PR1 / PR2 landed、PR3 以降未着手)
**発端**: 2026-07-26 volume-only dogfood
**関連**: `home-workspace-volume.md` (Phase 4、破られた契約の出典) / `volume-only-daemon.md` (退行を持ち込んだ cutover) / `phase6-container-backend.md`

## 背景: これは follow-up ではなく退行

Phase 4 (`home-workspace-volume.md`) は workspace HOME の**永続**を明文の契約として決めた。

> `homes/` は runtimes/ と違い **GC 対象外** (workspace 永続) — `home-workspace-volume.md:118`

> セッションが workspace home に書くので認証はそのまま永続する — 同 `:35`

この契約は userns backend では成立していた。workspace HOME が
`workspaceDataHomeRoot()` = `~/.local/share/boid/homes/<slug>` (XDG data dir、永続)
に置かれていたためである。

解決経路は一貫して `WorkspaceHomesDir(runtimesDir)` = `filepath.Dir(runtimesDir)/homes`
であり (`workspaceDataHomeRoot()` は runtimesDir が空のときの fallback にすぎない)、
**退行の全ては runtimesDir の根が XDG data dir から XDG_RUNTIME_DIR に変わったこと**にある。
container backend の runtimesDir は `internal/server/wire.go` の
`hostVisibleRuntimesDirFor(cfg)` = `filepath.Dir(cfg.SocketPath)/runtimes` に解決され、
実デプロイではこれが `BOID_RUNTIME_DIR` = `/run/user/<uid>` 配下、すなわち **tmpfs** になる。

`compose.yml` のヘッダコメントはこの帰結を認識しており、こう書いている:

> BOID_RUNTIME_DIR is host XDG_RUNTIME_DIR, typically tmpfs — workspace HOME
> under container backend does not survive a host reboot the way it does for
> the userns backend; a docker-managed named volume with per-workspace Subpath
> mounts is the likely eventual fix, tracked as a follow-up, not blocking this PR.

「userns backend なら永続する」ことを先送りの根拠にしていたが、**PR-4
(`volume-only-daemon.md` §論点e) で userns backend を撤去した時点でその根拠は消滅した**。
container backend が唯一の経路になった今、Phase 4 の永続契約は「どの経路でも成立しない」。
これは未着手の改善項目ではなく、明文化済みの契約が破れている退行として扱う。

**実害**: harness の認証情報 (`~/.claude/.credentials.json`、`~/.claude.json`) と
init.sh が構築した toolchain (go / node / volta / claude / codex / opencode、実測 1.5GB) が
**ホスト再起動のたびに全消失**する。復旧には workspace ごとに対話認証をやり直す必要がある。

## 実測で確定した事実 (2026-07-26)

| 確認事項 | 結果 |
|---|---|
| `/run/user/1000` の種別 | `tmpfs` (6.0G、`mount` 出力で確認) |
| 認証情報の実際の所在 | `/run/user/1000/homes/default/.claude/.credentials.json` — tmpfs 上 |
| **podman 4.9.3 の mount `Subpath` 対応** | **無効。エラーを返さず silent に無視される** |

Subpath の実測は重要なので詳細を残す。`boid-runner:latest` に対し
`Mounts:[{Type:volume, Source:<vol>, Target:/home/boid, VolumeOptions:{Subpath:"homes/default"}}]`
で container を作成したところ、podman 4.9.3 は 201 を返して起動し、`/home/boid` には
**volume のルート**がマウントされた (`homes/` がそのまま見えた)。Subpath は比較的新しい
docker / podman でのみ対応する機能で、podman 4.x は当該フィールドを黙って捨てる
(導入バージョンの正確な下限は一次情報で未確認 — docker は Engine 25.0 か 26.0、
podman は 5.0 系。設計判断には影響しないが、doc に版を書くなら裏取りすること)。

→ **単一 volume + per-workspace Subpath という設計 (compose.yml のコメントが示唆していた方向) は
採用できない**。採用した場合、podman 4.x 上では全 workspace が同一の volume ルートを共有し、
workspace 分離が silent に破れる。これは Phase 4 が dogfood で確認した分離契約
(`next-session-home-workspace-volume`: 「全 5 workspace で auth 永続 + 分離確認」) の
直接的な違反であり、しかも症状が出るまで気付けない種類の破れ方をする。

## 決定: 案 A (per-workspace named volume + init.sh は使い捨て container)

nose 判断 (2026-07-26)。検討した 3 案:

| 案 | 内容 | 判断 |
|---|---|---|
| **A** | workspace ごとに named volume を作り job container に `/home/boid` としてマウント。init.sh は当該 volume をマウントした使い捨て container で実行 | **採用** |
| A' | 同上、ただし init.sh は job container 内で実行 | 不採用 (init.sh の trusted 境界が変わる) |
| B | 単一 volume + per-workspace Subpath | **不採用** (podman 4.9.3 が Subpath を silent 無視するため、workspace 分離が破れる。上記実測節を参照) |
| B' | B を採用し podman 5.0+ を要求する | 不採用 (nose host が 4.9.3。engine バージョン下限を上げる判断は本件の範囲を超える) |
| C | compose に `~/.local/share/boid/homes` の bind mount を足し host 永続 dir に戻す | 不採用 (host filesystem 依存の積み増しは pivot の目的に逆行) |

C を退けた理由は `volume-only-daemon.md` の pivot 判断そのものである。「シークレットをバインドするのは
ホスト側に必要なファイルがある前提で、k8s 上でお客様環境という将来目標と衝突する」。
workspace HOME は認証情報を抱える最も永続性を要求される資産であり、`BOID_RUNTIME_DIR` と同じ
「k8s で成立しない負債」カテゴリに置くのは筋が悪い。

## 論点

### 論点 a: volume の命名と label — 既存 reap から「発見されない」ことが要件

命名は `containerWorkspaceNetworkName` の既存規約に倣う。install_id で scope し、
workspace slug をサニタイズして連結する:

```
boid-ws-home-<installID8>-<sanitized-slug>
```

**label 設計は「reap のために付ける」ではなく「reap に見つからないようにする」が正しい。**
現行の掃除経路を素直に踏襲すると、daemon を再起動するたびに認証データが消える —
本 doc が直そうとしているインシデントの再演になる。破壊経路は **7 つ**ある
(初版は 4 つ、経路 5 = `POST /volumes/prune` は PR1 実装時に発見。
経路 6 / 7 は PR1 の codex レビューで発見した。この 2 つは
**volume object を消さずに中身だけ破壊する**点が 1-5 と質的に違う):

1. `internal/reap/reap.go:124` — `reap.Run` は列挙した volume を `VolumeRemove{Force: true}` で
   **無条件に全削除**する。列挙の filter は `boid.install_id=<id>`。そしてこれは
   `boid reap` CLI 専用ではなく、`internal/dispatcher/container_backend.go:1296` の
   `ReapOrphans` (**daemon 起動時の startup reap**) から毎回呼ばれる。job container が
   生きていれば in-use で守られるが、ホスト再起動直後は必ず成功する。

   **列挙は 2 本必要** (PR1 codex レビュー Blocker 1)。HOME volume は
   `boid.install_id` を**意図的に持たない** (それがこの filter そのものだから) ので、
   1 本目の query では**そもそも列挙されない**。列挙されなければ skip 判定にも到達せず、
   「skip した volume は必ず出力する」契約が嘘になり、`--include-workspace-homes` は
   完全な no-op になる。よって `unionResources` は
   `boid.workspace_home_install_id=<id>` を filter にした 2 本目の `VolumeList` を
   **常に** (既定モードでも) 発行して union する。destroy するか skip するかは
   列挙後に `WorkspaceHomePolicy` が決める
2. `container_backend.go:1319` の `reapOrphanVolumes` — 発火条件は `ReapOrphans` が渡す
   list filter = **`boid.job_id` label の presence** (:1248)。そのうえで
   `reapOwnsLabels` (:1312) が install_id 一致を確認する。つまり job_id label を付けなければ
   そもそも列挙されない (初版はここを「install_id label を付けた時点で対象」と書いていたが
   不正確だった。install_id 単独で死ぬのは経路 1 の方)
3. `container_backend.go:1565` の `ensureNamedVolumes` — named volume を作る既存経路だが、
   **job の label 一式 (`boid.job_id` 含む) を付けて `VolumeCreate` する**。しかも
   `boid.job_id` label が無い volume には「ReapOrphans's volume sweep will not find it」と
   `slog.Warn` を出す。永続 volume ではこの warning は**正しい状態**なので、
   警告条件の見直しも必要になる。なお同関数の doc comment (:1557) は
   「既存 volume に request の label を適用しない」と明記しているので、
   `resolveWorkspaceHome` 側で正しい label 付きに先行 `VolumeCreate` しておけば、
   Launch 側の後追い呼び出しは label 面では無害になる
4. **`internal/sandbox/dockerproxy/reap.go:67`** — job 終了時の `dockerproxy.Reap` は
   per-job ledger に記録された volume を **label を一切見ずに name で DELETE** する
   (`reapDelete(ctx, client, "/volumes/"+id)`)。`reap.Run` の ledger union (`reap.go:250`) も同様。
   **label 設計だけでは防げない経路**である点が 1-3 と決定的に違う。
   `capabilities.docker` を持つ job の sandbox 内 docker client が
   `docker volume create boid-ws-home-<installID8>-<slug>` を発行すると、
   volume 名は決定的で予測可能なため、既存 volume に対する create が冪等成功しつつ
   **ledger に載り**、job 終了時に消される。`dockerproxy/policy.go` は volume の
   mount オプションは検査するが **volume 名は検査していない**

5. **`internal/sandbox/dockerproxy/policy.go` の `POST /volumes/prune`** — PR1 実装時に発見した
   5 つ目の経路。`isAllowedMutating` の POST allowlist に載っていて **無条件 allow** だった。
   `docker volume prune` は「どのコンテナにも使われていない volume」を消すので、
   **その job が使っていない他 workspace の HOME volume が確実に巻き込まれる**。
   リクエストに名前が乗らないので prefix 判定では防げず、endpoint ごと塞ぐしかない

6. **`internal/sandbox/dockerproxy/policy.go` の `POST /containers/create` の
   `HostConfig.Mounts[].Source`** — PR1 codex レビューで発見。`mountSpec` は
   `Type` と `VolumeOptions` しか見ておらず **`Source` フィールドを持っていなかった**。
   よって sandbox 内 docker client が
   `{"Type":"volume","Source":"boid-ws-home-<installID8>-<slug>","Target":"/victim"}` を
   mount した使い捨て container で `rm -rf /victim/*` を実行すると通る。
   **volume object は消えないので経路 4 の create/delete deny を一度も経由しない**が、
   中身 (認証情報 + toolchain) は全滅する。対策は `Source` が予約 prefix なら deny。
   `Type` の値には**依存させない** — engine 側の Type 正規化 (未指定時の既定、
   大小文字、docker/podman 差分、将来の type 追加) を policy が正確にモデル化する必要が
   出た時点でそこが bypass になるため。`policy.go` は既に
   parser differential (`TestParserDifferential_*`) を脅威モデルに置いている

7. **同 `POST /containers/create` の `HostConfig.VolumesFrom`** — PR1 codex レビューで発見。
   構造体に**フィールドすら無く parse されていなかった** (= 無条件 allow)。
   `--volumes-from <container>` は他 container の mount 一式を丸ごと継承するので、
   PR6 以降 job container 自身が HOME volume を mount するようになると経路 6 と同じ破壊が成立する。
   **リクエストに乗るのは container 名なので prefix 判定は原理的に不可能**
   (継承元が何を mount しているかは policy から検証できない。engine に inspect を投げれば
   `CheckRequest` の純関数性が壊れるうえ TOCTOU になる)。よって **非空なら deny**。
   sandbox 側の実害は無い — 自分の named volume を複数 container で共有したいなら
   `Mounts` で書けばよく、そちらは従来どおり allow

経路 4 / 5 / 6 / 7 の対策は label ではなく名前空間の予約と endpoint 封鎖になる:
dockerproxy policy で予約 prefix (`boid-ws-`) に対する volume create / delete /
**mount source** を deny し、`POST /volumes/prune` と `HostConfig.VolumesFrom` は
検査不能なので丸ごと deny、
防御として `dockerproxy.Reap` と `reap.Run` の volume ループにも name-prefix 除外を入れる。

なお **`PUT /containers/{id}/archive`** (`docker cp` の書き込み方向) と
**legacy `POST /containers/{id}/start` の `HostConfig`** は経路 6 と同じ形の攻撃になりうるが、
前者は `isAllowedMutating` が PUT を一切許可しない fail-closed、
後者は `checkContainerStart` が非空 `HostConfig` を無条件 deny で、いずれも元から塞がっている
(PR1 でそれを pin するテストだけ追加した)。

**決定 (PR1 で確定・実装済み)**:

| 項目 | 確定値 |
|---|---|
| 単一出典 package | **`internal/dockerres`** を新設 (import ゼロの leaf)。`dispatcher → reap → dockerproxy` の 3 者すべてが import できる。既存の「定数を重複定義して doc comment で drift を戒める」流儀は撤去 |
| workspace HOME volume の label | `boid.workspace_home=<slug>` + `boid.workspace_home_install_id=<installID>` の **2 つのみ**。`boid.job_id` / `boid.install_id` / `boid.workspace` は**付けない** (それぞれが既存の列挙 filter) |
| 予約 prefix (広い) | `boid-ws-` = `dockerres.ReservedVolumeNamePrefix`。**dockerproxy の volume deny 判定のみ**に使う。workspace *network* 名と同じ prefix なので network には適用しない |
| 永続 prefix (狭い) | `boid-ws-home-` = `dockerres.WorkspaceHomeVolumePrefix`。**reap 除外の判定に使う**。workspace network は再生成可能なので reap が消してよい |
| volume 名 | `boid-ws-home-<installID8>-<sanitized-slug>` (`dockerres.WorkspaceHomeVolumeName`)。PR1 では誰も呼ばないが、予約 prefix と実際の命名が乖離しないよう同じ場所に置いた |
| `boid reap` の契約 | **既定で workspace HOME volume を残す**。`--include-workspace-homes` で明示的に消せる。skip した volume は `skipped volume <name> (workspace HOME; use --include-workspace-homes to destroy)` として `nothing to reap` より前に出力する。`Report.Empty()` の定義は変えない (skip は destroy ではない) |
| `reap.Run` の volume 列挙 | `boid.install_id=<id>` と `boid.workspace_home_install_id=<id>` の **2 本の `VolumeList` の union**。2 本目はモードに関わらず**常に**発行する (列挙しないと skip を報告できない)。`installID` が空のときの流儀は 1 本目と同じ (term は空値で出す) |
| 保護判定の非対称性 | 名前判定 (`IsWorkspaceHomeVolumeName`) は `reap.Run` / `dockerproxy.Reap` / `reapOrphanVolumes` 共通。label 判定 (`reapOrphanVolumes` のみ) は **key の presence** で見る (`_, ok := labels[...]`)。値は workspace slug なので空文字になりうる (slug 未設定の DI 経路)。値の非空判定にすると doc と実装が食い違い、その分だけ保護から漏れる |
| startup reap | `container_backend.go` の `ReapOrphans` からの `reap.Run` 呼び出しは**常に除外側** (`reap.PreserveWorkspaceHomes`)。daemon 再起動で認証が消えることは絶対に許さない |
| ledger に残った過去分 | skip した volume は destroy していないので `destroyed` map に入れず、ledger からも drain しない。経路 4 の deny により **新規に ledger へ載ること自体が起きなくなる**ので、残るのは PR1 以前の分だけ。無害 (skip されるだけ) で、job の runtime dir が GC されると消える |

**Phase 4 の「homes/ は GC 対象外」契約をここで再度落とさないこと。**

### 論点 a-2: workspace remove 連動の削除とサイズ可視化の rewiring

Phase 4 の契約は「GC 対象外」と対で「**掃除は workspace remove 連動のみ**」
(`home-workspace-volume.md:118-119`) である。その実装は `internal/api/workspace_homes.go` にあり、
すべて host path 前提なので volume 化で機能しなくなる:

- `deleteWorkspaceHome` (:263) — `DELETE /api/workspaces/{slug}` が `os.RemoveAll(info.Path)` を呼ぶ。
  volume 化後は stat が ENOENT → `Exists=false` → **silent no-op**。
  workspace を消しても認証情報入りの volume が誰にも消されず**永久に orphan 化**する
- `computeWorkspaceHomeSize` / `ListWorkspaceHomeSizes` (:94-196) —
  `GET /api/workspaces/{slug}` のサイズ表示と `POST /api/gc` の orphan 検出も全滅
  (全 workspace が `Exists=false` になる)

**rewiring 方針**: 削除は `VolumeRemove`、orphan 検出は volume 列挙と workspace DB row の差分。

サイズは注意が必要で、`Volume.UsageData` は **`GET /system/df` (DiskUsage) の応答でのみ
populate され、`VolumeInspect` では省略される** (moby の型定義に基づく。オフラインのため
一次情報未確認 — 実装前に裏取りすること)。しかも system df は engine 全体を走査するため重い。
使い捨て container 内で `du` を取る方式も選択肢として比較すること。

### 論点 b: marker / lock の置き場

現行は `homesDir/<slug>.init.json` と `homesDir/<slug>.lock` (= workspace HOME と同じ親)。
volume 化すると daemon から直接読み書きできなくなる (volume の中身は container 越しにしか触れない)。

**方針**: marker / lock は daemon 自身の永続領域 = `boid_state` volume 内
(`/home/boid/.local/share/boid/homes-meta/<slug>.{init.json,lock}`) に置く。
daemon は自分の volume なので普通の file I/O で扱え、flock もそのまま使える。

注意: 現行の `acquireWorkspaceHomeLock` の flock 意味論をそのまま維持すること
(PR #787 の TOCTOU fix — lock 取得後に script を再読込・再ハッシュする二重チェック)。

**決定 (PR2 で確定・実装済み)**:

| 項目 | 確定値 |
|---|---|
| 置き場 | `<dataHome>/homes-meta/<slug>.{init.json,lock}`。dir は 0700 で `MkdirAll`。`homes/` と対になる命名 (`grep` で対応関係が読める) |
| `<dataHome>` の出所 | `internal/server/wire.go` の `dataHomeFor(cfg)`。実デプロイ (`cfg.DBPath` が実ファイル) では `filepath.Dir(cfg.DBPath)` で、`boid.db` / `web_secret` / `install_id` / `secret.key` / `tls/` / `kits/` / `skills/` と同じ dir、container デプロイでは `boid_state` volume の中。ただし `dataHomeFor` は**それだけの関数ではない**: DB が `:memory:` / 空なら `filepath.Dir(cfg.SocketPath)` に落ち、両方空なら `""` を返す (下段参照) |
| `dataHomeFor` が `""` を返したとき | **`dataHomeFor` の空値の意味は呼び出し側ごとに違う**。attachments (`projectSvc.DataDir`) / `orchestrator.ReposRoot` は「feature disabled」と読むが、`WireConfig.DataHomeDir` は読まない — dispatcher 側の `workspaceHomeMetaDir` が `$XDG_DATA_HOME/boid` へ fallback する (下段)。`dataHomeFor` の doc comment は元々「callers should treat that as feature disabled」とだけ書いていて PR2 の新しい読み方と食い違っていたので、両方の読み方を明記するよう修正した |
| **新設した配線 seam** | `dataHomeFor(cfg)` → `dispatcher.WireConfig.DataHomeDir` → `Runner.DataHomeDir` → `workspaceHomeMetaDir()`。**`Runner` は daemon の永続領域を知らなかった** (持っていたのは `RuntimesDir` だけ) ので、本 PR は「位置替えのみ」ではなく**配線が 1 本増える** |
| bare `Runner{}` の fallback | `DataHomeDir` が空なら `workspaceDataHomeRoot()` = `$XDG_DATA_HOME/boid`。`WorkspaceHomesDir` が同じ理由で持っている fallback と同型。**ただしこの fallback は「実ユーザの `$HOME` を汚さない」保証にはならない** — 汚さないのは `internal/dispatcher` に `TestMain` があるからで、fallback 自体は ambient env を見るだけ。実際 `internal/server` は bare `&Runner{}` で `Dispatch` していて `TestMain` を持っていなかったため、`go test ./internal/server/` が実ユーザの `~/.local/share/boid/` を汚し、`~/.config/boid/workspaces/default/init.sh` を**実行**していた。対策は `testutil/homeenv` + 各 package の `TestMain` (後述) |
| init.sh の一時ファイル | **`homes-meta` 側へ一緒に移す**。`runWorkspaceInitScript` の doc comment は「一時ファイルの置き場は lock-serialized」を TOCTOU 保証 (PR #787) の根拠にしているので、lock だけ移すとその前提が嘘になる |
| 旧 marker | **移行も削除もしない**。読まないだけ。結果として upgrade 後の初回 dispatch で init.sh が 1 回だけ再実行される (init.sh は冪等が契約)。この一連の挙動は `TestResolveWorkspaceHome_LegacyMarkerBesideHomes_NotReadNotDeleted` で pin 済み |
| `homes/` 側の残骸 | `ListWorkspaceHomeSizes` の `IsDir()` フィルタは**残す**。旧 marker / lock が残っている環境で workspace として誤検出しないため (テスト名を `..._IgnoresLegacyMarkerAndLockFiles` に改名し、意味を「同居ファイルの除外」から「レガシー残骸の除外」に更新) |
| 改竄耐性の根拠 | 「home dir の**外**だから」は**新旧どちらの置き場でも成立する** (旧 `homes/<slug>.init.json` も mount の外) ので、移設の理由としては使わない。移設の理由は PR6 で home が volume 化しても daemon が普通の file I/O + flock を保てること。**「daemon の領域だから job から一切見えない」とは主張しない** — 下の「未解決」節の `boid_state` volume mount 参照 |
| marker ↔ 実体の identity 突合 (nonce) | **本 PR に含める**。理由は下記 |
| テスト隔離 | `testutil/homeenv` を新設し、`internal/dispatcher` / `internal/server` / `cmd` の `TestMain` を一本化。`homeenv.AssertIsolated` を各 package に常設ガードとして置く |

**この PR で挙動が変わる点**: (a) marker / lock / init.sh 一時ファイルの**置き場**、
(b) marker が **home の identity (nonce) を記録し、突合が取れないと init を再実行する**。
(a) から派生して、旧 marker が読まれなくなるため upgrade 後 workspace ごとに init.sh が 1 回だけ再実行される
(nonce を持たない marker も同じ扱いなので、この再実行は 1 回に統合される)。
それ以外 (workspace HOME 実体の位置、init.sh の実行環境、flock の意味論) は不変。
marker の JSON には `home_id` フィールドが 1 つ増える (`omitempty`、旧 marker は読めるまま)。
なお `homes/` はこの時点でも `RuntimesDir` 由来のまま (= container backend では tmpfs) — volume 化は PR6。

**marker と実体が別ライフサイクルになる副作用を、同じ PR で潰す。** 移設前は marker と home dir が
同じ親ディレクトリにあり、揮発するときも同時だった。移設後は marker だけが生き残る状況が生じる。
発生条件は PR6 を待たない — **PR2 の時点で既に成立している**:
`homes/` は `RuntimesDir` 由来 = `BOID_RUNTIME_DIR` = tmpfs、`homes-meta/` は `boid_state` volume なので、
**ホスト再起動のたびに「HOME だけ消えて marker が残る」**。PR6 以降はこれに加えて
手動 `docker volume rm`、論点 a の reap 誤爆、workspace remove の半完了が乗る。
そのとき次の dispatch は marker を見て init を skip し、**空の HOME で job が走る**。実際に出るエラーは
adapter の「CLI not found」(`internal/adapters/claude/run.go:43`) で、これは init.sh 未整備を
指す文面なので真因 (HOME の消失) に誘導しない。

対策: init 完了時に **home 内へ crypto/rand 由来の nonce ファイル
(`<homeDir>/.boid-workspace-home-id`) を書き、同じ値を marker の `home_id` にも記録する**。
skip 条件を「marker の hash が一致」から「**hash 一致 かつ nonce ファイルが存在し marker の記録と一致**」に変える。
不一致・不在・`home_id` 無し (旧 build の marker) はすべて **fail-safe 側 = 再 init**。

**nonce が検出できるもの / できないもの** (codex review 2 巡目で初版の記述を訂正):

| | 内容 |
|---|---|
| **検出できる** | **事故による desync**。ホスト再起動での tmpfs 消失、`docker volume rm`、論点 a の reap 誤爆、workspace remove の半完了、別 incarnation のバックアップからの復元。いずれも nonce ファイルごと消える / 別物に置き換わるので、検出はこのクラスに対して完全に機能する。**これが nonce を入れた理由そのもの** (= marker と実体の寿命を分離したことの埋め合わせ) |
| **検出できない** | **意図的な細工**。job は自分の `$HOME` 内の nonce を**読める**ので、値を控えたまま home の中身を消し、nonce だけ書き戻せば skip を誘発できる |

初版はここを「job は marker 側 (自分の `$HOME` mount の外) を読めないので、skip を誘発する値を
偽造できない」と書いていたが、**これは誤り**。marker を読む必要も偽造する必要もなく、
自分の home にある nonce をそのまま replay すればよい。

**意図的な細工に対しては防御しない。** これは構造的な限界であり、機構を足しても解決しない:
nonce は home と同じ寿命を持つ必要があるので home 内に置くしかなく、home は job が rw で所有する。
そこに置いた何であれ job には読めて書き戻せる。そして脅威モデル上の実害も小さい —
この攻撃の成果は「同じ workspace の次の job が壊れた home で走る」ことだけで、
**job は nonce の有無に関係なく今でも自分の home の中身を自由に消せる**。
単一ユーザのパーソナルオーケストレータにおける自傷の範疇であり、nonce は事態を悪化させていない。

**ただし daemon を巻き込ませてはいけない。** ここだけは自傷では済まず、別タスクの dispatch が
巻き添えになる。これは nonce の「値」ではなく「読み方」の問題で、初版の `readWorkspaceHomeNonce` は
素の `os.ReadFile` だったため、job が終了時に細工を残すだけで 2 経路の DoS が成立していた
(codex review 2 巡目、Blocker):

- nonce を **FIFO に置換** → 次の dispatch が `open(2)` で**無期限にブロック**する
  (job の container が消えた後、書き手は永久に現れない)。再 init にすら到達しない
- nonce を **`/dev/zero` 等への symlink** に置換 → daemon が OOM まで読み続ける

対策として読み取りを `O_NOFOLLOW` (symlink を辿らない) + `O_NONBLOCK` (FIFO の open で待たない) +
`fstat` による regular file 確認 + 1 KiB のサイズ上限に変更した。いずれかに引っかかったら
**「nonce 無効」= 再 init** (fail-safe 方向は維持) とし、定常遷移ではないので `slog.Warn` を出す。
`internal/skills/safe_deploy.go` の openat2 + `RESOLVE_NO_SYMLINKS` による全 component の
再歩行までは採らない — あちらは job が差し替えられる**サブディレクトリ**に書くのに対し、
こちらで攻撃者が制御できるのは最終 component だけ (その上は mount point 自身とその親で、
job は自分の `$HOME` の中からそれらを rename できない) だから。
テストは `TestReadWorkspaceHomeNonce_RejectsUnsafeFiles` /
`TestResolveWorkspaceHome_NonceReplacedByFifo_DoesNotWedgeNextDispatch` /
`TestResolveWorkspaceHome_NonceReplacedBySymlink_NotFollowed` で pin。

**この読み取りを入れた上で初めて**「改竄の代償は冪等な init.sh の 1 回余分な実行だけ」という
記述が真になる。以上の整理は `workspace_home.go` の `workspaceHomeNonceFileName` /
`workspaceHomeMarker.HomeID` / `workspaceHomeInitialized` / `readWorkspaceHomeNonce` の
doc comment にも同じ内容で書いてある。

**nonce は init.sh の実行有無に関わらず書く。** Phase 4 の契約では script を持たない workspace も
marker だけは打たれる (`home-workspace-volume.md:98`) ので、条件分岐させるとそのクラスにだけ
突合が効かなくなる。PR5 で init.sh を container 実行に移す際は、この書き込みが
次節の prep ステップに移る (prep を条件付きにしてはいけない理由も同じ)。

**割り当てを PR5/PR6 から PR2 に前倒しした理由**: nonce は「marker と実体の寿命を分離した」ことの
埋め合わせであり、**分離を行う PR がその埋め合わせを負う**のが筋。PR5/PR6 に置くと、
上に書いたとおり PR2 landed 直後からホスト再起動のたびに窓が開き、PR6 まで開きっぱなしになる。
初版は nonce を「volume の identity」と捉えていたため volume 化の PR に置いていたが、
実際に必要なのは volume ではなく **home ディレクトリの incarnation の identity** で、
これは PR2 の時点で既に必要になっている。

### 論点 b-2: volume prep ステップ (init.sh の有無に関わらず必ず 1 回走らせる)

**volume 作成後、init.sh の有無に関わらず prep container を 1 回走らせる。** 仕事は 2 つ:

1. **skeleton の mkdir** (`~/.claude`、`~/.claude/skills`、`~/.local/bin` など) を **uid 1000 で**作る
2. **nonce の書き込み** (論点 b)。**nonce 機構そのものは PR2 で導入済み**なので、ここで新規に
   設計するのではなく「daemon プロセスの `writeWorkspaceHomeNonce` による直接書き込み」を
   「prep container 内での書き込み」に**移設する**のが PR5 の仕事になる。
   marker 側 (`home_id`) は daemon の永続領域に残るので変わらない

**1 が必要な理由 (実測)**: job container は `$HOME/.claude/skills` に skills を重ねる
(論点 e-2)。このとき bind target が volume 内に存在しないと、**engine が中間パスを
uid 0 所有で自動作成する**。空 volume を `/home/boid` に、host dir を
`/home/boid/.claude/skills` に mount した container を podman 4.9.3 で起動して実測した結果:

```
drwxr-xr-t 3    0    0 4096 Jul 26 05:24 .claude      ← root 所有で自動生成
drwxrwxr-x 2 1000 1000 4096 Jul 26 05:24 .claude/skills
```

`~/.claude` が root:root になるため、uid 1000 で走る harness は
`~/.claude/.credentials.json` も `~/.claude/projects/*.jsonl` も**書けない**。
自動作成は container start 時 (entrypoint 実行前) に engine が行うので、runner 側の
コードでは防げない。**認証永続を守るための設計が、そのままでは認証を書けなくする。**

**2 が必要な理由**: Phase 4 の契約は「script が無い workspace は素通し (マーカーだけ打つ)」
(`home-workspace-volume.md:98`)。prep を init.sh 有無で条件分岐させると、
素通し workspace には nonce を書く主体が存在せず、論点 b の desync 対策が
そのクラスに適用できない。

**契約の更新**: 素通しの文面を「container 実行なし」ではなく
「**script 実行なし (prep は必ず行う)**」に改める。init.sh を持つ workspace では、
prep を init container の先頭に boid が挿入する builtin prelude として統合すればよい
(container 起動は 1 回で済む)。

### 論点 c: init.sh の実行経路

現行は daemon プロセスが `/bin/bash <tmpfile>` を直接 exec し、`HOME` を workspace HOME に
差し替えて走らせる (host 側 trusted 実行)。これが init.sh の契約:

> 実行環境: ホスト側 (trusted) で boid が dispatch 前に呼ぶ

案 A ではこれを **workspace volume をマウントした使い捨て container** に置き換える。
DooD の仕組み (job container 生成) は既にあるので流用できる。

- image: `boid-runner:latest` (job と同じ)
- mount: workspace volume → `/home/boid`
- env: `buildWorkspaceInitEnv` 相当 (HOME / BOID_WORKSPACE_SLUG / BOID_WORKSPACE_HOME +
  PATH 等の allowlist)。ただし **host の PATH を引き継ぐ意味が無くなる** ので、
  container の PATH に読み替える必要がある
- script の渡し方: **stdin (`bash -s`) に一本化する**。daemon container 内の一時ファイルは
  host engine から見えないので sibling container の bind source にできない
  (compose.yml の KNOWN GAP 節と同じ構造)。素朴に `os.CreateTemp` すると詰まる
- 出力: 現行同様に集約して `slog` へ。失敗時は exit code と tail をエラーに含める
- **network**: 明示的に決めること。job container は per-workspace の internal network
  (`internal: true`、egress は allowlist proxy 経由のみ) に閉じ込められるが、init.sh の
  主目的は toolchain のダウンロード (実測 1.5GB: go / node / volta / claude / codex / opencode) で、
  その allowlist 下では installer がほぼ全滅する。無制限 egress を持つ network に置くのが
  妥当だが、それは「trusted 境界は維持される」の一部として明文で宣言すべき事項であって、
  暗黙に決めてよいことではない。**どの network に attach するかを実装者が一意に読めるよう
  書くこと** — 「compose project の default network (`boid_default`) に
  `docker network connect` する」のか「engine の既定 bridge に置く」のかで指定が変わる
- **多重実行の防止**: flock はプロセス死で解放されるが、container は親プロセスの死を生き延びる。
  daemon が crash して再起動した場合、次の dispatch が再 init を始めた時点で**旧 init container が
  まだ volume に書いている**可能性がある。Phase 4 の「同時 dispatch でも実行は 1 回」契約
  (`home-workspace-volume.md:83`) が破れる。決定的な container 名
  (例 `boid-ws-init-<installID8>-<slug>`) を name 衝突による相互排除に使い、
  起動前に既存 init container を検出して待つか殺すこと
- **init container の label**: `boid.job_id` を付けてはいけない。`ReapOrphans` の
  `ReapedJobIDs` → MarkStale / auto-reopen の job 会計を汚染する
  (`container_backend.go:1261` 付近)。かといって無 label だと誰も掃除しないので、
  専用 label と掃除規則を論点 a と揃えて決めること

**trusted 境界は維持される** — script を選ぶのも container を起こすのも daemon であり、
sandbox 化された job の中で走るわけではない (A' との違いはここ)。ただし
「ホスト側で実行」という文面は「daemon が制御する専用 container で実行」に更新が要る。
`home-workspace-volume.md` の init.sh 契約節と、リファレンス実装 init.sh の
冒頭コメントの両方を直すこと。

### 論点 d: init.sh 自身の置き場 (別課題だが密結合)

dogfood でもう 1 件判明: **workspace の init.sh を volume に入れる CLI 経路が存在しない**。
`workspaceInitScriptPath` は daemon の `os.UserConfigDir()` を見るので、実際に読まれるのは
container 内 `/home/boid/.config/boid/workspaces/<slug>/init.sh` (volume 内) だが、そこへ
書き込む手段が無い (dogfood では `podman cp` で手動投入した)。

- host にある既存 5 workspace 分 (`default`/`boid`/`khi`/`ubs`/`bm-next`) の init.sh を移行できない
- `boid workspace export` の spec に `init_script` フィールドが無く、export/import でも運べない
- エラーメッセージが `~/.config/boid/workspaces/<slug>/init.sh` と表示するため、
  **host のパスに見えて誤誘導**する。同一文面が **3 箇所** にある:
  `internal/adapters/claude/run.go:50` / `codex/run.go:37` / `opencode/run.go:37`

同じクラスの問題が `host_commands.yaml` にもある。`boid host-commands` は `list` と `reload`
だけで、reload の説明は「hand edit した後に」だが volume-only では hand edit できない。
daemon 側は `host_commands: {}` の空で、host 側の定義 (atl/az/board 等) が移行されていない。
さらに host_commands の `path` は `/home/nosen/go/bin/atl` のような **host のバイナリパス**であり、
daemon が container 内にいる以上そこに実体が無い。**host_commands 機構が volume-only で
どう成立するのかは設計レベルで未解決**であり、本 doc の範囲外として別途扱う。

本 doc では init.sh のみ扱う。方針は 2 つ併用:
1. `boid workspace export/import` の spec に `init_script` を含める (移行と backup)
2. 専用 CLI (`boid workspace set-init-script <slug> -f <file>` / `get-init-script`) を追加 (日常編集)

`config.yaml` が `boid config get/set/apply/edit` を得たのと同じ扱いにする
(`volume-only-daemon.md` §論点f が確立した「daemon 所有の file config は HTTP API 越しに読み書きする」原則)。

### 論点 e: named volume mount の実現経路 — 既存機構が使える

**注意: 初版はここを「`MountType` に volume 型の追加が要る」と書いていたが事実誤認だった**
(Fable レビュー指摘)。`internal/sandbox/types.go` の `MountType` に volume が無いのは事実だが、
realization 層は既に 3-way 分類を持ち、**named volume を end-to-end で実現できる**:

- `internal/sandbox/realization/realization.go:285` — `classifySource` は Source が
  `/` で始まらない場合に `MountSourceNamedVolume` を返す
- `internal/dispatcher/container_backend.go:1524` — `containerMounts` が
  `mount.Mount{Type: mount.TypeVolume, Source: <name>}` に変換する
- 同 :1565 — `ensureNamedVolumes` が `VolumeCreate` (label 付き) まで実行する

さらに `realization.go:65-73` の doc comment は、この kind が**まさに workspace HOME のために**
先行実装されたことを明記している:

> Named-volume HOME is a Phase 7 follow-up. The kind exists now so Realize's classification is
> complete and so PR5 / Phase 7 can opt a mount into it **without changing this package**.

**したがって選択は「新しい型を足すか」ではなく、以下のどちらか**:

- (i) 既存の「Source が相対パスなら named volume」規約に乗る。`homeMounts`
  (`sandbox_builder.go:979`) が volume 名を Source に入れて `MountBind` を返すだけで済む。
  実装は最小だが、相対パスの偶発的な混入が named volume として誤分類されるリスクが残るので
  guard が要る
- (ii) 明示的な `MountVolume` 型を足し、既存の暗黙規約と併存させるか置き換える。
  意図は明確になるが、`classifySource` の分類と両立させる設計が要る

**決定 (PR1): (i) を採用。新しい `MountType` は足さない。**
根拠は上に引用した `realization.go:65-73` の doc comment がまさにこの用途を想定して
先行実装されたと明記していること。guard は `ensureNamedVolumes` に入れた —
各 name を `dockerres.IsValidVolumeName` (`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`) で検証し、
不正なら **Launch を fail-closed で落とす** (エラー文面に問題の Source 名を含める)。
`Realize` はエラーを返さないシグネチャなのでそこは変えていない。

`sandbox_builder.go` の `homeMounts` は PR1 では触っていない (volume 化は PR6)。

`ensureNamedVolumes` の label 問題 (論点 a) は PR1 で対応済み: name ごとに label を出し分け、
workspace HOME volume には論点 a の 2 label のみを付ける。
「`boid.job_id` label が無い volume」への `slog.Warn` も workspace HOME では出さない
(label が無いのが**正しい状態**なので、warn するとノイズになって本来の警告が無視される)。

userns backend は撤去済みなので、bind 版の workspace HOME 経路を残す必要はない
(`workspaceHomeDir == ""` の tmpfs fallback は、workspace 未解決時の挙動として維持)。

### 論点 e-2: embedded skills sync が workspace HOME に直接書いている

`internal/dispatcher/runner.go:377` は **dispatch のたびに**
`skills.DeployAll(workspaceHomeDir + "/.claude/skills")` で workspace HOME へ直接 file I/O し、
失敗すると dispatch 自体を fail させる (Phase 4 PR3 で、adapter の bind-mount 経路を
retire した代わりに入った経路)。

volume 化すると daemon はこのパスに書けなくなる。放置すると
「存在しない host dir に書いて成功し、job には skill が届かない」silent drift か、
書き込みエラーで全 dispatch 停止のどちらかになる。**論点 b の marker/lock と同列の
「daemon が workspace HOME に書く経路」であり、同時に解決しなければならない。**

毎 dispatch に使い捨て container を起こすのはコスト的に非現実的なので、方針候補:
embedded skill は boid バイナリから常に再生成できる (揮発してよい) 性質を使い、
host-visible runtime dir (`hostVisibleRuntimesDirFor` 配下) に materialize して
job container に `$HOME/.claude/skills` へ RO で重ねる。

**この方式は論点 b-2 の prep ステップとセットでなければ成立しない。** bind target
`~/.claude/skills` が volume 内に存在しないと engine が `~/.claude` を uid 0 所有で
自動作成し、認証情報が書けなくなる (実測結果は論点 b-2 を参照)。prep で
`~/.claude` と `~/.claude/skills` を uid 1000 で先に作っておくこと。

### 論点 f: 既存 workspace HOME の移行

dogfood 環境では認証済み workspace HOME (1.5GB) を
`~/.local/share/boid/homes-backup-20260726/default` に退避済み。

新方式に移る際、既存の host 側 `homes/<slug>` から volume へ内容をコピーする経路が要る。
使い捨て container に volume をマウントして `tar` で流し込む形になる
(daemon から volume に直接書けないため)。

**ただし podman rootless の uid mapping を跨ぐ点に注意** — これは pivot の発端そのものと
同じ罠である (`volume-only-daemon.md` の incident 「壁 2」)。backup dir を engine bind で
container に見せると、container 内 uid 1000 ≠ host uid 1000 (subuid mapping) のため
`.credentials.json` (0600) が読めない。逆に container uid 0 (= host uid 1000) で読むと、
volume 内に作られるファイルの所有者が job container の実行 uid とずれる。

**方針**: bind mount を使わず **tar を stdin で流し込み**、展開側で uid を正規化する
(`--no-same-owner` 相当)。`boid.json` などの 0600 ファイルの mode は維持すること。

**この方式は host mode CLI からの実行に限定される。** tar の生成側が host dir を読める必要があり、
container 内 daemon は起動時に backup dir に到達できない (到達させるには本 doc が
uid mapping を理由に退けた engine bind が要る)。したがって「起動時の自動移行」を
選ぶなら、bind + container uid 0 での読み取り + 展開時 chown 1000 という別の変種になる。

一度きりの移行なので **CLI (`boid workspace import-home <slug> --from <dir>`) に限定するのが素直**。

## PR 分割案

| PR | 内容 | 挙動変化 | 依存 |
|---|---|---|---|
| 1 | **[landed]** volume 破壊経路 **7 つ**の封じ込め: 論点 e = (i) 採用 + `internal/dockerres` 新設 (label / prefix / 命名の単一出典) + `ensureNamedVolumes` の label 分離・warning 条件見直し・volume 名 validation + `reap.Run` の **2 本立て volume 列挙**・name 除外・`WorkspaceHomePolicy` + `boid reap --include-workspace-homes` 契約宣言 + `reapOrphanVolumes` の name/label 二重除外 (label は presence 判定) + dockerproxy policy の `boid-ws-` volume create/delete deny・**`/volumes/prune` deny**・**`Mounts[].Source` deny**・**`VolumesFrom` deny** + `dockerproxy.Reap` 側 name-prefix 除外 | sandbox 内 `docker volume prune` と `docker run --volumes-from` が 403 になる (それ以外は無し — 対象 volume がまだ存在しない) | — |
| 2 | **[landed]** marker/lock **+ init.sh 実行用一時ファイル**を daemon 永続領域 (`boid_state` = `dataHomeFor(cfg)`) の `homes-meta/` へ移設。**`dataHomeFor(cfg)` → `WireConfig.DataHomeDir` → `Runner.DataHomeDir` の配線を 1 本新設** (`Runner` は永続領域を知らなかった)。**＋ 論点 b の nonce (marker `home_id` ↔ `<homeDir>/.boid-workspace-home-id` の突合) を同 PR で導入** (初版は PR5/PR6 に割り当てていた。移した理由は論点 b 参照)。**＋ nonce の読み取り安全化** (`O_NOFOLLOW` / `O_NONBLOCK` / regular file 確認 / 1 KiB 上限。job が nonce を FIFO や symlink に置換して daemon を止められた経路を塞ぐ — 論点 b 参照)。**＋ `testutil/homeenv` によるテスト隔離** (`internal/dispatcher` / `internal/server` / `cmd` の `TestMain`) | 置き場 + 突合。**旧 marker (置き場も `home_id` も違う) は読まないので upgrade 後 workspace ごとに init.sh が 1 回だけ再実行される** (冪等前提で吸収)。旧 marker の削除はしない。以後は **HOME が消えれば marker があっても再 init される** | — |
| 3 | skills の materialize 先を host-visible runtime dir へ + `$HOME/.claude/skills` へ RO overlay | 無し (現行の host bind HOME でも同じく機能する) | — |
| 4 | `WorkspaceSlug` を独立に thread する (`runner.go:724` の `filepath.Base` 依存を切る) | 無し | — |
| 5 | init.sh + prep を使い捨て container 実行へ (network / stdin / 多重実行防止 / prep skeleton + **nonce 書き込みの移設**: 機構は PR2 で入っているので、書く主体を daemon プロセスから prep container に移すだけ)。**この時点では現行の host-visible homes dir を engine bind して検証する** | init.sh の実行環境が変わる | 3 |
| 6 | **[不可分コア]** `resolveWorkspaceHome` を volume ベースへ + `homeMounts` の volume 化 + 契約 doc 更新 (nonce 突合は PR2 で導入済み。ここでは nonce の読み書きが volume 越しになることの確認のみ) | **workspace HOME が volume になる** | 1,2,4,5 |
| 7 | workspace remove 連動の削除・サイズ可視化・orphan 検出を volume API へ rewiring (論点 a-2) | | 6 |
| 8 | 既存 homes の移行 CLI (`boid workspace import-home`、uid mapping を跨ぐ tar stdin) | | 6 |
| 9 | init.sh の CLI 経路 (export/import の `init_script` + 専用 CLI) | | — (独立) |

**分割の考え方**: 「daemon が workspace HOME に書く経路」は 3 つある (marker/lock、init.sh 実行、
skills sync)。**volume への切り替えを跨ぐ変更は 1 PR にまとめる必要がある**が、
PR2-5 はいずれも**現行の path ベース HOME のまま挙動を保って先行 land できる**ため、
最リスクの PR6 を「切り替えそのもの」だけに縮められる。

初版は 3+4 を「必要なら 1 PR」、第 2 版は 6 項目を「必ず 1 PR」としていたが、後者は過大だった
(Fable 再レビュー R-3)。`volume-only-daemon.md` が「巨大 1 PR は review 困難・CI failure の
切り分け困難」を理由に段階案を採った経緯とも整合しない。

**PR6→PR7 の中間状態**: PR7 が landed するまで `boid workspace remove` の home 削除は
silent no-op になり volume が残る。一時的に許容する (手動 `docker volume rm` で対応可能)。

初版にあった「PR1: `sandbox.Mount` に volume 型追加」は論点 e の事実誤認に基づいていたので削除し、
label / reap 側の封じ込めに置き換えた。

**PR1 で追加された sandbox 可視の挙動変化は 2 点**: `POST /volumes/prune` (論点 a 経路 5) と
`HostConfig.VolumesFrom` (経路 7) が 403 になる。いずれも**リクエストから対象 volume を
特定できない**ため prefix 判定では防げず、塞ぐしかなかった。
`VolumesFrom` の代替は `Mounts` での明示 mount で、そちらは従来どおり allow。
`/containers/prune` / `/images/prune` / `/networks/prune` / `/system/prune` は従来どおり allow
(いずれも volume に触れない。`/system/prune` は docker/podman のいずれの Engine API にも
実在しない — `docker system prune` は client 側で個別 prune に分解される — ので allowlist に
載っていても dead entry)。予約 prefix への volume create / delete / mount source も deny に
なるが、これは「そもそも sandbox が触るべきでない名前」なので実挙動の退行にはあたらない。

## 未解決 / 本 doc の範囲外

- **host_commands の volume-only 対応** (論点 d 後半)。container 内 daemon から host バイナリを
  どう呼ぶのか。broker 経由・廃止・image 同梱のいずれか、設計判断が要る
- **`boid web set-addr` / `set-url` の撤去** (dogfood で判明した silent no-op、
  `scopeLocal` のまま host の config.yaml を書いて成功表示する)
- **`notify.command` の host path 依存** (`/home/nosen/.local/bin/ntfy.sh` が container 内に無い)
- **`capabilities.docker` を持つ job から daemon の state volume が mount できる**。
  compose デプロイでは daemon の永続領域は `boid_state` named volume
  (compose project prefix 込みで `boid_boid_state`) で、`/home/boid` 全体にマウントされている。
  dockerproxy policy の volume 判定 (`dockerres.IsReservedVolumeName`) は
  **`boid-ws-` prefix しか予約していない**ので、job は
  `{"Type":"volume","Source":"boid_boid_state","Target":"/state"}` を mount した
  sibling container を作れる。読めるのは workspace home の init marker どころではなく
  **`secret.key` / `boid.db` / `tls/ca.key` / `web_secret` / `install_id` 一式**である。
  PR1 が塞いだのは「job が *workspace HOME volume* を壊す」経路であって、
  「job が *daemon 自身の state volume* を読む」経路ではない。
  **PR2 より前から存在し、PR2 の範囲を超えるので本 PR では塞いでいない。**
  したがって「marker は daemon の領域にあるから job から一切見えない」という主張は成立せず、
  `workspace_home.go` の doc comment も正確な範囲 (「job 自身の `$HOME` mount 経由では触れない」)
  に書き直してある。対策の方向としては予約 prefix を daemon の state volume 名まで広げる、
  あるいは volume mount source を allowlist 方式に反転させる等が考えられるが、
  **`capabilities.docker` の脅威モデル全体を見直す別件**として扱う
- k8s (Phase 7) での PersistentVolumeClaim への読み替え。named volume 前提の設計は
  そのまま PVC に対応付くはずだが、本 doc では扱わない
