# workspace HOME の named volume 化 (永続性退行の修復)

**状態**: 実装中 (2026-07-26 作成。PR1 / PR2 / PR3 / PR4 / PR5 / **PR6** / **PR7** landed、**PR8** 実装済み、PR9 未着手)
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

**PR7 で実装済み (2026-07-27)**。 PR6→PR7 の間だけ上記 2 点がそのまま発現していた
(`workspace remove` の home 削除は silent no-op、サイズは全 workspace で空、orphan は 1 件も
検出されない)。 中間状態の注記は `internal/api/workspace_homes.go` の冒頭と
`docs/{ja,en}/guide/workspace-home.md` から撤去した。

**機構**: 削除は `VolumeRemove`、サイズは `DiskUsage` (`GET /system/df`)、
orphan 検出は volume 列挙と workspace DB row の差分。
engine 呼び出しは `internal/dispatcher/workspace_home_volumes.go` の
`WorkspaceHomeVolumeStore` に閉じ、`internal/api` は方針 (予約 slug の保護 / orphan 判定 /
degrade の仕方) だけを持つ。 この分割は **`internal/api` が moby の型を一切 import しない**
状態を保つため (下表 D2)。

#### 実測 (rootless podman 4.9.3、read-only、2026-07-27)

| 確認事項 | 結果 |
|---|---|
| `GET /volumes/<name>` (VolumeInspect) の `UsageData` | **返らない** (フィールド自体が無い) |
| `GET /volumes` (VolumeList、label filter 有無問わず) の `UsageData` | **返らない** |
| `GET /system/df` の `UsageData` | **`{RefCount, Size}` が全 volume に付く** |
| `GET /system/df` の所要時間 | **0.47–0.72s** (images 469 / volumes 157 / containers 10 のホスト、warm) |
| `?type=volume` / `?verbose=1` の効果 | **podman 4.9.3 は両方とも黙って無視** — 3 パターンとも応答が 151995 bytes で byte-identical |
| `Size` の意味論 | **`du --apparent-size` と完全一致**。1MiB 実ファイル + 100MiB sparse + hardlink + symlink + 小ファイルの probe volume で **105906193 bytes**、mountpoint に対する `du -sb` と同値 |
| in-use volume の `DELETE /volumes/<name>` | **409** `volume is being used by the following container(s): <id>` |
| in-use volume の `DELETE /volumes/<name>?force=1` | **同じく 409** — force は in-use を剥がさない |
| 不在 volume の `DELETE ?force=1` / 非 force | **204** / **404** |
| in-use 時の `RefCount` | **1** |

#### 決定

| # | 論点 | 決定 | 根拠 |
|---|---|---|---|
| **D2** | `internal/api` から engine を叩く seam | engine 呼び出しは `dispatcher.WorkspaceHomeVolumeStore` (narrow interface `WorkspaceHomeVolumeAPI` = `VolumeInspect`/`VolumeList`/`VolumeRemove`/`DiskUsage`)。 `internal/api` は消費側 interface `WorkspaceHomeStore` を自前で宣言し、平データ `dispatcher.WorkspaceHomeVolume` だけを受け取る | `SelfContainerInspector` と同じ様式。 `internal/api → internal/dispatcher` は既存の向きで、逆向きは作れない (cycle) ので、データ型は dispatcher 側に置くしかない。 結果として **`internal/api` は moby を 1 つも import しない**まま |
| **D3** | サイズ取得の方式 | **`DiskUsage`(`Volumes: true, Verbose: true`) を 1 回**。 使い捨て container の `du` は不採用 | `UsageData` を返す endpoint は df だけ (実測)。 `du` container 案は volume ごとに container 起動 + image 依存 + init container と同型の失敗面を増やすのに、得られる数値は **df と同じ単位**。 engine 全体走査のコストは呼び出し元が `workspace show` / `workspace remove` / `boid gc` の 3 つだけで dispatch 経路に無いため許容 (0.47–0.72s 実測、`workspaceHomeVolumeTimeout` = 30s で bound) |
| **D3-a** | `Verbose` を付ける理由 | podman では無意味だが**必須** | moby client v0.5.0 は API >= 1.52 の分岐で `Verbose` が無いと `VolumesDiskUsage.Items` を捨てる。 podman は Ping が 1.41 を返すので legacy 分岐に落ち Items が無条件に入る = **dogfood では露見しない**。 実 docker で全サイズが unknown になる |
| **D3-b** | apparent size 契約の変化 | **hardlink は 1 回だけ数える**ようになり、**symlink はリンク先文字列長を数える**ようになった。 sparse は従来どおり論理サイズ | 旧 `humanize.ApparentSize` は regular file の `info.Size()` を名前ごとに単純加算していた。 df の Size は本物の `du` 意味論。 ユーザ向け doc の該当記述を更新した |
| **D4** | `WorkspaceHomeSize.Path` の語彙 | `Path` (`json:"path"`) を **`Volume` (`json:"volume"`) に置換**。 `WorkspaceRemoveResponse.HomePath` → `HomeVolume` (`json:"home_volume"`)。 併存させない | volume 名は path ではない。 併存させると「2 PR 前に使うのをやめた directory」を指す legacy field を出し続けることになる。 CLI/daemon skew は本 repo に API versioning が無く、知らない field は zero value に落ちるだけ (`printWorkspaceHomes` の doc comment が旧 daemon 挙動として既に明文化している流儀)。 Phase 3 でリモート接続が入った以上 skew は現実にあるので、**renderer 側が空の識別子を「未報告」として括弧ごと省く**ようにした — サイズは出る、出所だけ出ない |
| **D4-b** | skew 時の `workspace remove` (round-2 review Blocker 1) | 「識別子を省く」だけでは**不足**だった。 `HomeDeleted=false` + 削除エラー無しのとき、**`HomeVolume` が空なら黙らず warning を出す** (`docker volume ls --filter label=boid.workspace_home=<slug>` を案内)。 `HomeVolume` がある = daemon が volume 名を答えている場合のみ「home が無かった」として無言 | D4 の「サイズは出る、出所だけ出ない」は `workspace show` には成り立つが `remove` には成り立たない。 PR6 daemon は存在しない host directory を `os.RemoveAll` して `home_deleted=false` / エラー無し / `home_path` (PR7 CLI は読まない) を返すため、**DB row だけ消えて認証情報入り volume が engine に残っているのに CLI は "removed" しか出さない**。 判別は `Volume` field の有無で行う — store は volume の存在有無に関わらず `Volume` を必ず埋めるので、空 = 「home 経路を通っていない daemon」と一意に決まる。 同じ理由で `HomeSizeError` があって未削除のケース (= `VolumeInspect` が失敗し存在を確認できていない) も「size unknown」ではなく「削除を確認できなかった」として出す |
| **D5** | 機能有効化ゲート | `RuntimesDir != ""` → **`Homes != nil`** (engine handle の有無)。 false のときの挙動 (home 情報を丸ごと省略) は不変 | rewiring 後この機能の依存は engine handle だけで、host path は 1 つも残らない。 nil 判定は `*client.Client` の**具象**側で行う (`workspaceHomeStore()`) — typed-nil を interface に入れると non-nil になり、機能が silent に ON になって初回使用で panic する |
| **D6** | in-use 時の削除 | `Force: true` は維持するが、**409 は握り潰さず `HomeDeleteError` に載せる** | 実測どおり force は in-use を剥がさない。 force が買っているのは「inspect と remove の間に消えた volume を 404 にしない」= 旧 `os.RemoveAll` の寛容さだけ。 成功扱いにすると「認証情報を消した」と嘘をつくことになる |
| **D7** | orphan 検出の突合 | 列挙は `boid.workspace_home` の **presence filter**。 slug は**label の値**から取る。 install scope は **名前の再構成** (`WorkspaceHomeVolumeName(myInstallID, labelSlug) == v.Name`) で判定し、`boid.workspace_home_install_id` は**参照しない** | `SanitizeNamePart` は非可逆なので名前から slug は復元できない。 install-id label は installID が空のとき付かない (`ensureNamedVolumes`) ので、`reap.Run` 流の install-scoped filter は**その環境で 1 件も列挙しない** — 破壊する reap には fail-safe だが、報告するだけの listing では逆。 名前再構成なら label の有無に関わらず同じに効き、engine を共有する 2 install が互いの home を orphan として報告することも防げる。 degrade するのは「install id を持つ前に作った `...-noinst-<slug>` volume」だけで、それは daemon が mount もしていないので listing から外れる方が一貫する (doc に手動 cleanup 手段を明記) |
| **D8** | engine 呼び出しの deadline 粒度 (round-2 review Major 1) | `workspaceHomeVolumeTimeout` は **1 operation ではなく 1 engine call** を縛る。 `Get`/`List`/`Remove` は各リクエストの直前に caller の ctx から新しい deadline を切る (`engineCall`) | 1 つの ctx を `DiskUsage` → `VolumeRemove` で共有すると、**df が deadline を使い切った時点で削除が期限切れ ctx で即失敗する** = 本 package の最重要契約「サイズ失敗は削除を妨げない」が壊れる。 しかも `WorkspaceHandler.Remove` は DB row を先に消すので、結果は「workspace は消えた・認証情報 volume は残った」という最悪形。 operation 全体の上限は伸びる (`Remove` は最大 3×) が、caller 側 ctx で縛れば済む話であり、契約を捨てて上限を守る意味はない。 fake も ctx 期限切れを模倣できるようにして (`dfHangs` / `slowBy` + 各メソッドの `ctx.Err()` 確認) 経路を pin した |
| **D9** | listing 失敗の伝達 (round-2 review Major 2) | engine の volume 列挙失敗を **`WorkspaceHomesListError` に載せる** (lister 失敗と同じ field)。 daemon log だけに残すのは不可 | 省略された section + 空の list error は「home volume が 0 件の install」と**バイト単位で同じ**で、CLI は何も出さない = engine down と区別できない。 host path 版は列挙失敗時に `listErr` を設定していたので、載せないのは新規の欠落ではなく**契約の後退**。 field を分けないのは CLI の行動が同じ (「一覧は出せない・理由はこれ」) だから |
| **D10** | test が実 engine を触らない構造 (round-2 review Blocker 2) | `testutil/homeenv` の isolate 対象に **`DOCKER_HOST` を追加** (throwaway dir 内の存在しない socket を指す) し、`DOCKER_API_VERSION`/`DOCKER_CERT_PATH`/`DOCKER_TLS_VERIFY` は unset。 `testutil.NewTestServer` は un-isolated な環境では `t.Fatal` する | `NewTestServer` は `Config.Backend` を注入しない = **production path** なので、`client.New(client.FromEnv)` が開発者の engine に繋がる。 PR7 で `DELETE /api/workspaces/{slug}` が `VolumeRemove(Force: true)` になったため、**テストが実 volume を消す** (実測: `go test ./cmd/` 1 回で `boid-ws-home-noinst-team-c` が消滅)。 engine 注入 seam ではなく env 側で塞いだのは、engine handle が `buildRuntime` の奥で解決されるため consumer 全部に効く唯一の場所だから。 `internal/api` は `TestMain` 自体が無かった (= AssertIsolated も無かった) ので、`NewTestServer` 側の guard を choke point として追加した |

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
| 改竄耐性の根拠 | 「home dir の**外**だから」は**新旧どちらの置き場でも成立する** (旧 `homes/<slug>.init.json` も mount の外) ので、移設の理由としては使わない。移設の理由は PR6 で home が volume 化しても daemon が普通の file I/O + flock を保てること。**「daemon の領域だから job から一切見えない」とは主張しない** — 下の「解決済み: `capabilities.docker` を持つ job から daemon の state volume が mount できた」節を参照 (PR2.5 で塞いだが、daemon が自分の container を特定できる環境に限る) |
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

#### 決定 (PR6 で変更・実装済み): identity は home 内のファイルではなく volume の label に置く

**home 内の nonce ファイルは撤去した。** PR6 で home が named volume になり、
**daemon は volume の中身を一切読めなくなった** (読むには container を起こすしかなく、
それを毎 dispatch やると completion marker が存在する理由 = fast path が消える)。
書けるが読めないファイルは検査ではないので、identity を volume 自身の label
`boid.workspace_home_id` (`dockerres.LabelWorkspaceHomeID`) に移した。

| 項目 | 確定値 |
|---|---|
| 突合の機構 | `VolumeCreate` **1 回**。「volume を確保する」と「その identity を読む」を同時に行う。container 不要 |
| 根拠 (実測 2026-07-27, podman 4.9.3) | 同名 volume に対する create を 3 回 (label あり / 別 label / label 無し) 発行 → いずれも **201 + 1 回目の label をそのまま返却**。docker compat API 経由 |
| 生成側 | `resolveWorkspaceHome` (毎 dispatch、candidate を crypto/rand で mint) と `containerBackend.ensureNamedVolumes` (Launch が volume 不在を見つけた場合)。**boid が volume を作る経路はこの 2 つだけで、どちらも必ず identity label を付ける** |
| label 無し volume を見つけたら | **fail loud** (dispatch を止める)。Engine API に volume label の更新は無いので再 init しても収束しない — 収束しない fail-safe は livelock。boid 自身は作れない状態なので、外部で作られたと報告するのが正しい |
| marker の世代 | **2** に bump。理由は「世代 1 の marker が指す identity の**形式**が存在しなくなった」ことと、PR8 を経由しない移行経路 (手動 `docker cp`、backup 復元) への保険 (論点 f) |

**検出できること / できないこと (PR6 での更新)**:

| | 内容 |
|---|---|
| **検出できる** | **volume の削除・再作成**。`docker volume rm`、論点 a の reap 誤爆、workspace remove の半完了。PR6 以降 workspace home が消える経路は事実上これだけになった (named volume はホスト再起動を生き延びるので、tmpfs 消失という元々の動機は**消滅した**) |
| **検出できない (新規)** | **volume は残ったまま中身だけ消えた / 入れ替わった**ケース。daemon から volume の中身は見えないので、これは実装の抜けではなく**境界そのもの**である。手動 `docker cp` での復元、container 経由での中身の差し替えがこのクラス |
| **元から検出できない** | **job による意図的な細工**。PR2 の codex review 2 巡目で確定済み — home は job が rw で所有するので、home 内に置いた何であれ job には読めて書き戻せた。PR6 はこの点を悪化させていない (むしろ job から触れない場所へ移った) |

**この 2 つを混同しないこと**: 「中身だけ消えた」は PR6 で**新たに失われた**検出であり、
「意図的な細工」は**元から無かった**。前者の代償は 論点 e-2 の bind target 検証を
job container 側に移したことで部分的に埋め合わされている
(skeleton が壊れていれば次の job が loud に落ちる)。

##### 以下は PR2 期の nonce ファイル方式の記録である (PR6 で撤去済み)

> ここから「割り当てを PR5/PR6 から PR2 に前倒しした理由」の節までは、identity が
> `<homeDir>/.boid-workspace-home-id` というファイルだった時期の判断の記録である。
> **PR6 でファイルごと撤去され、`readWorkspaceHomeNonce` /
> `workspaceHomeNonceFileName` と `TestReadWorkspaceHomeNonce_*` /
> `TestResolveWorkspaceHome_Nonce*` は現在のコードに存在しない。**
> 残してあるのは、identity を volume label に移した判断が「何を失って何を得たか」を
> 説明できるのがこの記録だけだからで、実装の説明として読んではいけない。
> 現行の説明は 1 つ上の「決定 (PR6 で変更・実装済み)」節にある。

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
記述が真になる。**この段落が説明している防御は PR6 で不要になった** — 読む対象のファイルが
無くなり、identity は daemon 側が engine に問い合わせる label になったので、
job が細工できる読み取り経路そのものが存在しない。現行コードで対応する doc comment は
`workspace_home.go` の `workspaceHomeMarker.HomeID` / `workspaceHomeInitialized` /
`Runner.ensureWorkspaceHomeVolume` の 3 つで、`readIfExists` の doc comment に
「nonce ファイルは 3 番目の caller だったが撤去された」という言及が残っている。

**nonce は init.sh の実行有無に関わらず書く。** Phase 4 の契約では script を持たない workspace も
marker だけは打たれる (`home-workspace-volume.md:98`) ので、条件分岐させるとそのクラスにだけ
突合が効かなくなる。PR5 で init.sh を container 実行に移す際は、この書き込みが
次節の prep ステップに移る (prep を条件付きにしてはいけない理由も同じ)。
→ **PR6 で無効化**。identity は volume 作成時に付くので、init.sh の有無に依存しなくなった。
prep を無条件に走らせる理由は skeleton mkdir 側に一本化された (論点 b-2)。

**割り当てを PR5/PR6 から PR2 に前倒しした理由**: nonce は「marker と実体の寿命を分離した」ことの
埋め合わせであり、**分離を行う PR がその埋め合わせを負う**のが筋。PR5/PR6 に置くと、
上に書いたとおり PR2 landed 直後からホスト再起動のたびに窓が開き、PR6 まで開きっぱなしになる。
初版は nonce を「volume の identity」と捉えていたため volume 化の PR に置いていたが、
実際に必要なのは volume ではなく **home ディレクトリの incarnation の identity** で、
これは PR2 の時点で既に必要になっている。

#### marker の世代 (`init_generation`) — PR5 codex レビュー 2 巡目 [Major 1] で追加

**決定**: marker に `init_generation` (int、`omitempty`) を持たせ、
**現在の世代と一致しない marker は再 init 扱い**にする。現在値は
`dispatcher.workspaceHomeInitGeneration`。

| 世代 | 実行環境 |
|---|---|
| **0** (= フィールド不在) | PR2-PR4。daemon プロセスが `/bin/bash` を直接 exec し、`$HOME` は host の `~/.local/share/boid/homes/<slug>` |
| **1** | PR5 以降。runner image から起こす使い捨て container。`$HOME` は **job が見るのと同じ container 内 path** (`hostHomeDir()`) |

**なぜ必要か**: PR5 の skip 条件は script hash と nonce だけで、**どちらも PR2-PR4 期に
init 済みの workspace で一致する**。つまり PR5 の新経路が既存 install で一度も実行されず、
「init.sh が job と同じ `$HOME` パスを見る」という PR5 の目的 (論点c §D9 / 挙動変化 2) が
**既存 workspace に一切反映されない**。 script の内容は変わっていないが、
それを実行した**環境**が変わっており、この 2 つは独立した軸である
(`ScriptSHA256` が前者、`init_generation` が後者)。

**なぜコストが低いか**: 世代 1 の時点では home の**実体は変わっていない**
(host の `homes/<slug>` を bind するだけ) ので、toolchain は既にそこにある。
冪等が契約の init.sh は大半を短絡する。再 init が実際に直すのは
**絶対パスを焼き込んだ artifact** — symlink の target、shebang、wrapper script、
installer が記録した prefix。

**なぜ PR6 まで待てないか** (PR6 で更新 — 初版は撤去済みの nonce ファイルを前提にしていた):
PR6 の cutover 自体は fresh volume を作るので、identity 突合だけでも自然に再 init される。
**世代を入れておく価値は PR8 側にある** — 移行 CLI は既存 host home の**内容を volume にコピー**する。
identity が home 内のファイルだった頃は「忠実にコピーすると identity ごと運ばれて突合が通る」ことが
問題だったが、**PR6 で identity は volume の label に移ったのでコピーでは運ばれない**。
代わりに残る穴は「**identity も世代も一致したままの volume に、host 時代の中身が流し込まれる**」であり、
その扱いは論点 f に書き直した。世代 1 の marker (PR5 期に init 済み) を弾く役割は変わらず有効である。

**比較は等値であって下限ではない**。`>=` は「必要な世代以上なら OK」と読めて自然だが、
リリースを rollback した環境で「実行中の build が用意していない home」を skip してしまう。
不一致はすべて再 init。

**`BoidVersion` を流用しない理由**: 既に記録されており毎リリース変わるので、比較に使うと
**環境が変わっていない patch release でも全 workspace が再 init される**。
世代は「名前が指すものが変わったときだけ変わる」ので、不一致に意味がある。

以上は `workspace_home.go` の `workspaceHomeInitGeneration` /
`workspaceHomeMarker.InitGeneration` / `workspaceHomeInitialized` の doc comment にも
同じ内容で書いてある。テストは
`TestResolveWorkspaceHome_MarkerFromAnOlderExecutionGeneration_ReInitializes` /
`..._MarkerRecordsTheCurrentExecutionGeneration` /
`..._MarkerFromANewerExecutionGeneration_ReInitializes` で pin。

**今後この機構を使う条件**: 実行環境を変える PR は世代を bump する。逆に
「script の解釈が変わらず、用意される home も同一」であれば bump しない。

### 論点 b-2: volume prep ステップ (init.sh の有無に関わらず必ず 1 回走らせる)

**volume 作成後、init.sh の有無に関わらず prep container を 1 回走らせる。** 仕事は 2 つ:

> **PR5 で実装済み (2026-07-27)**。 独立した prep container ではなく、init container の
> builtin prelude / postlude として統合した (論点c §D1)。 実際に作る集合は
> `dispatcher.workspaceHomeSkeletonDirs()` が単一出典で、daemon 側の `prepareBindTarget`
> と共有している (drift すると engine が uid 0 でその 1 本だけ作ってしまうため)。
> `~/.local/bin` などは**入れていない** — bind target ではないので engine の自動生成対象に
> ならず、必要なら init.sh が自分で作れる。
>
> **PR6 で更新 (2026-07-27)**: postlude (nonce) は撤去し、prelude (skeleton mkdir) だけが
> 残った。 daemon 側の `prepareBindTarget` も撤去し、代わりに
> **skeleton の集合を marker (`skeleton_dirs`) に記録して集合が変わったら再 prep する**
> 方式に置き換えた。 所有者検証は job container 側 (`internal/sandbox/runner` の
> `verifyHomeSkeleton`) へ移設した。 詳細は下記「PR6: 作成と検証の行き先」。

1. **skeleton の mkdir** を **backend の uid/gid で**作る (§D7 — daemon の
   `os.Getuid()` 由来であり、**リテラルの 1000 ではない**。 GH Actions では 1001 になる実績がある)。
   作るべきものは `~/.claude` と、**embedded skill ごとの `~/.claude/skills/<name>`**
   (`skills.EmbeddedSkillNames()` の各 entry — 論点 e-2 の決定で bind は
   skill ごとに 1 本ずつになったので、`~/.claude/skills` を作るだけでは
   足りない) の**ちょうどその集合だけ**であり、`~/.local/bin` のような
   bind target でないディレクトリは**含まない** (上の PR5 注記を参照)。
   **PR3 が daemon プロセスの `skills.MkdirAllNoSymlink` で暫定的にやっているのと
   同じ集合**であり、PR5 の仕事はその移設である (論点 e-2 の
   `syncEmbeddedSkills` doc comment 参照)
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
→ **PR5 で `docs/plans/home-workspace-volume.md` の 契約節・`docs/{ja,en}/guide/workspace-home.md`・
`docs/examples/workspace-home-init.sh` に反映済み。**

#### 決定 (PR6): 作成は marker で追随させ、検証は job container へ移す

volume 化で daemon は home の中に mkdir も stat もできなくなった。 PR3/PR5 が
daemon 側に置いていた 2 つの仕事はこう分かれた:

| 仕事 | PR5 まで | PR6 以降 | 理由 |
|---|---|---|---|
| **作成** (mkdir) | init container の prelude **+** daemon が毎 dispatch (`prepareBindTarget`) | init container の prelude のみ。 marker に `skeleton_dirs` を記録し、**集合が変われば再 prep** | daemon は volume に書けない。 集合は **boid バイナリの性質** (embedded skill ごとに 1 本) なので、skill を足したリリースが marker 一致のままの home に bind target を増やす — それを marker に載せて検出する |
| **検証** (所有者) | daemon が mkdir 直後に `fstat` | **job container の runner** が harness 起動前に `sandbox.Spec.HomeSkeletonDirs` を検査 | daemon は volume を stat できない。 論点 e-2 は「作る主体の中で検証しても意味がない」と要求しており、prelude は作る側なので不可 |

**検証の対象は skeleton 全体ではなく「mount に覆われていない祖先」だけである**
(PR6 codex レビュー Minor 1 で訂正)。 per-skill の leaf (`~/.claude/skills/<name>`) は
**同じ spec が read-only bind の target にしている**ので、job container の中で `os.Stat` すると
volume 側ではなく **daemon が materialize した bind source** が返る。 つまり leaf の検証は
別のディレクトリについての検証になっており、成立していなかった。 現行は
`dispatcher.homeSkeletonDirs` が **この spec の mount target に一致する entry を落とす**
(ハードコードではなく mount 一覧との突合なので、将来 `.claude` を覆う mount が入っても同じ理屈で外れる)。
同じ突合が **ProfileInit sandbox** (`boid kit init` / workspace configure) も外す —
`Runner.Dispatch` は profile を知る前に workspace home を resolve するので
`SandboxRuntimeInfo` の 2 フィールドは埋まっているが、ProfileInit の分岐は
**HOME を mount しない** (host `/` の read-only rbind がそのまま `$HOME` になる)。
フィールドで判定すると、誰も作る予定の無いディレクトリを理由に
**ProfileInit の job が毎回落ちる**。 mount 一覧から導けばこれも同時に外れる。

残るのは `.claude` と `.claude/skills` の 2 つで、**認証永続化に効くのはこの 2 つである**
(engine は欠けている path を**丸ごと**掘るので、leaf が無いときは祖先も uid 0 で作られ、
そちらが検出される)。

**leaf が uid 0 で作られる可能性自体は残る**が、実害は無い: 作られた直後に read-only bind で
覆われ、その container の生存中ずっと覆われたままなので、sandbox 内の誰もそこへ書かないし
書く必要も無い (embedded skill は boid が供給する再生成可能なコンテンツであって harness の state ではない)。
残るのは「**将来そのスキルを配らなくなったリリースで、空の root 所有ディレクトリが volume に居残る**」
という後片付けの問題だけで、`verifyHomeSkeleton` が案内する host 側削除で同じように消せる。

**job container 側に置いたのは妥協ではなく改善である**。 daemon 側の検証と比べて:

- **作る主体ではない** (作るのは init container の prelude) — e-2 の要求を満たす
- **毎 dispatch 走る** (init container は marker が一致する限り走らない)
- **engine の自動生成の「後」に走る** — daemon 側の検証は launch より前なので、
  「engine がこの launch で uid 0 のディレクトリを作った」ことは次の dispatch まで見えなかった
- **harness と同じ uid で走る** — 「daemon の uid と一致するか」は
  「harness が書けるか」の代理指標にすぎなかったが、こちらは当の主体そのもの

**失われたもの (明記)**: daemon 側 mkdir の副作用だった**自己修復**。 job が `~/.claude` を
消して終了した場合、これまでは次の dispatch の mkdir が正しい所有者で作り直していた。
volume に対しては誰もそれができないので、次の launch で engine が uid 0 で作り、
runner がそれを loud に報告する形になる。 **検出は元々この仕組みの唯一の価値**として
明文化されていた (「これは lock ではなく detector である」) 一方、修復は mkdir を
やっていたことの副産物だった。 復旧手順も移設した (`verifyHomeSkeleton` のエラー文面):
volume の `Mountpoint` 配下を所有者権限で削除し、**marker も消して init を 1 回走らせる**
— 削除だけでは次の launch で engine がまた uid 0 で作るため。
**案内するパスは volume 内の相対パスであること** (PR6 codex レビュー Major 2 で訂正):
HOME volume は sandbox の `$HOME` に mount されているので volume の root = `$HOME` であり、
消すのは `<Mountpoint>/.claude` である。 container 内の絶対パスをそのまま
`Mountpoint` に連結した `<Mountpoint>/home/boid/.claude` は**存在しない**ので、
運用者は何も消せず、次の job が同じ検査で落ちる — エラー自身が作ったループになる。
そのため `sandbox.Spec` は mount point (`HomeSkeletonRoot`) と相対 entry
(`HomeSkeletonDirs`) を分けて持ち、runner は前者を stat 用に join し、
後者をそのまま復旧コマンドに埋める。

**実測 (2026-07-27, podman 4.9.3, rootless, `UsernsMode: keep-id`, `User: 1000:1000`)**:
image 由来で populate された volume に対し `/home/boid/.claude/skills/<name>` を bind すると

```
drwxr-xr-t 3    0    0 .claude          ← engine が uid 0 で自動生成
drwxr-xr-t 3    0    0 .claude/skills
$ touch /home/boid/.claude/probe   →   Permission denied
```

host 側から見た所有者は subuid 100000 で、daemon (uid 1000) には chown も unlink もできない。
**論点 b-2 の元の実測 (host dir 時代) が volume + keep-id でもそのまま再現する**ことを確認した。

#### bind target の TOCTOU: 窓は閉じない・**回復不能な結果だけ**塞ぐ (PR3 codex レビューの判断)

bind target を用意してから job container が起動するまでには窓がある。同一 workspace の
**並行 job はその workspace HOME を rw で共有している** (`homeMounts` の bind に `ReadOnly` は無く、
`resolveWorkspaceHome` は slug だけで home を引く — job ごとではない) ので、その窓で
job A が `~/.claude` を rename / delete でき、job B の起動時に bind target が不在になって
engine が uid 0 で自動生成する。

**窓そのものは閉じない。** 理由は 3 つ:

1. **PR3 が持ち込んだ窓ではない。** job A は今でも `~/.claude/.credentials.json` を直接消せるし
   `~/.claude` を rename できる。bind target の有無に関わらず成立していた性質で、
   新しい脆弱性クラスではない (`homeMounts` の doc comment が Phase 5b PR6 の codex レビュー以来
   「同一 workspace の 2 job は並行しうる」を明記している)
2. **閉じるには workspace 単位で mkdir 〜 container 起動を直列化するしかない。**
   全 dispatch が container 起動を含む区間でロックを持つことになり、代償が (1) に見合わない
3. **PR5 の prep container でも解消しない。** prep は workspace HOME につき 1 回走るだけで、
   その後も job は HOME を rw で持ち続ける。prep 直後から同じ rename が可能になる

**新しいのは「結果が回復不能になる」点だけ**であり、そこは PR3 で塞いだ。uid 0
(rootless podman では host subuid に写像された root) 所有のディレクトリは、uid 1000 の harness は
もちろん **daemon 自身も chown で修復できない**。放置すると workspace HOME が永続的に poisoning され、
症状は「認証が毎回消える」という silent failure になる。

**PR3 の実装**: `prepareBindTarget` (`internal/dispatcher/skills_overlay.go`) が mkdir の**後**に
`skills.MkdirAllNoSymlink` の返す所有者 uid を daemon 自身の uid と突き合わせ、
一致しなければ dispatch を fail させる。比較対象は **daemon が所有していること**であって
特定 uid ではない (deploy 形態で uid は変わる)。所有者 uid は walk が最後に開いた fd への
`fstat(2)` で読む — path 指定の stat では別ディレクトリの所有者を報告しうるので、
チェック自体が race になる。

**これは lock ではなく detector である。** チェック通過後に差し替えが起きればその job は負ける。
価値は「**次の** dispatch が poisoning を黙って踏まずに、修復手順つきで報告する」ことにある。

**エラーが案内する修復手順は「当該ディレクトリの所有者権限での直接削除」**
(rootless podman なら `podman unshare rm -rf <dir>`、rootful docker なら `sudo rm -rf <dir>`)。
次の dispatch が daemon 所有で作り直すので、HOME の他の中身は残る。
初版は `boid workspace remove <slug>` を案内していたが、これは修復手順として成立しない
(PR3 codex レビュー 2 巡目):

- **`default` には使えない**。予約 slug なので `cmd/workspace.go` の `runWorkspaceRemove` /
  `api.ProjectAppService.RemoveWorkspace` / `orchestrator.WorkspaceRepository.Remove` の 3 層で拒否され、
  さらに `api.deleteWorkspaceHome` が同じ slug で home 削除を明示的にスキップする。
  workspace 未割り当ての project は全部この workspace に dispatch される (`normalizeWorkspaceSlug`)。
- **非 default でも home 削除は best-effort**。`api.WorkspaceHandler.Remove` は DB row を先に消し、
  home の削除が失敗しても 200 のまま返す (`WorkspaceRemoveResponse` の part-completed 契約)。
  結果は「workspace 定義だけ消えて poisoning された home が孤児として残る」という
  最悪の組み合わせになりうる。 成否は CLI 出力
  (`home volume deleted` / `warning: home volume delete failed`) でしか判別できない。
  失敗要因は PR6/PR7 で変わった: 当時は所有者の違うディレクトリに対する `os.RemoveAll` の
  EACCES (実測: 書き込み権の無いディレクトリ配下の子の `unlinkat` が EACCES。空なら親の権限だけで
  消せる。engine の自動生成は不在パス全体を作るので必ず子を持つ)。 PR7 以降は `VolumeRemove` なので
  所有者は関係なく、代わりに **その workspace で job が走っていると engine が 409 を返す**
  (論点 a-2 D6。force でも剥がせない)。
- **成功しても workspace 定義ごと消える**。assign 済み project は `default` に戻される
  (`WorkspaceRepository.Remove` の transaction) ので、作り直して再 assign が要る。

したがってエラー文では直接削除を主手順とし、`workspace remove` は上記 3 点の但し書き付きの
代替として残す (home がこの 1 ディレクトリを超えて壊れている場合には依然として出口だから)。

### 論点 c: init.sh の実行経路

現行は daemon プロセスが `/bin/bash <tmpfile>` を直接 exec し、`HOME` を workspace HOME に
差し替えて走らせる (host 側 trusted 実行)。これが init.sh の契約:

> 実行環境: ホスト側 (trusted) で boid が dispatch 前に呼ぶ

案 A ではこれを **workspace volume をマウントした使い捨て container** に置き換える。
DooD の仕組み (job container 生成) は既にあるので流用できる。

**trusted 境界は維持される** — script を選ぶのも container を起こすのも daemon であり、
sandbox 化された job の中で走るわけではない (A' との違いはここ)。ただし
「ホスト側で実行」という文面は「daemon が制御する専用 container で実行」に更新が要る。

**決定 (PR5 で確定・実装済み)**。以下 §D1〜§D10 は実装が従っている決定であり、
対応する doc comment (`internal/dispatcher/workspace_init.go` /
`container_backend_workspace_init.go`) に同じ内容が根拠つきで書いてある。

| # | 項目 | 確定値 |
|---|---|---|
| **D1** | container の回数 | **1 回だけ**。prep (skeleton mkdir / nonce 書き込み) は別 container ではなく、boid が挿入する **builtin prelude / postlude** として同じ container に統合する。**init.sh の有無に関わらず必ず 1 回走る** (論点 b-2 の 2 — 素通し workspace に nonce を書く主体が無くなるため) |
| **D2** | user script の渡し方 | wrapper 全体を container の **stdin に流す** (`bash -s`)。daemon container 内の一時ファイルは host engine から見えず sibling container の bind source にできない (compose.yml の KNOWN GAP と同じ構造)。user script は wrapper に**連結しない** — **quoted heredoc** (`<<'DELIM'`) で container 内の一時ファイルに書き出してから `bash <file>` で実行する。delimiter は **crypto/rand 由来の推測不能な文字列** (`BOID_INIT_EOF_<32 hex>`) を run ごとに生成する。**「hash した bytes == 実行した bytes」は維持されるが、hash 対象は user script であって wrapper 全体ではない**。heredoc は最終行を必ず改行終端するので、**改行で終わらない script は 1 byte 増える** — そこだけ `truncate -s -1` で戻して byte 一致を保つ。NUL byte を含む script と delimiter 行を含む script は **fail-closed で拒否**する (前者は heredoc で運べない、後者は無言の切り詰めになる) |
| **D3** | exit code | user script が非ゼロなら init は失敗し、**その exit code をそのまま伝播**する。prelude / script-setup / postlude は boid 側の段階なので専用 code (91 / 92 / 93) を返す。**postlude (nonce) は user script が成功したときだけ実行する**。段階の判別は wrapper が stderr に出す `boid-init: stage=<stage> exit=<code>` マーカー行 (**最後の 1 行が勝つ**) を第一根拠とし、マーカーが無いときだけ exit code 表に落ちる — user script が 91 を返しても prelude 失敗と誤認しないため。マーカーも boid code も無い場合は `unknown` (例: OOM kill の 137、engine 側の起動失敗) で、**init.sh のせいにはしない** |
| **D4** | network | **engine の既定 bridge** (`NetworkingConfig` も `NetworkMode` も設定しない)。理由: ①job 用 workspace network は `Internal: true` + allowlist proxy で、init.sh の主目的である toolchain ダウンロード (実測 1.5GB) がほぼ全滅する。②compose の `boid_default` に `docker network connect` する案は却下 — daemon の自己特定に依存するが `ContainerBackendOptions.SelfContainerID` は `os.Getenv("HOSTNAME")` 由来で**実環境 (live rootless podman) では空になる実績がある**。必要な性質は「外に出られる」ことだけ。③init container は daemon の内部サービス (broker / gateway / egress / dockerproxy) と話す必要が無い。**無制限 egress を持つことは trusted 境界の一部として明示的に宣言する** |
| **D5** | label と掃除 | 専用キー `boid.workspace_init` (値 = slug) + `boid.workspace_init_install_id`。**`boid.job_id` を付けてはいけない** — `ReapOrphans` は presence だけで列挙し `ContainerRemove{Force, RemoveVolumes}` するので 1.5GB ダウンロード途中でも殺され、さらに remove 失敗が `FailedJobIDs` に載って**無関係な task の auto-reopen を抑止**する。`boid.install_id` も同様に `reap.Run` の filter。掃除は `ReapOrphans` に**専用 label を見る別ループ** (`reapOrphanWorkspaceInitContainers`) を足して行い、**`ReapedJobIDs` / `FailedJobIDs` は一切汚さない**。回収対象は**終端状態の allowlist** = `exited` / `dead` のみ (`workspaceInitContainerIsTerminal`)。初版は `running` / `restarting` を守る denylist だったが **`paused` が force remove に流れていた** (codex レビュー 2 巡目 [Major 2])。paused は終了済みの残骸ではなく**途中状態を保持した生存プロセス**で、daemon 再起動だけで install が破棄される。allowlist に反転すると**未知の状態文字列が自動的に保護側**に落ちる — engine は state を増やすし wire 型はただの string なので、denylist は同じ形のバグを将来分ぶん抱える。`created` も**終端ではない**ので回収しない (create と start の間の並行 daemon と区別できない)。`removing` は engine が既に処分中。回収し損ねた container は決定的名により次の init が D6 の経路で解決するので、誤って残す代償は container レコード 1 個に収まる。集合の出典は `container.ContainerState` (`moby/moby/api/types/container/state.go`) と `ValidateContainerState` |
| **D6** | 多重実行の防止 | 決定的な container 名 `boid-ws-init-<installID8>-<slug>` (`dockerres.WorkspaceInitContainerName`) による名前衝突。flock はプロセス死で解放されるが container は生き延びるので、daemon crash 後の再 init を止められるのは engine 側の 409 だけ。衝突は `errdefs.IsConflict` で検出し、既存が **running なら完了を待つ** (上限 30 分)、**停止していると確認できたときだけ**即 remove、その後 create を 1 回だけ retry し、**2 回目も conflict したら fail loud**。**破壊の前に「自分のものか」を 3 つの label 値すべてで確認する**: `boid.workspace_init` の**存在**・その**値 == 今 init しようとしている slug**・`boid.workspace_init_install_id` == 自 install。初版は slug の**値**を比較せず「名前は (installIDPart, slug) の純関数だから構成上一致する」と論じていたが、**これは循環している** — 衝突相手がその生成関数で作られた保証こそが未知だからである (`docker rename` で他 workspace の init container が名前を奪える。codex レビュー 2 巡目 [Blocker])。また `InspectResponse.State` は**ポインタ**であり `nil` は「停止」ではなく「engine が状態を答えなかった」なので、**inspect が 500 を返したときと同じく fail loud** にして何も消さない (同 [Blocker])。待ち上限を超えた場合は **force kill せず fail** する (上限超過時に boid は「遅い install」と「wedge」を区別できず、後者を誤って kill する代償の方が大きい)。flock は同一プロセス内の直列化として**引き続き維持** |
| **D7** | uid / userns | `User` は job container と同じ `b.uid:b.gid` (daemon の `os.Getuid()` 由来。**リテラルの 1000 は書かない** — GH Actions では 1001 になる実績がある)。rootless podman では `UsernsMode: keep-id` (`resolveUsernsMode` の backend 単位キャッシュを流用) — 付けないと prep が作ったディレクトリが host 側で subuid 所有になり、PR3 の `prepareBindTarget` が即 fail する |
| **D8** | image と Entrypoint | image は **backend の default (`boid-runner:latest`)**。workspace の `ContainerImage` override は**尊重しない**: ①`resolveWorkspaceHome` は `Runner.resolveContainerImage` より前に走り、backend 外から image を解決する公開経路が無い。②`resolveImage` は `boid.runner_protocol=v1` label の無い override を拒否し、その label を焼いている image は**まだ存在しない**ので、尊重すると `container_image` を設定した workspace は home 初期化ごと不可能になる。③§決定 11 により override は base image 由来が必須なので base は常に部分集合。image の ENTRYPOINT は `["/usr/local/bin/boid","runner-container"]` 固定なので **`Entrypoint: ["/bin/bash","-s"]` で明示的に上書き**し、`Cmd` は空 slice (nil は image の CMD を継承する)。TTY 無しなので出力は `demuxDockerFrame` で demux する |
| **D9** | env | **`HOME` / `BOID_WORKSPACE_SLUG` / `BOID_WORKSPACE_HOME` の 3 つだけ**。旧 allowlist (`PATH` / `USER` / `LOGNAME` / `LANG` / `LC_ALL` / `TERM` を daemon プロセスの値で) は全部落とす。`PATH` は **key ごと出さない** — そうすることで engine が image の PATH を適用し、boid 側に image と drift しうる定数を持たなくて済む。`USER`/`LOGNAME` は daemon の host 上の identity で container の user とは無関係 (image は該当 uid の `/etc/passwd` entry を焼いてある)。`LANG`/`LC_ALL` は image に無い locale を指すと glibc/perl が警告を出す。`TERM` は TTY が無いのに curses 系 installer が描画を試みる原因になる。**`HOME` の値は job が見るのと同じ container 内 path** (`hostHomeDir()`) — installer が `$HOME` 配下に焼き込む絶対 path が sandbox 内で有効であるために必須 |
| **D10** | 実装の置き場 | `RunWorkspaceInit` は **backend 側 (`*containerBackend`)**。ただし `backend.SandboxBackend` interface には足さない — あれは **job** の lifecycle 契約 (Launch/session/Wait/ReapOrphans) であり、init には job row も session も transcript も job 会計も無い。代わりに **`dispatcher.WorkspaceInitExecutor` を新設して `Runner.Backend` に型アサーション**する (既存様式: `IsContainerBackend` / `ContainerBackendUIDGID` / `ContainerBackendBrokerTLS`)。request 型 `dispatcher.WorkspaceInitRequest` は **export する** — しないと他 package のテストが自前 backend で interface を満たせず、「この fake は workspace home を用意できない」が compile error ではなく実行時エラーになる。`backend.SandboxSession` は通さない (transcript spool / registerSession / diagnostics collector が付いてくる) — `ContainerCreate` + attach + `ContainerWait` + demux を直接叩く軽量パス |

**PR5 が持ち込む挙動変化** (ユーザから見えるもの):

1. **init.sh の実行環境が host から container になった** — 使えるコマンドは image が持つものだけ。
   ホストにしか無いツールには到達できない
2. **`$HOME` の値が変わった** — ホストの `~/.local/share/boid/homes/<slug>` から
   container 内の mount 先 (job が見るのと同じ path) へ。 **副作用として、
   `$HOME` 配下に絶対 path を焼き込むツールが sandbox 内で壊れなくなった**
   (`docs/examples/workspace-home-init.sh` の `link_sandbox_safe` が存在する理由が消えた。
   ヘルパー自体は path 変更に対する保険として残す)
3. **env が 3 つに絞られた** (§D9)
4. **`$0` の値が変わった** — daemon 側 `homes-meta/` の一時ファイルから container 内 `/tmp/...` へ
5. **stdin が `/dev/null` になった** (旧実装も `exec.Cmd{Stdin: nil}` = /dev/null なので実質不変だが、
   wrapper が stdin 経由で来る以上「user script が stdin を読むと wrapper を食う」罠が生じるため、
   明示的に `</dev/null` を付けて維持している)
6. **prep が init.sh の有無に関わらず走る** (§D1)。素通し workspace でも container が 1 回起動する
7. **エラー文面に段階名が入る** (§D3)
8. **skeleton mkdir の失敗が prelude で先に出る** — fresh workspace で `<home>/.claude` が
   ファイルで塞がれている場合、以前は `prepareBindTarget` の (復旧手順つきの) エラーだったが、
   PR5 では prelude の mkdir が先に当たる。 どちらも fail loud で、
   `prepareBindTarget` の経路は **初期化済み home に対して**引き続き効く
   (そもそもそれが検出したい状況 — prep 後に job が `~/.claude` を差し替える — の形である)
9. **既存 workspace で init.sh が upgrade 後 1 回だけ再実行される** — marker の
   `init_generation` が世代 0 (PR2-PR4 の host 実行) のままなので、PR5 の実行環境で
   1 度も init されていない home として扱われる。 これが無いと 1〜8 のどれも既存 install に
   届かない (上の 2 — `$HOME` パスの統一 — が特に効かない)。 詳細と根拠は
   論点 b の「marker の世代」節

**PR5 で意図的に残したもの**: `syncEmbeddedSkills` の daemon 側 mkdir
(`prepareBindTarget`) は**撤去しない**。論点 b-2 は「PR5 の prep へ移設する」と書いているが、
prep は home につき 1 回しか走らない (marker は init.sh の hash しか見ない) のに対し、
**embedded skill の集合は boid バイナリの性質**である。 skill を 1 つ増やした release は
marker が一致したままの home に bind target を 1 つ増やすので、daemon 側 mkdir を今消すと
その target だけ engine が uid 0 で作る — 論点 b-2 が塞いだはずの罠がそのまま再発する。
PR6 で home が volume 化したときに「消す」のではなく「volume でも効く形に置き換える」こと。
所有者**検証**を daemon 側に残すのは論点 e-2 の明文の決定どおり (作る側と同じ場所に置くと意味を失う)。

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
→ **PR6 で実施済み**。 `homeMounts` に渡る値が `<runtimesRoot>/homes/<slug>` から
`dockerres.WorkspaceHomeVolumeName(installID, slug)` に変わっただけで、
`sandbox.Mount` は **`MountBind` のまま**である (分類は Type ではなく Source が決める)。
`SandboxRuntimeInfo.WorkspaceHomeDir` は **`WorkspaceHomeVolume` に改名**した —
値が別種の mount を選ぶようになった以上、`...Dir` という名前は
`filepath.Join` を誘発して**別の junk volume を無言で作る**。
`sandbox.Mount.Source` の doc comment もこの規約を明記するよう更新した。

**init container 側にも同じ分岐が要る** (§D4)。 `workspaceInitHomeMount` の fail-closed は
**極性が反転した**: PR5 までは「相対パスを拒否」(相対だと volume 名として解釈され、
空の volume を黙って作ってしまうため)、PR6 からは「絶対パスを拒否」(絶対だと host dir を
bind してしまい、init は成功報告・marker も打たれるのに**本物の home は一度も準備されない**)。
どちらの時代も「2 つの表現を取り違えると同じ形で壊れる」ので、検査は消えるのではなく
現行の表現に追随する。 さらに init container は `ensureNamedVolumes` を通らないので、
`RunWorkspaceInit` が **自分で label 付きの `VolumeCreate` を発行する** (§D4 の宿題)。

`ensureNamedVolumes` の label 問題 (論点 a) は PR1 で対応済み: name ごとに label を出し分け、
workspace HOME volume には論点 a の 2 label のみを付ける。
「`boid.job_id` label が無い volume」への `slog.Warn` も workspace HOME では出さない
(label が無いのが**正しい状態**なので、warn するとノイズになって本来の警告が無視される)。

userns backend は撤去済みなので、bind 版の workspace HOME 経路を残す必要はない
(`workspaceHomeDir == ""` の tmpfs fallback は、workspace 未解決時の挙動として維持)。

### 論点 e-2: embedded skills sync が workspace HOME に直接書いている

(以下 2 段落は PR3 以前の状態。解決済み — 下の決定表を参照)

`internal/dispatcher/runner.go` は **dispatch のたびに**
`skills.DeployAll(workspaceHomeDir + "/.claude/skills")` で workspace HOME へ直接 file I/O し、
失敗すると dispatch 自体を fail させていた (Phase 4 PR3 で、adapter の bind-mount 経路を
retire した代わりに入った経路)。

volume 化すると daemon はこのパスに書けなくなる。放置すると
「存在しない host dir に書いて成功し、job には skill が届かない」silent drift か、
書き込みエラーで全 dispatch 停止のどちらかになる。**論点 b の marker/lock と同列の
「daemon が workspace HOME に書く経路」であり、同時に解決しなければならない。**

毎 dispatch に使い捨て container を起こすのはコスト的に非現実的なので、
embedded skill は boid バイナリから常に再生成できる (揮発してよい) 性質を使い、
host-visible runtime dir (`hostVisibleRuntimesDirFor` 配下) に materialize して
job container に RO で重ねる。

**決定 (PR3 で確定・実装済み)**:

| 項目 | 確定値 |
|---|---|
| 重ね方 | **skill ごとに 1 本ずつ RO bind** (`<src>/<name>` → `$HOME/.claude/skills/<name>`)。 `$HOME/.claude/skills` を丸ごと重ねる案は**不採用** — ユーザが手で置いた非 embedded skill (bitbucket / jira 等) が隠れる。 これは机上の懸念ではなく、`internal/adapters/opencode/bindings.go` の doc comment と `docs/{ja,en}/guide/workspace-home.md` が「init.sh / 手動で `~/.claude/skills/<name>` にコピーする」運用を実際に案内しているので退行になる。 列挙は `skills.EmbeddedSkillNames()` (Phase 4 PR3 で呼び出し元ゼロになっていたが、doc comment がまさにこの用途で残っていた) |
| materialize 先 | `<runtimesRoot>/skills` = `dispatcher.embeddedSkillsDir(RuntimesDir)`。**install ごとに 1 箇所** (workspace ごとでも job ごとでもない)。 job container の bind source は host から見えている必要があるので、daemon 自身の data root (compose では `boid_state` volume) ではなく `hostVisibleRuntimesDirFor` 配下 |
| 呼び出し頻度 | **dispatch ごとに再 materialize** (現行維持)。 `skills.DeployAll` は内容一致なら書かない冪等実装で、同一 baseDir への並行呼び出しも安全 (`internal/skills/safe_deploy.go` の temp file 掃除が PID 生存判定つき)。 install 1 箇所に集約したことで並行 dispatch の重なりは**広がった**が、それを安全にしているのが同じ機構 |
| `cleanOrphanRuntimes` との相互作用 | `internal/server/wire.go` の `cleanOrphanRuntimes` は daemon 起動時に runtimes root 直下の全ディレクトリを走査し、対応する job row が無ければ `os.RemoveAll` する。 よって `<root>/skills` は **daemon 再起動のたびに消える**。 既存の `spec/` `tls/` `broker-tls/` と同じ扱いで、**dispatch ごとに再 materialize する限り無害**。 逆に言えば「毎回 DeployAll するのは無駄」と見て `sync.Once` や存在チェックで条件付きにすると、再起動後の job が skill 無しで走る。 この相互作用は `embeddedSkillsDir` の doc comment に明記した |
| bind target の作成 | **PR3 では daemon が明示的に mkdir する** (`<home>/.claude`、`<home>/.claude/skills`、`<home>/.claude/skills/<name>`)。 中間の `skills` も明示的に作るのは、engine の自動生成が leaf だけでなく**不在パス全体**を作るため、所有者チェックの対象に含める必要があるから。 理由と PR6 での行き先は下記 |
| bind target の所有者検証 | **mkdir の直後に daemon 所有であることを検証し、違えば dispatch を fail** (PR3 codex レビュー)。 TOCTOU の窓は閉じない — 閉じるには workspace 単位の直列化が要り、PR5 の prep でも解消しない。 塞ぐのは「uid 0 所有になると daemon も harness も修復できない」という**回復不能な結果**だけ。 詳細は論点 b-2 の該当節 |
| 失敗時 | **fail loud を維持** (決定 D4)。 materialize 失敗も bind target 作成失敗も dispatch ごと失敗させる。 `sandbox.Mount.Guard` は**付けない** — source 消失時に silent skip すると、skill 無しで job が走って「/boid-task が無い」という診断しにくい形で出る |
| `internal/server/server.go` の `DeployAll` | **撤去**。 出力先 `<dataHome>/skills` を読むコードはリポジトリ内に存在しなかった (adapter の bind 経路が Phase 4 PR3 で退役した際の残骸)。 materialize 地点が 2 つあってうち 1 つが dead、という状態が本論点の混乱の元だったので、片方を残さない。 既存 install に既にあるディレクトリは削除しない (読まれないだけ) |

**bind target を PR3 で daemon が mkdir する理由** (調査時の推奨とは逆の判断):
現状 `skills.DeployAll(<home>/.claude/skills)` が `openBaseDirSafe` で `/` から掘り下げるため、
`.claude` / `.claude/skills` は**副作用として必ず存在していた**。 PR3 でその書き込みを
runtime dir 側へ移すと、fresh workspace では bind target が存在しなくなる。
bind target が無いと **engine が中間パスを uid 0 所有で自動作成する** (実測は論点 b-2) ため、
**論点 b-2 が PR6 の問題として記述している罠が PR3 で発火する**。 workspace HOME は
PR3 時点ではまだ host path なので daemon から書ける。

この mkdir は **PR5 の prep container (論点 b-2) に移る暫定**である。
mount spec 側で表現する道は無い — `sandbox.Mount.NeedsDirs` は userns backend の
runner だけが読んでいて、PR-4 (`volume-only-daemon.md` §論点e) の backend 撤去で
**dead になっている** (`realization` 層は見ていない)。

mkdir は `os.MkdirAll` ではなく `skills.MkdirAllNoSymlink` (PR3 で
`openBaseDirSafe` を export したもの) を使う。 書き込み先が job の rw 所有下
(`$HOME` の中) だからで、`internal/skills/safe_deploy.go` が最初から扱っていた
脅威モデルそのものである。 DeployAll 自身の書き込み先は job の届かない場所へ移ったが、
その hardening は**この mkdir に引き継がれた**。

mkdir の直後には**所有者検証**が付く (`prepareBindTarget`)。これは mkdir と対で移設するもの
ではない — **prep へ移すのは mkdir だけで、検証は daemon 側に残す**。検証の目的は
「prep やこの mkdir が作ったはずのディレクトリが、その後 engine か job に差し替えられていないか」を
**次の dispatch の時点で**見ることであり、作る側と同じ場所に置くと意味を失う。
判断の根拠は論点 b-2 の「bind target の TOCTOU」節を参照。

**この方式は論点 b-2 の prep ステップとセットでなければ完成しない。** PR6 で
workspace HOME が volume になると上記の daemon 側 mkdir が効かなくなるので、prep で
`~/.claude`、`~/.claude/skills`、`~/.claude/skills/<name>` を **backend の uid/gid で**
(§D7 のとおり daemon の `os.Getuid()` 由来。 リテラルの 1000 ではない) 先に作っておくこと。
併せて、volume 化後は daemon から `fstat` で所有者を読む今の実装が使えなくなるため、
所有者検証の実現手段 (prep container 内での検証 + 結果の持ち帰り等) を PR6 で決め直すこと。
**検証そのものを落としてはならない** — 落とすと「認証が毎回消える」silent failure が戻る。

**決定 (PR6 で確定・実装済み)**:

| 項目 | 確定値 |
|---|---|
| daemon 側 mkdir (`prepareBindTarget`) | **撤去**。 代わりに marker の `skeleton_dirs` で集合の変化を検出し、変わっていれば init container を再実行する (論点 b-2 の該当節) |
| 所有者検証 | **job container の runner へ移設** (`sandbox.Spec.HomeSkeletonDirs` → `internal/sandbox/runner.verifyHomeSkeleton`)。 「作る主体の中で検証しない」という本節の要求は満たしたうえで、毎 dispatch・engine の自動生成の後・harness と同じ uid という 3 点で daemon 側より強い |
| 集合の比較 | **順序非依存** (`equalStringSets`)。 順序は `embed.FS` の listing 由来で、並び替えは home が必要とするものの変化ではない — 順序に反応させると install 全体の init.sh が無意味に再実行される |
| `skills.MkdirAllNoSymlink` | **残す** (doc comment のみ更新)。 dispatcher の呼び出し元は消えたが、これは `DeployAll` がまだ使っている `openBaseDirSafe` の薄いラッパで、その openat2 walk の symlink 拒否と fstat 所有者読みを覆っているテストはこの関数のテストだけである。 撤去すると**生きている primitive の被覆が消える** |

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

#### 決定 (PR8 で確定・実装済み、2026-07-27)

| 論点 | 確定値 |
|---|---|
| 実行主体 | **daemon**。CLI は host dir を読んで tar を作り、`POST /api/workspaces/{slug}/home/import` に**ストリームするだけ**。上の「host mode CLI からの実行に限定される」は **tar の生成側**にかかる制約であって、engine 操作を CLI に置く理由ではない — marker (`<dataHome>/homes-meta/<slug>.init.json`) は `boid_state` volume の中で CLI から触れず、init flock も identity 生成も daemon 側の不変条件だからである。`boid reap` が engine を直接叩く前例はあるが、あれは「**daemon が落ちていても動く**」ことが要件 (`phase6-container-backend.md` §決定6) で、本件は逆に「**daemon が生きていないと正しくなれない**」 |
| endpoint | `POST /api/workspaces/{slug}/home/import`、`Content-Type: application/x-tar`、body = 非圧縮 tar。他の workspace endpoint と違い `http.MaxBytesReader`(1 MiB) を**かけない** (実測 4.3GB)。body は `io.Reader` のまま `containerBackend.runWorkspaceHomeContainer` の `io.Copy` を通って container の stdin へ渡り、daemon の heap には載らない |
| 前提条件の順序 | 副作用なしで断れるものを全部先に: media type → workspace row の存在 (404) → importer 配線 (501)。この後に初めて volume を壊す |
| media type は**必須** (codex Blocker 1、2026-07-27 修正) | 初版は「宣言されていれば検証する」形で、**未指定は素通しして importer を呼んでいた**。 `curl --data-binary @file` は既定で `Content-Type` を送らないので、**手で叩いたときに最も出やすい形が唯一チェックを飛ばす形**だった。 このルートは body を 1 byte も読む前に home volume を破棄するので、素通しの代償は「型を推測する」ではなく「認証情報を消してから tar で失敗する」。 未指定も誤指定と**同じ 400** で断る |
| in-use 検出 | **`VolumeRemove` の 409 そのもの** (`errdefs.IsConflict` → `dispatcher.ErrWorkspaceHomeInUse` → HTTP 409)。検出と破壊が 1 つの atomic な engine 操作なので、job が走っている workspace は**何も壊れる前に**断られる。事前の `ContainerList` は「より stale になりうるだけ」で保証を増やさないので採らない。`Force` は付けない — 付けると「元々無かった」(404) と「消した」(204) が区別できなくなり、一度も dispatch していない workspace への移行という正常系が潰れる |
| init container の流用 (D3) | **primitive は流用、関数は流用しない**。`createWorkspaceInitContainer` / `clearWorkspaceInitContainer` / `runWorkspaceInitContainer` (→ `runWorkspaceHomeContainer` に一般化、stdin が `io.Reader` になった) をそのまま使い、**決定的 container 名と label も init と共有する** — 共有することで「移行と init が同時に同じ home に書く」ことが engine 側で排他になる (flock は 1 プロセス内でしか効かない)。`RunWorkspaceInit` 自体は流用しない: stdin が shell program か payload か、entrypoint が `bash -s` か `tar` か、が違い、init 側の stage marker / boid 固有 exit code は存在しない stage を指すことになる。network だけは共有しない (init は既定 bridge が要る、こちらは `none`) |
| 展開 argv | `tar --extract --file - --directory <target> --no-same-owner --same-permissions` (`workspaceHomeImportArgv`)。`--same-permissions` が無いと非 root の GNU tar が umask を被せる — `0600` は umask では緩まないが `0666`/`0755` は削れるので、「mode はだいたい合っている」を契約にしないために明示する (mutation で実証: 外すと `0755`→`0705`, `0666`→`0606`) |
| tar の作り方 | `dispatcher.WriteWorkspaceHomeTar` (host 側)。mode 維持 / 所有者は uid=gid=0 に正規化 (どうせ `--no-same-owner` で捨てる + host アカウントを跨境界の artifact に残さない) / symlink は target 逐語 (絶対 symlink も書き換えない) / **hard link は `TypeLink` で dedupe** (node・volta 系は hard link を多用し、実体二重送信は数 GB の無駄) / socket・FIFO・device は skip して**名前を報告** / sparse は dense に読む |
| 安全性 (D4) | source は読むだけ。既定は対話確認 (`--yes`/`--force` で skip)。`--dry-run` は walk だけして「何がどれだけ動くか」を出す。進捗は 2 秒ごとに files/bytes を出す |
| 失敗時 | volume 破棄後に展開が失敗したら **volume ごと削除する** (codex Major、2026-07-27 修正)。 初版は「途中まで展開された volume + marker 無し」を残して「一度も dispatch していない workspace と同じ状態」と称していたが、**同じではない**: 一度も dispatch していない workspace には volume が無い。 途中まで展開された home は init.sh が「空でない home」に対して走る、というのが差で、これは危険な差である — init.sh は契約上冪等で、その素直な書き方は `command -v claude` 型の在庫確認による早期 return である。 `.local/bin/claude` だけ届いて実体が未転送の home はこの確認に **YES と答える**ので、init.sh が exit 0 → daemon が完了 marker を書く → 以後その marker が毎回一致し、**壊れた状態が恒久的に固定**され、ずっと後で adapter の「CLI not found」として出る。 削除しておけば次の dispatch が空から作り直す。 削除自体に失敗した場合は握り潰さず、展開エラーと**両方**を返して `docker volume rm` の手順を案内する (再実行では直らない状態なので)。 cleanup は**呼び出し元の ctx を使わない** (`context.WithoutCancel` + 30s) — 展開が失敗する最頻経路は CLI の切断であり、それは ctx を cancel 済みだから |

##### 移行後の再 init: 2 つの leg と、その順序

PR8 は下の申し送りが要求する 2 手段を**併用**し、順序を「副作用なしで断れる step を先に」で決めた:

1. `RemoveWorkspaceHomeVolume` (= in-use 検出。ここで断れば副作用ゼロ)
2. marker 削除 (leg 2) — **volume を壊した直後**。ここから先は marker が指す home が既に無いので、
   daemon が突然死しても再 init 側に倒れる。失敗しても fatal にせず result で報告する
3. `EnsureWorkspaceHomeVolume` で作り直し (leg 1、新しい identity)
4. tar 展開

step 1 が成功した後は**どの結末でも再 init される**: 全部成功 = marker 無し + identity 新規 /
step 2 だけ失敗 = marker はあるが存在しない identity を指す (leg 1) /
step 3 or 4 が失敗 = marker が無い (leg 2、step 3 失敗なら volume 自体が無いので両方)。

テストは 2 つの leg を**独立に**押さえている (`TestRunner_ImportWorkspaceHome_ReInitsWithTheMarkerRemovalUNDONE` /
`..._ReInitsWithTheVolumeRecreateUNDONE`): 片方を undo して、もう片方だけで init が走ることを見る。
mutation proof も取ってある — volume 作り直しを外すと leg 1 のテストだけが赤、marker 削除を外すと
leg 2 のテストだけが赤、両方外すと受け入れテスト (`..._NextDispatchRunsInitExactlyOnce`) が赤になる。

##### dispatch との排他 (codex レビュー Blocker 2、2026-07-27 修正)

flock は `resolveWorkspaceHome` の **fast path (marker 一致) では取られない**。
つまり settled な workspace の dispatch は「home を resolve してから container を作るまで」
**何のロックも持っていない**。そこに移行が入ると:

```
dispatch : volume A を resolve、identity A を観測          ...
移行     :                    A を削除 → B を作成 → B へ展開開始
dispatch :                             ... ensureNamedVolumes(名前) → B
                                           → ContainerCreate(B)
両方     :                                 agent と tar が B に同時書き込み
```

**初版はこの窓を「`verifyWorkspaceHomeIdentity` が覆っており job 側が大声で失敗する」と書いていたが、
これは誤り**だった。あの再検査は `ensureNamedVolumes` の `VolumeCreate` が返した identity を
resolve 時の値と突き合わせるもので、**`ContainerCreate` と atomic ではない**。
再検査の *前* に入れ替わった場合しか捕まらず、*後* に入れ替わった場合は名前で mount されるだけで
比較すべきものが残らない。 実装側の doc comment (`ensureNamedVolumes` の
「the window it narrows without closing」) は元々そう認めており、誤っていたのは本 doc の結論の方。

**確定した対処**: daemon プロセス内の in-flight 登録簿 (`dispatcher.workspaceHomeInFlight`、
mutex + slug ごとの map)。

| | 挙動 | 理由 |
|---|---|---|
| dispatch 側 | `resolveWorkspaceHome` を呼ぶ **前**に登録し、`Dispatch` の `defer` で解放 (= `Launch` 完了後) | 登録を resolve の後にすると resolve 自身が窓の中に残る。 解放が `defer` なのは、この区間に十数個の失敗 exit があり、1 つでも漏らすと以後その workspace の移行が daemon の寿命いっぱい拒否され続けるから |
| 移行側 | 同じ mutex を取り、in-flight な dispatch があれば **断る** (`ErrWorkspaceHomeBusy` → HTTP 409) | 移行は端末の前にいる operator の操作で、拒否は volume に触る前に起きるので**再実行 1 回**しか失わない |
| 移行中に来た dispatch | **待つ** (ctx でのみ中断) | dispatch は hook 評価から来る。 拒否 = **job の失敗**で task 状態に記録が残る。 しかも marker 不一致の dispatch は元々同じ flock で待っており、fast path が待たなかったのは lock を取っていなかったからに過ぎない |
| 移行 vs 移行 | 断る | 元々 flock で直列化はされていたが、2 本目の HTTP request が**無言で数分ブロック**するだけだった |

**なぜ flock を延ばさなかったか**: 両者とも同一プロセスの goroutine であり (移行は daemon の
HTTP route にしか存在しない)、file lock が要る相手は居ない。 一方で fast path にも lock を取らせて
`Launch` まで保持すると、project registry で守られた dispatch 区間全体 (`Runner.WithProjectLock` =
`api.ProjectAppService.projectMu` や git gateway の staging と交差する) に file lock を跨がせることになり、
どちらの側を読んでも順序が確定できなくなる。

##### `workspace remove` との排他 (codex round 2 Major 1、2026-07-27 修正)

初版は「削除しかしない経路はわざと対象外」に `boid workspace remove` を含めていた。
この根拠は **dispatch 側については正しく、移行側については盲点**だった。
`workspace remove` は **DB row を先に消し、home volume を後で消す**。 一方 import 側は
handler で `GetWorkspace` を通した後、その結果を保持せずに移行を始める。 したがって:

```
import : GetWorkspace("myws") → ok
remove :                        row を削除 → volume を削除
import :                                              volume 削除 (不在 = 正常系) →
                                            作成 → 認証情報を展開
import : 200 OK
```

残るのは **workspace row の無い、認証情報入りの home volume**。 どの dispatch も mount せず、
`boid gc` には orphan として出て、`boid workspace remove` でも消せない (row が無いので 404)。
これは API 層の doc comment が「防ぐ」と書いていた状態そのものである。

**確定した対処は 2 つで、どちらか片方では閉じない**:

| | 内容 |
|---|---|
| ① 登録簿を remove にも広げる | `workspace remove` は **row 削除と volume 削除の両方**を `beginRemoval` で囲む。 移行中なら **409 で断る** (待たない): 移行は 4.3GB の展開で分単位、`workspace remove` を無言で数分待たせるより、何も壊す前に断って再実行させる方が良い。 逆に移行側は removal が in-flight なら断る。 **dispatch は removal で待たせない** — removal は「消すだけ」で、それは `verifyWorkspaceHomeIdentity` が本当に覆う側であり、待たせると「job が走っている workspace の remove」の既定の part-completed 挙動が硬い拒否に変わってしまう |
| ② 移行が **登録簿を保持したまま** row を再確認する | ①だけでは「remove が **import の登録より前に開始し終了した**」順序が残る (登録簿に見えるものが無い)。 `Runner.ConfirmWorkspaceExists` (`server/wire.go` が `ProjectAppService.GetWorkspace` を closure で渡す) を `beginMigration` の**直後**に呼ぶ。 この時点以降 removal は拒否されるので、答えが陳腐化しない。 **登録の前に呼ぶと窓が狭まるだけで閉じない** — mutation proof 済み (`..._ConfirmsTheWorkspaceUnderTheRegistration` が赤くなる) |

##### daemon の強制終了に対する耐性 (codex round 2 Major 2、2026-07-27 追加)

上の「失敗時」の cleanup は `ImportWorkspaceHome` が**エラーを返した**場合しか走らない。
SIGKILL / OOM kill / `docker compose down -t 0` / 電源断は戻り値を作らないので defer も cleanup も
走らず、**途中まで展開された volume + marker 無し**という、まさに round 1 で潰した状態が残る。
起動時の orphan sweep も救わない — あれは **container** を label で reap するもので、
生き残るのは **volume** の方である。

**確定した対処**: `<dataHome>/homes-meta/<slug>.migrating.json` に「移行中」の sentinel を置き、
daemon 起動時 (`buildRuntime` の startup reap の直後、auto-reopen の**前**) に
`Runner.SweepInterruptedWorkspaceHomeMigrations` が掃く。

- **volume の label では表現できない**ことを確認済み: Engine API に**既存 volume の label を
  更新する手段が無い** (`VolumeCreate` は既存名に対して「そのまま返す」— これは
  `ensureWorkspaceHomeVolume` の fast path が乗っている性質そのものであり、
  だからこそ label 無し volume は「再 init で直る」ではなく hard error になっている)。
  volume **名**に状態を載せる案は、dispatch が mount しない volume に展開することになるので却下。
- **順序** (round 4 で phase を挟む形に改訂。 下の「record に phase を持たせる」を参照 —
  以下は round 2 時点の記述として残す):
  `record 書き込み → volume 削除 → marker 削除 → volume 作成 → 展開 → record 削除`。
  record は「最初の破壊の**直前**」に書き、「最後の建設の**直後**」に消す。
  書き込みは marker と同じ temp+rename+**fsync** (`writeWorkspaceHomeJSONFile` に括り出した) —
  電源断を跨ぐのが仕事なので page cache 止まりでは記録にならない。
- **どの時点で落ちても「削除して init から作り直す」側に倒れる**。 record を書く前に落ちれば
  何も壊れていない。 record を書いた後・削除前に落ちると、**無傷の home も消す**ことになるが、
  これは安全側である —— **ただしこの over-reach は round 4 で phase を入れて解消した**
  (無傷と分かっている record は home に触らない)。 以下はその判断が必要だった当時の根拠であり、
  phase が「判断できない」に落ちる場合の fallback の根拠として今も有効である: (a) operator はその home の置き換えをまさに依頼した後であり、
  (b) 移行元は読むだけで消していないので再実行で完全に戻り (それは元々あらゆる失敗時の
  復旧手順)、(c) 逆に「残す」側に倒すと、在庫確認型の冪等 init.sh が途中 home を
  「もう入っている」と誤判定して完了 marker を書き、**壊れた状態が恒久固定**される。
- **副作用ゼロの拒否 (in-use 409 / busy / row 消失) では record も消す** — 何も壊していない
  workspace の home を次回起動が消してしまうため (round 4 以降はこの unlink が失敗しても
  phase が `recorded` のままなので安全側に倒れる。 **round 4 の Blocker 1 経路 B は
  まさにこの unlink の失敗**だった)。 逆に **partial cleanup が失敗した場合は
  record を残す** ので、手で消さなくても次回起動が引き取る。
- 残る窓は「展開完了 ～ record 削除」の 1 unlink 分 (round 4 で「展開完了 ～ `home-rebuilt` の
  rename」に縮んだ)。 数分の操作に対して無視できる幅で、代償は再実行 1 回。
  **record 削除に失敗したとき**は API/CLI が warning でファイル名を出す
  (round 4 以降、文言は `migration_record_discards_home` で分岐する)。
- テストは実際の移行を展開中に gate して meta dir を snapshot し、移行が (Go の defer を
  飛ばせないので) 正常に巻き戻った後にその snapshot を書き戻すことで、
  **SIGKILL 直後の on-disk 状態を実装から採取して**再現している。

##### engine cleanup の deadline (codex round 2 Major 3、2026-07-27 修正)

展開 container の `defer ContainerRemove` は cancel 済み ctx を捨てて `context.Background()` を
使っていた。 cancel を落とすのは正しい (展開失敗の最頻経路が CLI 切断なので、
呼び出し元の ctx を使うと cleanup がまさに必要な場面で失敗する) が、**deadline が無かった**。
accept したまま応答しない engine socket に当たると defer が永久停止し、
`ImportWorkspaceHome` はエラーを返さない → 30s bound の volume cleanup に到達しない →
`doneMigration` も走らないので同 workspace の後続 dispatch が待ち続ける。

`containerCleanupContext` (= `WithoutCancel` + 30s) に統一し、grep で同種を洗って
**init container の defer** (同じ形。しかも呼び出し元は init flock を保持しているので、
落ちるとその workspace の全 dispatch が syscall で固まる) と
**`Launch` の 3 つの teardown 経路** (spool open / attach / start 失敗) にも適用した。
`waitLoop` の exit 後 remove も同じ形にした (呼び出し元をブロックはしないが、
「この package の teardown remove には必ず deadline がある」を file の性質にするため)。
`ContainerWait` は本質的に無期限なので対象外。

##### それでも閉じていない窓 (意図的)

- **別プロセス**。 登録簿は daemon プロセス内。 同一 engine に対する 2 つ目の daemon は依然として
  素通りする (flock は slow path のみ、従来どおり)。 1 install = 1 daemon が前提なので許容する。
- **削除しかしない経路** (`boid reap --include-workspace-homes` /
  operator の `docker volume rm`) は**わざと対象外**。 `boid workspace remove` は
  round 2 まで ここに書かれていたが、上記のとおり**対象に含めた** (理由は dispatch 側ではなく
  移行側 — remove は破壊者ではなく被害者で、移行が volume を作り直す先の row を消してしまう)。 これらは消すだけで、同じ名前で作り直して
  書き込むことはない。 home を消された dispatch は自分の `ensureNamedVolumes` が作った
  fresh volume の identity 不一致で **loud に落ちる** — こちらは `verifyWorkspaceHomeIdentity` が
  本当に覆っている場合である。 移行だけが「削除 → 同名で再作成 → 書き込み」をやるので、
  dispatch が検査する前に検査を通ってしまう volume を作れる。
- `ensureNamedVolumes` と `ContainerCreate` の**間**に削除が入る窓は、上記のどの経路からも残る
  (engine が mount 用に unlabeled volume を暗黙作成し、job が空 home で走る)。 本 PR で変わっておらず、
  PR1/PR6 が元々記述していた残余窓と同じもの。

##### 中断された移行の残骸は dispatch 側でも封じる (codex round 3 Blocker 1、2026-07-27 修正)

起動時 sweep (`SweepInterruptedWorkspaceHomeMigrations`) だけでは**不十分だった**。
sweep は volume 削除に失敗しても `defer done()` で登録を解放し、`buildRuntime` は
その error を**ログに出して startup を続ける** (reap と同様、reconciliation の失敗で
daemon 起動を潰さないため)。 したがって次の 1 本の道筋だけで、2 度目の crash 無しに
機構全体が無効化される:

```
移行中に kill → 途中まで展開された volume + record、marker 無し
起動 (engine が一時的に不調) → sweep の VolumeRemove が失敗、record は残り startup 継続
engine 復旧 → sweep を再実行するものが無い
auto-reopen / 通常 dispatch → in-flight 登録簿は空なので誰も待たない
                            → 途中の home に init.sh が走る
```

在庫確認型の init.sh は「もう入っている」と答えて exit 0 → daemon が完了 marker を書く →
**壊れた状態が固定**され、ずっと後に adapter の「CLI not found」として出る。

**確定した対処**: 不変条件を「**record がある間、その home に init も job も走らせない**」と
言い直し、`resolveWorkspaceHome` の先頭 (volume を ensure する前) でも record を見る。

- **見つけたら repair する** (案 (a))。 拒否 (案 (b)) も健全だが、実際に起きるケースで劣る:
  record が残る理由は typically「engine が一時的に落ちていた」で、job が来る頃には
  engine は戻っている。 拒否は一時障害を「人間が volume 名を調べて手で消すまで
  その workspace が使えない」に変換するが、repair は「init が 1 回余分に走る」に変換する
  — この file が他の全箇所で既に受け入れているコスト。
- **discard は sweep と同一実装** (`discardInterruptedWorkspaceHomeMigrationLocked`、
  volume → marker → record の順)。 dispatch が治した workspace と boot が治した
  workspace が違う状態になってはいけない。
- **engine がまだ応答しないなら dispatch は失敗させる**。 job 1 本の失敗は task に残り
  再実行できるが、途中の home に書かれた完了 marker は恒久的で、しかも別の場所に出る。
  error には volume 名と `docker volume rm` を入れる。
- 排他は既に揃っている: dispatch は `beginWorkspaceHomeDispatch` の登録を持っており、
  それが移行を排除する。 ここで `beginMigration` を取ると**自分の dispatch 登録と
  deadlock する**ので取らない (flock だけ取る)。
- 通常経路のコストは「存在しない path への `os.Stat` 1 回 / dispatch」。

##### sentinel の durability は fail-closed (codex round 3 Major、2026-07-27 修正)

「durable な record」を不変条件にしておきながら、共有 writer
(`writeWorkspaceHomeJSONFile`) が**親ディレクトリ fsync のエラーを全部捨てて成功を返して
いた**。 失敗しているストレージ上では「durable でない sentinel を持ったまま home volume を
破壊する」ことになり、電源断で rename が失われれば**途中の volume に sentinel が無い** ——
この機構が消そうとしていた状態そのものが再現する。

| 対象 | policy | 理由 |
|---|---|---|
| 完了 marker | **best-effort** (`homesMetaBestEffortDurability`) | marker がこの writer に求めるのは**原子性** (temp + rename) であって durability ではない。 rename が飛んでも「冪等な init.sh が 1 回余分に走る」で、これは marker の不確実性を常に解決している向きと同じ。 ここで hard fail させると、ストレージの一時不調が**その workspace の dispatch 失敗**に化ける (防ごうとしている結果より悪い) |
| 移行中 record | **required** (`homesMetaRequiredDurability`) | 呼び出し元の**次の文が home volume の破壊**。 ここで止めれば何も壊れていないので、operator は同じ workspace を持ったまま、ストレージが直ってから再実行できる |

削除側 (`removeWorkspaceHomeMigrationRecord`) も **unlink 後に親 dir を fsync する**。
unlink が電源断で巻き戻ると record が復活し、**完璧に移行できた home を次回起動の sweep が
破棄する** (数 GB + operator の午後)。 失敗は握り潰さず
`MigrationRecordRemoveError` として報告する (= 再起動前に該当ファイルを消せる)。
「元々無い」場合は sync せずに成功を返す (消すものが無いので、冪等な呼び出し経路に
失敗モードを増やすだけになる)。

fsync を失敗させる移植可能な方法が無いので、`fsyncDir` を package 変数にして
テストから差し替えている。

##### 空の tar と symlink の `--from` (codex round 3 Blocker 2、2026-07-27 修正)

`--from` が**ディレクトリへの symlink** だと、CLI と tar writer の事前検査
(どちらも symlink を辿る `os.Stat`) は「ディレクトリだ」と答えるのに、
`filepath.WalkDir` は**ルートの symlink を辿らない** — callback は `path == root` で
無条件に skip されるので、**entry 0 件の tar が正常終了する**。
daemon は body を読む前に volume を破棄するので、結果は
「**空 volume に空 archive を展開して 200 を返す**」+ CLI が「imported」と表示。

2 段構えで直した:

1. **入口で正しく判定する**。 `ResolveWorkspaceHomeSource` が `os.Lstat` でルートを見て、
   symlink なら `EvalSymlinks` で**解決して walk する** (拒否ではなく解決を選んだ:
   別ディスクの home を symlink で指すのは普通の運用で、他のツールも辿る)。
   解決先は確認プロンプトと `--dry-run` に `typed (-> resolved)` の形で出す。
   **末尾コンポーネントだけ**解決する — 途中の symlink は WalkDir が正常に扱う。
   home の**中**の symlink は従来どおり symlink のまま送る (home 自身の構造だから)。
2. **「移行対象が 0 件なら破壊しない」**。 原因は symlink 以外にもある (typo、
   未マウントの backup、空ディレクトリ)。 結果が同じなので**結果の側で**断る:
   - **CLI 側**: `WorkspaceHomeSourceHasEntries` (最初の 1 件で `fs.SkipAll` するので
     実質 readdir 1 回)。 1 byte 送る前・確認プロンプトより前。 `--dry-run` でも同じく断る
     (dry-run は「本番が何をするか」を予告するものなので)。
   - **daemon 側**: `peekWorkspaceHomeTarHasEntries` が body の**先頭 1 block (512B)** を
     見て、全部ゼロなら `ErrWorkspaceHomeImportEmpty` → **HTTP 400**。 読んだ block は
     `io.MultiReader` で**replay する**ので展開側は byte 0 から受け取る
     (`..._StillMigratesTheFirstEntry` が vacuity guard)。 これが**破壊と同じ側にある
     唯一の guard** なので、古い CLI・手書き curl・producer 側の次のバグにも効く。
   意図的に空の home にしたい場合は `boid workspace remove` + 再作成で表現できるので、
   拒否して問題ない。

##### record に phase を持たせる (codex round 4 Blocker 1、2026-07-27 修正)

round 3 で「record がある間その home には何も走らせない」「dispatch が record を見つけたら
その場で volume を discard する」を入れたが、**record が残る理由は「移行が中断した」だけでは
ない**。 record は「移行を開始した」しか意味しておらず、recovery はそれを無条件に
「破壊済み」と読んでいた。 実際に起きる残り方は 2 つあり、**どちらも home は無事**である:

| | 経路 | round 3 での帰結 |
|---|---|---|
| A | 展開が**完了**し、最後の unlink が失敗する (homes-meta が書けない / read-only remount / EIO)。 API は 200 を返す | 次の dispatch が、移行済みの認証情報と toolchain を含む volume を破棄する |
| B | step 1 の in-use 409 (= **何も壊していないことが保証された唯一の拒否**) の後、cleanup の unlink が失敗する。 error はログに捨てられる | 利用中の job が終わった後に来た dispatch が、旧 home を破棄する |

**確定した対処**: record に `phase` を持たせ、recovery は**ファイルの存在ではなく phase**で
判断する。

| `phase` | 書く時点 | recovery |
|---|---|---|
| `recorded` | 何も壊す前 (= record 作成時) | record を消すだけ |
| `home-destroyed` | `VolumeRemove` が**成功した**時点 | volume・marker・record を削除 |
| `home-absent` | volume が**無いことを確認した**時点 (round 5) | record を消すだけ |
| `home-rebuilt` | 展開が完了した時点 | record を消すだけ |
| (無い / 未知) | round 4 より前の build が書いたもの | **`home-destroyed` として扱う** |

- **順序**: `"recorded" 書き込み → volume 削除 → "home-destroyed" 書き込み → marker 削除
  → volume 作成 → 展開 → "home-rebuilt" 書き込み → record 削除`。
  この線から早く抜ける経路は `"home-rebuilt"` の代わりに `"home-absent"` を書いてから
  record を消す (round 5、下記)。
- **round 2 の「記録された区間の方が広い」性質は保たれる**。 ただし区間の言い方を
  正確にする必要がある: 危険なのは「home が無い区間」ではなく
  「**その名前の volume が、誰も保証していない中身を持って存在する区間**」である。
  それは step 3 (volume 再作成) に開き step 4 (展開) の復帰で閉じる。 `home-destroyed` は
  **step 3 の前**に commit され **step 4 の後**にしか書き換えられないので、両端で厳密に広い。
  `VolumeRemove` 成功から `home-destroyed` 書き込みまでの隙間で落ちた場合は
  **volume が存在しない**ので、これは「一度も dispatch していない workspace」と同じ状態
  であり、次の dispatch が普通に作り直す。
- **phase 更新も fail-closed** (round 3 で record 作成に入れたのと同じ扱い)。
  `home-destroyed` が page cache 止まりのまま展開に進むと「途中の home + 何も壊していないと
  言う record」になり、recovery がそれを init.sh に渡してしまう。 書けなかった場合は
  **そこで中止する** — volume は既に消えているので home は空になるだけで、次の dispatch が
  作り直す。
- **`home-rebuilt` だけは fatal にしない**。 これが守るものはもう無い (移行は成功しており
  home は完全) ので、失敗させると無事な workspace に対して障害を報告することになる。
  代わりに**閉じられたかどうかを API/CLI に出す** (`migration_record_discards_home`):
  false なら「次回起動はファイルを消すだけ・対処不要」、true なら round 3 までと同じ
  「次回起動でこの home が破棄される・再起動前に消せ」。 安いケースで重い文言を出すのは
  無害ではない — compose deploy ではそのファイルは daemon の volume の中で、
  operator は到達すらできないので、**本当に効くべき warning を無視する習慣**がつく。
- **未知の phase が `home-destroyed` に倒れる**のは後方互換と fail-safe の両方から。
  round 3 までの record は「移行を開始した」= discard 許可の意味だったし、
  「boid が判断できない」ときの向きは marker の不確実性と同じ側 (over-reach = 再 init 1 回)
  で良い。 この build が書く record は 3 つとも phase を明示するので、この arm には落ちない。
- recovery が「discard を許可しない record」を処理する経路は **engine を一切呼ばない**ので、
  backend が `WorkspaceHomeImporter` を実装していなくても片付く。 importer の解決を
  **lazy** にしたのはこのため (up-front だと、無害な record が
  「この backend では移行できない」を理由にその workspace の全 dispatch を失敗させる)。
- テストは**実装が最後に書いた record を採取して**書き戻すことで「unlink が失敗した」を
  再現する (`migrationRecordTrace`、seam は既存の `fsyncDir`)。 手書きの record だと
  「daemon がもう書かない state」を検査してしまう。 vacuity guard は
  **展開中に採取した record** を書き戻して round 3 の挙動 (discard する) が
  残っていることを確認する。

##### homes-meta 自体の durability (codex round 4 Blocker 2、2026-07-27 修正)

record writer が fsync するのは `homes-meta` **自身**だけだった。 ディレクトリの
directory entry は**その親**にあるので、一度も dispatch していない workspace を移行すると
`MkdirAll` → record 書き込み → volume 作成 → 展開途中で電源断、で
**`homes-meta` の生成だけが失われ partial volume は残る**。 起動 sweep は meta dir 不在を
「中断なし」と読み、dispatch が meta dir を作り直して record の無い partial home に
init を実行する — sentinel が防ぐはずの状態そのもの。

`mkdirAllDurable` を追加し、**作成した各階層の親** (`MkdirAll` は途中の階層も作りうる) と、
**`path` の直接の親を無条件に** fsync する。 後者は belt-and-braces ではない:
`homes-meta` は `resolveWorkspaceHome` と `ImportWorkspaceHome` の**先に来た方**が作り、
前者の `MkdirAll` は durable でない (そこに書くのは best-effort policy の完了 marker だけ)
ので、「既にあった」は「既に durable」を意味しない。 それより上へは辿らない —
`<dataHome>` は `boid.db` を含む長命なレイアウトで、SQLite の書き込みが何度もその entry を
flush している。

##### 起動時 sweep の engine 呼び出しに deadline (codex round 4 Major、2026-07-27 修正)

`buildRuntime` は `context.Background()` を渡し、それがそのまま `VolumeRemove` に渡っていた。
moby client 自身は timeout を持たないので、接続だけ受けて応答しない engine socket に当たると
**daemon は sweep 内で永久停止し、auto-reopen にも API listener にも到達しない**。

- **bound は sweep の中に置く** (`workspaceHomeMigrationSweepTimeout`)。 「起動がこの step で
  止まらない」は step の性質であって呼び出し元の性質ではないし、`buildRuntime` に
  渡せるより良い ctx は無い。
- **volume ごと**にした。 sweep 全体で 1 つだと、engine に到達できない 1 件が予算を使い切って
  後続が既に expire した ctx で即失敗し、その残骸は「最後の砦」である dispatch 側に回る
  (eager に掃く意味が消える)。 総時間は `件数 × 30s` になるが、件数は install の規模の関数では
  なく「**operator が手で始めた移行が中断され、その後どの起動も dispatch も片付けていない
  workspace**」の数なので、実質 0、たまに 1 である。
- **timeout した場合は record が残る** — timeout した `VolumeRemove` は失敗した
  `VolumeRemove` であり、phase も書き換えない (sweep が phase を書くのは
  **削除に成功した後の `home-absent`** だけで、timeout はそこに到達しない)。
  `home-destroyed` のままなので次回起動と次の dispatch が引き取る。 逆に非破壊 phase の
  record は engine を呼ばないので、応答しない engine に取り残されることが原理的に無い。

##### unlink は durability の手段ではない (codex round 5 Blocker、2026-07-27 修正)

round 4 は**成功経路**にだけ「unlink の前に非破壊 phase を commit する」を入れ、
**失敗経路は `home-destroyed` のまま unlink する**ままだった。 これが安全でないのは
移行中の crash とは無関係な理由による: `removeWorkspaceHomeMigrationRecord` は
**先に unlink し、その後で directory を fsync する**ので、失敗の仕方が厄介な向きになる ——
**このプロセスにも以後の dispatch にも record は「消えた」と見えるが、消えたままである保証が無い**。

```
volume が無いことは確定・record は "home-destroyed" のまま
unlink 成功、その directory fsync が EIO   -> ログに出して捨てる (sweep 経路では握り潰し)
次の dispatch は record を見ない            -> init.sh で正常な home を作る
電源断で unlink が巻き戻る                  -> durable な "home-destroyed" が復活
次回起動の recovery が phase に従う          -> その正常な home を破棄する
```

第 2 の bug も、移行中の crash も要らない。 対処は round 4 と同じ形を**volume が
「壊れている」ではなく「無い」で終わる経路**に適用すること:

- **`home-absent` を新設**した (`recorded` / `home-rebuilt` の使い回しではなく)。 recovery の
  **動作**は 3 つとも同じ (record を消すだけ) なので、分ける理由は record が**何を主張するか**
  にある —— record のもう 1 つの仕事は「incident 後に operator が読む」ことで、
  `recorded` は「何も壊していない」、`home-rebuilt` は「完全な home がこの名前で存在する」と
  主張してしまい、どちらもこの状況では偽。 「volume が無い」と「volume に正常な中身がある」を
  recovery が区別する必要は**今は無い** (`discardsHome` は両方 false) が、両者が互いの事実を
  主張しないので、後で区別が要る変更が来たときに record から再導出せずに済む。
- 適用箇所は 3 つ: `ImportWorkspaceHome` の `windowErr` 経路と step 4b の cleanup
  (どちらも `clearWorkspaceHomeMigrationRecordForAnAbsentHome` 経由)、および
  **recovery 自身の discard** (`discardInterruptedWorkspaceHomeMigrationLocked`)。
  3 つ目は dispatch 経路だけなら不要だった (unlink の error を返すので、消せなかった record の
  裏で home が作られることはない) が、**sweep は buildRuntime が意図的にログして続行する**ので
  そこだけ穴が開いていた。
- **非破壊 phase の書き込み自体が失敗したら record は残す** (unlink しない)。 非対称性が
  決め手: **見えている** record は当該 workspace の全 dispatch を
  `recoverInterruptedWorkspaceHomeMigration` で止め、volume が作られる前に解決される ——
  そして存在しない volume に対する解決は**ただ**である (`VolumeRemove` の NotFound は成功扱い、
  marker 削除は ENOENT 許容)。 一方 **unlink 済みだが flush されていない** record は
  「dispatch が正常な home を作る」時間だけ見えず、その後で復活する。 残した場合の最悪の
  durable 結果は「存在しない volume を指す `home-destroyed` record」で、これは無害。
- 旧 build (round 4) が `home-absent` を読むと未知 phase → discard に倒れるが、これも
  上と同じ理由で無害 (指している volume が存在せず、recovery は volume 作成より前に走る)。

#### PR8 への申し送り: 移行後に init を必ず 1 回走らせるには

> **PR8 で消化済み (2026-07-27)**。下の要求どおり 1 と 2 を併用した。実装と順序、
> および 2 つの leg を独立に検証するテスト / mutation proof は上の「決定 (PR8 で
> 確定・実装済み)」を参照。本節は要求の記録としてそのまま残す。

> **PR6 で全面書き直し (2026-07-27、codex レビュー Minor 2)**。初版は
> 「移行の tar から `<home>/.boid-workspace-home-id` を除外せよ」と要求していたが、
> **このファイルは PR6 で撤去済み**で、identity は volume の label
> (`boid.workspace_home_id`) に移った。除外すべきファイルは存在しないので、
> 初版の推奨 1 は PR8 では **no-op** である。以下が現行の前提での申し送り。

**現行の skip 条件** (`dispatcher.workspaceHomeInitialized`) は 5 つの AND である:

1. `script_sha256` が現在の init.sh の内容と一致
2. `init_generation` が現在値 (**2**) と一致
3. `home_id` が空でない
4. `home_id` が **volume が今実際に持っている `boid.workspace_home_id` label** と一致
5. `skeleton_dirs` が現在の binary の集合と一致

**PR8 が踏む穴**: 移行は「中身のコピー」なので、上の 5 つの**どれも変化させない**。
すでに PR6 後に一度 dispatch 済みの workspace は
(identity = A の volume + `home_id` = A の marker + 世代 2 + 現行 skeleton + 同じ init.sh) で
安定しているので、そこへ host 時代の中身を流し込むと **5 条件すべてが一致したまま**になり、
**次の dispatch は init を skip して host 時代の焼き込みがそのまま生き残る**。

- identity では検出できない。identity は**入れ物の incarnation** を指しており、
  中身の入れ替えは原理的に見えない (`workspaceHomeMarker.HomeID` の
  「No longer detected」の項に同じことが書いてある)。
- 世代でも検出できない。PR6 が既に **2** に上げているので、移行対象の marker は現行世代である。
  PR8 でさらに bump すると **install 全体の全 workspace が再 init される** ので、
  移行した 1 workspace のために払うコストとして過大である。

**したがって PR8 は次のどちらかを、移行 CLI 自身の中で必ず行うこと** (「何もしない」は選択肢に入らない):

1. **移行先 volume を作り直す** — 既存 volume を `docker volume rm` してから
   `EnsureWorkspaceHomeVolume` で作り直し、そこへ tar を流し込む。新しい incarnation は
   新しい identity を持つので条件 4 が落ち、再 init が走る。
   volume ごと差し替えるので「旧 incarnation の残骸が混ざる」ことも同時に防げる
2. **移行後に marker を削除する** — `<dataHome>/homes-meta/<slug>.init.json` を消す。
   次の dispatch が marker 不在から再 init する。失うのは `boid_version` / `completed_at` の
   履歴だけで、**移行対象の 1 workspace にスコープが閉じている**のが利点

**1 と 2 の併用を推奨する**。1 は「入れ物ごと新しくする」という移行の意味論に忠実で、
2 は 1 を省いた場合 (既存 volume に上書きしたい、rm する権限が無い) の保険になる。
どちらか一方でも十分だが、marker と volume の寿命が別である以上、
**両方揃えて初めて「どちらの順序で失敗しても再 init に倒れる」**。

**PR8 を経由しない経路** (手動 `docker cp`、backup からの復元) には自動検出が無い。
これは実装漏れではなく境界そのものである — daemon は volume の中身を見られないので、
「入れ物は同じまま中身が入れ替わった」ことを知る手段が無い。
該当する操作をした運用者が **marker を手で消す**しかなく、
その旨を `docs/{ja,en}/guide/workspace-home.md` に書くのが PR8 の受け入れ条件に含まれる。

**どの案でも移行直後に init.sh が 1 回走る**ことは PR8 の受け入れ条件として明示すること。
1.5GB の toolchain が既にコピー済みなので、冪等な init.sh なら短時間で終わる。
逆に「移行したのに init が 1 度も走らない」場合は上記の穴が開いている。

## PR 分割案

| PR | 内容 | 挙動変化 | 依存 |
|---|---|---|---|
| 1 | **[landed]** volume 破壊経路 **7 つ**の封じ込め: 論点 e = (i) 採用 + `internal/dockerres` 新設 (label / prefix / 命名の単一出典) + `ensureNamedVolumes` の label 分離・warning 条件見直し・volume 名 validation + `reap.Run` の **2 本立て volume 列挙**・name 除外・`WorkspaceHomePolicy` + `boid reap --include-workspace-homes` 契約宣言 + `reapOrphanVolumes` の name/label 二重除外 (label は presence 判定) + dockerproxy policy の `boid-ws-` volume create/delete deny・**`/volumes/prune` deny**・**`Mounts[].Source` deny**・**`VolumesFrom` deny** + `dockerproxy.Reap` 側 name-prefix 除外 | sandbox 内 `docker volume prune` と `docker run --volumes-from` が 403 になる (それ以外は無し — 対象 volume がまだ存在しない) | — |
| 2 | **[landed]** marker/lock **+ init.sh 実行用一時ファイル**を daemon 永続領域 (`boid_state` = `dataHomeFor(cfg)`) の `homes-meta/` へ移設。**`dataHomeFor(cfg)` → `WireConfig.DataHomeDir` → `Runner.DataHomeDir` の配線を 1 本新設** (`Runner` は永続領域を知らなかった)。**＋ 論点 b の nonce (marker `home_id` ↔ `<homeDir>/.boid-workspace-home-id` の突合) を同 PR で導入** (初版は PR5/PR6 に割り当てていた。移した理由は論点 b 参照)。**＋ nonce の読み取り安全化** (`O_NOFOLLOW` / `O_NONBLOCK` / regular file 確認 / 1 KiB 上限。job が nonce を FIFO や symlink に置換して daemon を止められた経路を塞ぐ — 論点 b 参照)。**＋ `testutil/homeenv` によるテスト隔離** (`internal/dispatcher` / `internal/server` / `cmd` の `TestMain`) | 置き場 + 突合。**旧 marker (置き場も `home_id` も違う) は読まないので upgrade 後 workspace ごとに init.sh が 1 回だけ再実行される** (冪等前提で吸収)。旧 marker の削除はしない。以後は **HOME が消えれば marker があっても再 init される** | — |
| 3 | **[landed]** skills の materialize 先を host-visible runtime dir (`<runtimesRoot>/skills`) へ + `$HOME/.claude/skills/<name>` へ **skill ごとに RO bind** + bind target の daemon 側 mkdir (`skills.MkdirAllNoSymlink`、PR5 の prep に移る暫定) + `internal/server/server.go` の dead な `DeployAll` 撤去 | workspace HOME に skill の実体を置かなくなる (既存 install の `<home>/.claude/skills/<name>` は RO bind に覆われて見えなくなるだけで、削除はしない)。 sandbox 内で skill が **RO** になる (従来は rw、job が書き換えても次 dispatch で復元されていた)。 `<dataHome>/skills` が更新されなくなる (読み手はいない) | — |
| 4 | **[landed]** `WorkspaceSlug` を独立に thread する: `resolveWorkspaceHome` の戻り値を `(homeDir, slug, error)` にして、正規化 (`normalizeWorkspaceSlug`) が起きるその場から slug を返す。`Dispatch` はそれを `SandboxRuntimeInfo.WorkspaceSlug` へ流し、`runner.go` の `filepath.Base(workspaceHomeDir)` 依存を撤去 (slug の計算箇所は 1 つのまま — 2 箇所で独立に計算する形にはしない)。**＋ home dir 名を決める 1 行を `workspaceHomeDirFor` へ切り出す**: PR6 が volume 名 (`boid-ws-home-<installID8>-<slug>`) に差し替える地点であり、同時に**「basename ≠ slug」の状況をテストが作れる唯一の seam** — 現行レイアウトでは両者が一致するので、これが無いと「パスから導出していない」ことを検証するテストが tautological になり退行を検出できない | 無し (同じ値が別経路で渡るだけ) | — |
| 5 | **[landed]** init.sh + prep を使い捨て container 実行へ (論点c §D1-§D10: 既定 bridge / stdin + quoted heredoc / 決定的 container 名による多重実行防止 / prep skeleton mkdir と nonce 書き込みを prelude・postlude として統合 / 専用 label + `ReapOrphans` の別ループ / `WorkspaceInitExecutor` の型アサーション)。**現行の host-visible homes dir を engine bind して検証する**。`prepareBindTarget` の daemon 側 mkdir は**残した** (理由は論点c の「意図的に残したもの」)。**＋ marker の `init_generation`** (論点 b の該当節) — 無いと新経路が既存 workspace で一度も走らない | **init.sh の実行環境が変わる** (host → container)。`$HOME` の値・env 一覧・`$0`・素通し workspace の扱い・エラー文面が変わる — 一覧は論点c の「PR5 が持ち込む挙動変化」。**既存 workspace は upgrade 後 1 回だけ init.sh が再実行される** | 3 |
| 6 | **[landed]** **[不可分コア]** `resolveWorkspaceHome` を volume ベースへ (返り値が volume 名になる) + `homeMounts` / `workspaceInitHomeMount` の volume 化 + **identity を home 内 nonce から volume label `boid.workspace_home_id` へ移設** (論点b の該当節) + **marker に `skeleton_dirs` を追加**して daemon 側 mkdir を撤去 + **所有者検証を job container の runner へ移設** (論点b-2 / e-2 — 検証対象は mount に覆われていない祖先のみ、復旧案内は volume 内相対パス) + `LaunchOptions.WorkspaceSlug` / `LaunchOptions.WorkspaceHomeID` 新設 (論点D5、後者は identity を使用地点で再突合するため) + `init_generation` を **2** に bump + e2e teardown に HOME volume の掃除を追加 + 契約 doc 更新 | **workspace HOME が volume になる** (= ホスト再起動で認証と toolchain が消えなくなる)。 **既存 workspace は upgrade 後に 1 回 init.sh が走る** (fresh volume)。 postlude と `<home>/.boid-workspace-home-id` が無くなる。 PR7 まで `workspace remove` の home 削除 / サイズ / orphan が効かない | 1,2,4,5 |
| 7 | workspace remove 連動の削除・サイズ可視化・orphan 検出を volume API へ rewiring (論点 a-2)。 engine 呼び出しは `dispatcher.WorkspaceHomeVolumeStore` に閉じ、`internal/api` は moby を import しないまま (D2)。 サイズは `DiskUsage`(`Volumes+Verbose`) 1 回 (D3)、in-use は 409 を握り潰さず報告 (D6)、orphan 突合は label 値 + 名前再構成 (D7)、ゲートは `RuntimesDir != ""` から engine handle の有無へ (D5) | **`workspace remove` が home volume を実際に消すようになった**。 サイズ表示と orphan 検出が復活。 **`path` / `home_path` field が `volume` / `home_volume` に置き換わり、CLI の表示が host path から volume 名になった** (D4)。 サイズ意味論が本物の `du` に揃い、hardlink の重複計上が無くなった (D3-b)。 job が走っている workspace を remove すると 409 が warning として出る (従来は silent) | 6 |
| 8 | **[landed]** 既存 homes の移行 CLI (`boid workspace import-home <slug> [--from DIR] [--dry-run] [--yes]`、論点 f の決定表)。 **CLI は host dir を読んで tar を作りストリームするだけ**で、engine 操作・marker 削除・identity 生成はすべて daemon (`POST /api/workspaces/{slug}/home/import` → `dispatcher.Runner.ImportWorkspaceHome`)。 移行後の再 init は 論点 f の 2 leg 併用 (volume 作り直し + marker 削除) + `internal/client.PostStream` (streaming body) + `dispatcher.WriteWorkspaceHomeTar` (mode 維持 / hard link dedupe / 不正規ファイル skip 報告) + 展開は init container の primitive 流用 (決定的 container 名を共有して init と排他、network だけ `none`) + **daemon 内 in-flight 登録簿で dispatch と排他** (`dispatcher.workspaceHomeInFlight`、codex Blocker 2) | **移行した workspace は次の dispatch で `init.sh` が 1 回走る** (論点 f の受け入れ条件、意図的)。 **移行元 `homes/<slug>` は読むだけ** — 削除も変更もしない。 job が走っている workspace への移行は 409 で拒否され、その時点で何も壊れていない (engine の 409 に加え、**container 作成前の dispatch でも 409**)。 逆に移行中に来た dispatch は**待つ** (拒否 = job 失敗になるため)。 既存 home volume は破棄されるので既定で対話確認 (`--yes`/`--force` で skip)。 途中で失敗すると **volume ごと削除**され、「一度も dispatch していない workspace」と本当に同じ状態になり、次の dispatch が init から作り直す | 6 |
| 9 | init.sh の CLI 経路 (export/import の `init_script` + 専用 CLI) | | — (独立) |

**分割の考え方**: 「daemon が workspace HOME に書く経路」は 3 つあった (marker/lock、init.sh 実行、
skills sync)。**volume への切り替えを跨ぐ変更は 1 PR にまとめる必要がある**が、
PR2-5 はいずれも**現行の path ベース HOME のまま挙動を保って先行 land できる**ため、
最リスクの PR6 を「切り替えそのもの」だけに縮められる。

PR2 / PR3 landed 時点で残っている HOME への daemon 書き込みは 2 つ: init.sh 実行と、
PR3 が新設した bind target の mkdir (skill 実体の書き込みは runtime dir へ退いた)。
どちらも PR5 の prep / init container へ移す。

初版は 3+4 を「必要なら 1 PR」、第 2 版は 6 項目を「必ず 1 PR」としていたが、後者は過大だった
(Fable 再レビュー R-3)。`volume-only-daemon.md` が「巨大 1 PR は review 困難・CI failure の
切り分け困難」を理由に段階案を採った経緯とも整合しない。

**PR6→PR7 の中間状態 (解消済み)**: PR7 が入るまで `boid workspace remove` の home 削除は
silent no-op になり volume が残っていた。サイズ表示と orphan 検出も同時に無効だった。
その間はコード側 (`internal/api/workspace_homes.go` 冒頭) と利用者向け doc の両方に
「踏んだときに何が起きるか」と手動対応 (`docker volume rm boid-ws-home-<installID8>-<slug>`)
を明記していた。PR7 でどちらの注記も撤去した。

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

## PR6 が持ち込む挙動変化 (ユーザから見えるもの)

1. **workspace HOME が named volume になった** — 実体は
   `boid-ws-home-<installID8>-<slug>`。 `~/.local/share/boid/homes/<slug>/` は
   読まれも消されもしない (PR8 の移行 CLI が読む)。 **ホスト再起動で認証情報と
   toolchain が消えなくなった** — 本 doc の発端の退行の修復
2. **既存 workspace は upgrade 後に init.sh が 1 回走る** — fresh volume なので
   identity が一致せず、加えて `init_generation` も 2 に上がっている。
   volume は空なので、この 1 回は「短縮された再実行」ではなく**本当の初期化**になる
   (toolchain は移行するまで入っていない → PR8)
3. **`<home>/.boid-workspace-home-id` が書かれなくなった** — identity は volume の label へ。
   init container の postlude ごと撤去。 段階名から `postlude` が消えた
   (エラー文面は `prelude` / `script-setup` / `init.sh` / `unknown` の 4 つ)
4. **`~/.claude` の所有者が壊れているときの報告者が変わった** — daemon の dispatch 前
   エラーから、job container 起動後の runner による job 失敗へ。 復旧手順も変わり、
   ディレクトリ削除に加えて **marker の削除**が要る (削除だけでは engine がまた uid 0 で作る)
5. **embedded skill を増やしたリリースで init.sh が 1 回再実行される** — marker の
   `skeleton_dirs` が変わるため。 冪等な init.sh なら短時間で終わる
6. **`boid-ws-home-*` という名前の label 無し volume があると dispatch が止まる** —
   boid が作った volume は必ず identity label を持つので、外部で作られたと判断して
   fail loud する (再 init しても収束しないため)
7. **`boid workspace remove` の home 削除・サイズ表示・orphan 検出が PR7 まで効かなかった**
   (論点 a-2。PR7 で解消済み)
8. **e2e の teardown が `boid reap --include-workspace-homes` を使うようになった** —
   HOME volume は設計上 `boid.install_id` を持たないので、そうしないと CI ホストに
   1 run につき workspace 数ぶんの volume が永久に残り、leak チェックは成功を報告し続ける

## 未解決 / 本 doc の範囲外

- **host_commands の volume-only 対応** (論点 d 後半)。container 内 daemon から host バイナリを
  どう呼ぶのか。broker 経由・廃止・image 同梱のいずれか、設計判断が要る
- **`boid web set-addr` / `set-url` の撤去** (dogfood で判明した silent no-op、
  `scopeLocal` のまま host の config.yaml を書いて成功表示する)
- **`notify.command` の host path 依存** (`/home/nosen/.local/bin/ntfy.sh` が container 内に無い)
- k8s (Phase 7) での PersistentVolumeClaim への読み替え。named volume 前提の設計は
  そのまま PVC に対応付くはずだが、本 doc では扱わない

## 解決済み: `capabilities.docker` を持つ job から daemon の state volume が mount できた

**PR2.5 で解決 (2026-07-26)**。以下は当初この節に「未解決」として記録していた露出と、その決着。

### 露出 (PR1 の穴)

compose デプロイでは daemon の永続領域は `boid_state` named volume
(compose の `name: boid` が付くので **engine 上の実名は `boid_boid_state`**) で、
`/home/boid` 全体にマウントされている。dockerproxy policy の volume 判定
(`dockerres.IsReservedVolumeName`) は **`boid-ws-` prefix しか予約していなかった**ので、
job は次を投げるだけで daemon の $HOME を丸ごと読めた:

```json
{"Image":"busybox","Cmd":["sh","-c","cat /state/.local/share/boid/secret.key"],
 "HostConfig":{"Mounts":[{"Type":"volume","Source":"boid_boid_state","Target":"/state"}]}}
```

読めるのは workspace home の init marker どころではなく
**`secret.key` / `boid.db` / `tls/ca.key` / `web_secret` / `install_id` 一式**である。
PR1 が塞いだのは「job が *workspace HOME volume* を壊す」経路であって、
「job が *daemon 自身の state volume* を読む」経路ではなかった。**書き込み権限を一切要さない**
(mount するだけ) ので、経路 4 の create/delete deny は元から関係なかった点に注意。

### 採った方式: daemon の自己 inspect + policy への実行時注入

**「boid が自分で名前を決める volume」は prefix で判定できるが、`boid_boid_state` は
compose project が決める名前**なので、boid 側にパターンが無い。したがって
**daemon が起動時に「自分が何にマウントされているか」を engine に訊く**:

0. まず `/proc/self/mountinfo` を見て、daemon の永続領域が**独立した mount point の上に
   載っているか**を判定する。載っていなければ (root mount 上のただのディレクトリ =
   bare `boid start`) 守るべき volume が存在しないので、engine には一切問い合わせずに
   **無警告で終了**する。載っていれば以降に進み、**失敗したら必ず WARN する**
   (下記「判定軸を engine ではなく資産に置いた理由」)。
   比較の前に dataHome を `filepath.Abs` + `filepath.EvalSymlinks` で
   **kernel が mountinfo に書くのと同じ形に正規化**する (下記「dataHome の正規化」)。
   **engine を必要とするのはここから先だけ**なので、この順序が
   「どんな engine 由来の失敗も必ず `StateVolumeExpected=true` を伴う」を保証する
1. `/proc/self/mountinfo` から自分の container ID を取る
   (`internal/dispatcher/daemon_state_volume.go`)
2. その ID で `ContainerInspect` し、`Mounts[]` のうち `Type == volume` の `Name` を集める
   (ここまで全体に 5s の deadline。下記 [Major 2])
3. `internal/server/wire.go` → `dispatcher.WireConfig.ReservedVolumeNames` →
   `Runner.ReservedVolumeNames` → `startDockerProxy` →
   `dockerproxy.Server.SetReservedVolumeNames` で policy の予約集合に載せる
4. policy 側は PR1 の 3 経路 (`POST /volumes/create` の `Name` /
   `DELETE /volumes/{name}` / `POST /containers/create` の `HostConfig.Mounts[].Source`)
   を**そのまま再利用**し、判定だけ「静的 prefix ∪ 実行時集合」に広げた
   (`dockerproxy.ReservedVolumes`)。`CheckRequest` は純関数のままで、集合は起動時に確定した
   **値**として渡す (リクエスト処理中に engine へ問い合わせない)

`HostConfig.VolumesFrom` は PR1 が既に無条件 deny 済みで、本 PR では確認のみ (回帰テストを追加)。

**`/proc/self/cgroup` を主にしなかった理由**: 実機 (rootless podman 4.9.3) の daemon container で
`cat /proc/self/cgroup` は `0::/` しか返さない (cgroup v2)。同じ container で `HOSTNAME` env も
**空**だった (`ContainerBackendOptions.SelfContainerID` が使っている経路)。一方 mountinfo は
両 engine とも container ID を含む:

| engine | mountinfo の root field |
|---|---|
| docker | `/var/lib/docker/containers/<64hex>/hostname` (moby 自身の test corpus) |
| podman rootless | `/containers/overlay-containers/<64hex>/userdata/hostname` (実機 2026-07-26) |

判定は「`/etc/hostname` `/etc/hosts` `/etc/resolv.conf` `/run/.containerenv` のいずれかを
mount 先とする行の root に、64 桁 hex の segment がちょうど 1 つ」。mount **先**を見ないと、
docker host の mountinfo に現れる `/var/lib/docker/containers/<別 container>/shm` を拾ってしまう。
cgroup は fallback として併用 (cgroup v1 / `libpod-<id>.scope`)。

### 却下した案

- **`boid_` / `boid-` prefix を一律 deny** — 実装は最小だが、ユーザの `boid_myapp` に誤爆する。
  compose project 名を変えると守れなくなる (名前は boid のものではない)
- **ledger 所有でない volume の mount を全部 deny** — 最も堅いが挙動変化が大きすぎる
  (testcontainers 等の正当な用途を壊す)。`capabilities.docker` の脅威モデルを見直すなら本命

### 検出できなかったときの挙動

**「守るべき資産が volume に載っている可能性があるか」**と
**「volume 名が取れたか」**を別に判定する。前者は当初 sentinel file
(`/run/.containerenv` / `/.dockerenv`) の有無で判定していたが、codex レビュー
[Major 1] で撤回した (下記「判定軸を engine ではなく資産に置いた理由」)。

- **volume が載っている余地が無い** = daemon の永続領域 (`filepath.Dir(cfg.DBPath)`) が
  root mount 上のただのディレクトリ (= bare `boid start`) → state volume がそもそも
  存在しないので **no-op・無警告**
- **載っている可能性があるのに特定に失敗** (ID 不明 / 曖昧 / inspect エラー / inspect timeout)
  → 起動時に **1 回 WARN** して続行。fail-closed で daemon の起動を止めるのは代償が大きすぎる
  (この露出は全リリースで存在していたので、「起動しない」に置き換える取引にはならない)
- **特定できたが dataHome がどの volume にも含まれない** → 各段は成功しているのに守れていない
  状態なので、別途 WARN
- **判定不能** (mountinfo が読めない / dataHome が空 / dataHome を正規化できない /
  mountinfo に該当 mount 行が無い)
  → **WARN 側に倒す**。「無警告」は肯定的に「守るものが無い」と確認できたときだけの答えであり、
  確認に失敗した結果として無言になってはならない
- **dataHome の正規化だけ失敗し、ID 特定も inspect も成功した** → containment は**有効**
  (volume は実際に予約されている) なので error にはせず、`DataHomeUnresolved` に載せて
  **INACTIVE とは別文面の WARN** を出す。伝えているのは「予約が効いていない」ではなく
  「`data_home_covered` が信用できない」であり、両者を同じ文面に混ぜると
  健全なデプロイに対して `grep INACTIVE` が誤検知する
  (下記「dataHome の正規化」の round 3 追記、codex round 3 [Minor 1])

**不変条件**: `DetectDaemonStateVolumes` の **error を返す経路はすべて
`StateVolumeExpected=true` を伴う**。`StateVolumeExpected=false` は
「root mount 上のただのディレクトリだと肯定的に確認できた」1 経路だけが返し、そこは error が nil。
これにより呼び出し側は「error → WARN」「!StateVolumeExpected → 無言」を
互いに独立した分岐として書ける。`TestDetectDaemonStateVolumes_everyErrorCarriesStateVolumeExpected`
で全 error 経路を列挙して pin してある。呼び出し側 (`reservedDaemonStateVolumes`) も
**error を先に読む** ので、将来この不変条件が崩れても「無言」ではなく「余計な WARN」に倒れる
(codex round 2 [Major 2])。

### 判定軸を engine ではなく資産に置いた理由 (codex [Major 1] 対応)

sentinel file 判定には**失敗が no-op と区別できない**という致命的な性質があった。
containerd / Kubernetes は `/run/.containerenv` も `/.dockerenv` も書かず、mountinfo の
runtime 注入行にも 64 hex の container ID が出ず、cgroup v2 は `0::/` しか返さない。
この環境では「container 外」と誤分類され、**成功扱いの空集合を無警告で返して containment が
まるごと無効**になる。node が docker 互換 socket を job に見せている構成 (この機能が意味を持つ
唯一の構成) なら攻撃はそのまま成立する。

代わりに「daemon の永続領域が `/proc/self/mountinfo` 上で**独立した mount point の上に
載っているか**」を見る。named volume は必ず「載っている」側であり、この性質は engine 種別にも
sentinel にも依存せず、かつ**守ろうとしている資産そのもの**に直結しているので feature から
乖離しようがない。実測 (2026-07-26):

| 環境 | dataHome | それを覆う mount point | 判定 |
|---|---|---|---|
| live rootless podman daemon container | `/home/boid/.local/share/boid` | `/home/boid` (volume `boid_boid_state`) | 載っている → 特定へ |
| 開発ホスト (bare `boid start` 相当) | `/home/nosen/.local/share/boid` | `/` (mount point は `/` `/boot` `/run` `/dev` `/sys` のみ、`/home` は無い) | 載っていない → 無警告 no-op |

**既知の false positive**: `/home` や `/var` を独立パーティションにしている bare host は
「載っている」と判定され、container ID が取れず WARN が 1 回出る。守るものが無い環境での
1 行のノイズと、守るものだらけの環境での沈黙とのトレードで、後者を潰す方を選んだ。
WARN 文面は両方の読者に成立するよう「観測した条件」を述べる形にしてある。

### dataHome の正規化 (codex round 2 [Major 1] 対応)

mount 比較は純粋な文字列比較で、`pathIsWithin` は mount point `"/"` に対して無条件に true を返す。
したがって **kernel が書かない綴りの path はすべて `/` にマッチして「root mount 上のただの
ディレクトリ = 守るものが無い」と判定され、唯一の無警告経路に落ちる**。初版はこれを踏んでいた:

| 綴り | 実例 | 初版の判定 |
|---|---|---|
| 相対パス | `boid start --db-path ../home/boid/.local/share/boid/boid.db` (cwd `/workspace`)。`cmd/start.go` は `--db-path` を**拒否も絶対化もしない** | `/` にマッチ → **無警告で保護無効** |
| 途中に symlink | `/home/boid` → `/mnt/state` 等 | 同上 |

対策として比較前に `filepath.Abs` → `filepath.EvalSymlinks` を通す
(`resolveDataHomeForMountComparison`)。**正規化に失敗したら沈黙ではなく「判定不能 = WARN 側」**
に倒す (未作成の dataHome での ENOENT も含む。実運用では buildRuntime 時点で boid.db が
その path に開かれているので起きない)。

初版が `EvalSymlinks` を避けた理由は「hung NFS/FUSE の下だと context deadline でも中断できない
無限待ちを作る」だったが、これは**撤回**した。daemon はそもそも**同じ path で boid.db を開く**ので、
そこが hung なら self-inspect の有無に関わらず起動は詰まる。避けても可用性は 1 ミリも買えておらず、
上表の取り逃しだけが残っていた。

`DataHomeCovered` の判定にも正規化後の path を使う (engine が返す mount destination は
kernel 解決済みの絶対パスなので、相対/symlink 綴りだと必ず false になってしまう)。

#### 正規化失敗の行き先 (codex round 3 [Minor 1] 対応)

上の「判定不能 = WARN 側」は**特定にも失敗した場合**の話で、`errors.Join` で error に畳まれる。
問題は **ID 特定も inspect も成功し、正規化だけ失敗した**経路だった。round 2 実装ではこの
`resolveErr` を error 経路でしか使っておらず、**この組み合わせでは黙って捨てられていた**
(nil error + volume 名あり + `DataHomeCovered` あり = 起動ログが完全に綺麗)。
壊れた symlink を dataHome に持つ container デプロイがちょうどこの形になる。

かといって error として返すのも誤りである。呼び出し側にとって error は
**「containment INACTIVE」**を意味するが、実際には volume は名前が取れて予約されており、
containment は有効だからである。したがって `DaemonStateVolumes.DataHomeUnresolved`
(error 型フィールド) に載せ、`reservedDaemonStateVolumes` が
**INACTIVE 文面とは別の WARN** を出す。伝えているのは
「`data_home_covered` はこの run では信用できない」という**フィールド 1 個の信頼性**の話であって、
予約そのものの失敗ではない。

`DataHomeCovered` は false に倒さず、**絶対化だけ済んだ (symlink 未解決の) 綴り**で判定を続ける。
false は「crown jewels はどの予約 volume にも入っていない」という**別の**主張になってしまい、
それもまた根拠が無いからである。

pin: 生成側 `TestDetectDaemonStateVolumes_unresolvableDataHomeStillReportsItWhenIdentified`
(internal/dispatcher)、呼び出し側
`TestReservedDaemonStateVolumes_unresolvedDataHomeWarnsWithoutClaimingInactive`
(internal/server、WARN が出ること + それが INACTIVE 文面**でない**ことの両方を assert)。

### 自己 inspect の deadline (codex [Major 2] 対応)

self-inspect 全体に `selfInspectTimeout` (5s) を張る。守る対象は「engine が落ちている」
(即 dial error) ではなく **「socket は accept するが応答しない」** (wedged engine、engine 再起動を
生き延びた half-open socket、前段 proxy) で、deadline が無いと `context.Background()` を
引き継いで `boid start` が**永久に停止**する。「WARN して続行」という設計自体が成立しなくなる。
timeout は「container 内だが特定失敗」として WARN + 続行に落ちる。
`/proc` の `os.ReadFile` は procfs が in-kernel state から生成するのでブロックしない。
`filepath.EvalSymlinks` は実ファイルシステムを触るが、上記のとおり同じ path の DB open が
先に走っているので新しい停止要因ではない。

**docker client は self-inspect が自前で作らない** (codex round 2 [Major 2])。
`client.New(client.FromEnv)` は `DOCKER_CERT_PATH` の ca/cert/key を**同期的に読む**ので
(moby `client_options.go` の `WithTLSClientConfigFromEnv`)、self-inspect が独自に構築すると
**deadline で中断できない読み取りが起動経路にもう 1 つ増える**。加えて構築失敗が
「零値 + error」という *`StateVolumeExpected=false` を伴う error* を作り出し、
呼び出し側がフラグを先に見て**無警告で捨てていた**。
`buildRuntime` が docker client を**1 個だけ**作り、container backend
(`sandboxBackendForConfig`) と self-inspect の両方に渡す形に変更した。
構築失敗は従来どおり **daemon 起動拒否** (container backend が唯一の backend なので
どのみち起動できない)。これで同期読みは「container backend の前提条件」に戻り、
self-inspect 固有のリスクではなくなった。

pin は `TestServer_New_SelfInspectGetsTheBackendsDockerClient` で、
**両者が同一ポインタであること**を assert する
(`dispatcher.ContainerBackendUsesDockerAPI`)。「どちらも non-nil な `*client.Client`」では
2 個作る退行がそのまま通ってしまう — 上のコストはどちらも *2 個目* のコストだからである
(codex round 3 [Minor 2])。

### 曖昧な container ID を推測しない (codex [Major 3] 対応)

cgroup fallback は「最初に見つけた 64 hex」を返していた。nested runtime は**親の ID を先に**書く:

```
0::/docker/<親 container ID>/docker/<自己 container ID>
```

これで親を inspect すると、親にも volume があれば `DataHomeCovered=true` で
「containment 有効」と表示しながら**自分の state volume は予約されない** — 予約ゼロより悪い。
本来 WARN すべき場面が成功に見えるからである。したがって **distinct な ID が 2 つ以上出たら
「曖昧」として推測せずに失敗扱い** (= WARN + fail-open) にする。
「曖昧」の定義は *ファイル全体を走査して、各 runtime の unit wrapper (`docker-` / `libpod-` /
`crio-` / `cri-containerd-` / `containerd-`、`.scope` / `.slice`) を剥がした後に残る
**相異なる** 64 桁 hex が 2 つ以上*。cgroup v1 が controller ごとに同じ ID を並べる形は
**曖昧ではない** (重複は無視)。mountinfo 側は元から同じ規律 (1 行に 2 つ hex があればその行を
捨て、複数行が別々の ID を主張したら error) で、両者が揃った。

### 残る限界

- **daemon が自分の container を特定できない環境では効かない**。mountinfo/cgroup のどちらにも
  ID が出ない runtime (k8s / containerd、gVisor 等) では**無防備のまま**。ただし Major 1 修正後は
  そこで必ず **WARN が出る**ので、「知らないうちに無防備」ではなくなった
- **nested runtime (docker-in-docker) では原理的に自分を特定できない**。曖昧と判定して
  fail-open + WARN になる。推測しないことを選んだ結果であり、この環境では守れない
- **判定は daemon の *data root* だけを見る**。`XDG_CONFIG_HOME` を data root と別 volume に
  分離した構成で、data root だけが root mount 上にある場合、config 側 volume は無警告で
  無防備になる。現行 compose (`/home/boid` 配下に両方) では起きない
- **同じ理由で、container 内でも data root が overlay rootfs 上 (= volume 化されていない)
  なら no-op になる**。この場合 daemon の永続 state は再作成で消える運用なので守る対象が
  無いが、同じ container に別の named volume が付いていてもそれは予約されない。
  sentinel 判定を残していれば拾えたケースだが、拾うために container 内で volume が
  1 本も無い構成に毎回 WARN を出すのは割に合わないと判断した
- **dataHome が解決できない環境では (無防備ではなく) WARN に倒れる**。相対パス / symlink は
  `filepath.Abs` + `filepath.EvalSymlinks` で解決するようになった (上記「dataHome の正規化」) が、
  解決自体が失敗する場合 — 未作成のディレクトリ、権限、symlink ループ — は「判定不能」として
  WARN + fail-open になる。実運用では daemon が同じ path で boid.db を開いた後に走るので
  起きないが、`--db-path` に存在しない dir を渡した起動などでは 1 行出る
- **ただし正規化失敗そのものは containment を無効にしない**。ID 特定と inspect が成功していれば
  volume は予約され、無効化されるのは `data_home_covered` の**信頼性だけ**である
  (`DataHomeUnresolved` + 専用 WARN、上記「正規化失敗の行き先」)。この場合
  `data_home_covered` は symlink 未解決の絶対パスで計算した近似値なので、
  **true でも「crown jewels が予約 volume 内にある」証拠にはならない**。
  この 1 行が出ている起動では coverage の主張を根拠に使わず、dataHome を直せ
- **正規化は起動時の一度きり**。以後 dataHome の途中の symlink が張り替えられても追随しない
  (volume 名のスナップショットと同じ前提)
- **engine socket そのものが sandbox に露出している構造は変わっていない**。dockerproxy の
  allowlist が唯一の壁であり、policy をすり抜ける API 形があれば同じ場所に戻る
- **`HostConfig.VolumesFrom` は「名前で守る」ことが原理的にできない**。request が container を
  指すため body から「何を継承するか」が分からない。無条件 deny で塞いでいるだけで、
  もし将来この deny を緩めれば daemon container 経由で state volume が再び到達可能になる
- **daemon container 自身に対する操作は ledger scope check 頼み**。予約しているのは volume 名で
  あって container ID ではない
- **volume 名は起動時にスナップショットする**。daemon の稼働中に新しい volume が daemon に
  mount されることは (container を作り直さない限り) 起きないので実害は無いが、設計上の前提
