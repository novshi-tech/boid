# workspace HOME の named volume 化 (永続性退行の修復)

**状態**: 設計 (2026-07-26 作成、実装未着手)
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
本 doc が直そうとしているインシデントの再演になる。破壊経路は 3 つある:

1. `internal/reap/reap.go:124` — `reap.Run` は列挙した volume を `VolumeRemove{Force: true}` で
   **無条件に全削除**する。そしてこれは `boid reap` CLI 専用ではなく、
   `internal/dispatcher/container_backend.go:1296` の `ReapOrphans` (**daemon 起動時の
   startup reap**) から毎回呼ばれる。job container が生きていれば in-use で守られるが、
   ホスト再起動直後は必ず成功する
2. `container_backend.go:1319` の `reapOrphanVolumes` — `reapOwnsLabels` (:1312) は
   `labels[labelInstallID] == b.installID` だけを見るので、install_id label を付けた時点で対象になる
3. `container_backend.go:1565` の `ensureNamedVolumes` — named volume を作る既存経路だが、
   **job の label 一式 (`boid.job_id` 含む) を付けて `VolumeCreate` する**。しかも
   `boid.job_id` label が無い volume には「ReapOrphans's volume sweep will not find it」と
   `slog.Warn` を出す。永続 volume ではこの warning は**正しい状態**なので、
   警告条件の見直しも必要になる

**決定すべきこと (実装時送りにしない)**:

- workspace HOME volume には `boid.job_id` と素の `boid.install_id` を**付けない**。
  専用 label (例: `boid.workspace_home=<slug>` + install scope を別キーで持つ) にする
- 上記 3 箇所すべてに除外規則を入れる。PR2 のスコープに 3 箇所を明記する
- `boid reap` (deploy 全体を破壊する CLI) が workspace HOME を消すべきか否かを**契約として宣言**する。
  「このインストールの docker リソースを全部消す」コマンドの意味論と、
  「認証情報は消えてほしくない」要求のどちらを優先するか

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

**rewiring 方針**: 削除は `VolumeRemove`、サイズは VolumeInspect の UsageData
(`docker system df -v` 相当)、orphan 検出は volume 列挙と workspace DB row の差分。
これを扱う PR を分割表に持つこと (下記 PR4)。

### 論点 b: marker / lock の置き場

現行は `homesDir/<slug>.init.json` と `homesDir/<slug>.lock` (= workspace HOME と同じ親)。
volume 化すると daemon から直接読み書きできなくなる (volume の中身は container 越しにしか触れない)。

**方針**: marker / lock は daemon 自身の永続領域 = `boid_state` volume 内
(`/home/boid/.local/share/boid/homes-meta/<slug>.{init.json,lock}`) に置く。
daemon は自分の volume なので普通の file I/O で扱え、flock もそのまま使える。

注意: 現行の `acquireWorkspaceHomeLock` の flock 意味論をそのまま維持すること
(PR #787 の TOCTOU fix — lock 取得後に script を再読込・再ハッシュする二重チェック)。

**ただし marker と実体が別ライフサイクルになる副作用を潰すこと。** 現行は marker と home dir が
同じ親ディレクトリにあり、揮発するときも同時だった。新設計では volume だけが消える状況が生じる
(手動 `docker volume rm`、論点 a の reap 誤爆、workspace remove の半完了など)。そのとき次の
dispatch は marker を見て init を skip し、**空の HOME で job が走る**。実際に出るエラーは
adapter の「CLI not found」(`internal/adapters/claude/run.go:43`) で、これは init.sh 未整備を
指す文面なので真因 (volume 消失) に誘導しない。

対策として marker に volume の identity を持たせ、実体と突合してから skip する:
init 時に volume 内へ nonce を書き marker にも記録する (VolumeInspect の CreatedAt でもよいが、
engine 差が出る)。nonce を volume 内に置く方式は、marker を home の外に置いた元々の理由
(`workspace_home.go:20-24` — sandbox からの改竄耐性) と両立するかを実装時に確認すること。

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
  その allowlist 下では installer がほぼ全滅する。daemon と同じ default bridge
  (無制限 egress) に置くのが妥当だが、それは「trusted 境界は維持される」の一部として
  明文で宣言すべき事項であって、暗黙に決めてよいことではない
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

どちらでも `ensureNamedVolumes` の label 問題 (論点 a) は別途対応が必要。

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
job container に `$HOME/.claude/skills` へ RO で重ねる。この場合 volume 側の HOME に
skills が存在しないことになるので、`.claude/` 配下の他の内容 (認証情報) と
mount が競合しないかを確認すること。

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

一度きりの移行なので CLI (`boid workspace import-home <slug> --from <dir>`) でもよいし、
起動時の自動移行でもよい。実装時に決める。

## PR 分割案

| PR | 内容 | 依存 |
|---|---|---|
| 1 | volume 化の下準備: 論点 e の (i)/(ii) 決定 + `ensureNamedVolumes` の label 分離と warning 条件見直し + reap 3 箇所 (`reap.Run` / `reapOrphanVolumes` / `ensureNamedVolumes`) の除外規則 | — |
| 2 | **[不可分]** `resolveWorkspaceHome` を volume ベースへ + marker/lock を daemon 永続領域へ + marker の volume identity 突合 + init.sh を使い捨て container 実行へ + skills sync の materialize 先変更 + `WorkspaceSlug` 導出の修正 + 契約 doc 更新 | 1 |
| 3 | workspace remove 連動の削除・サイズ可視化・orphan 検出を volume API へ rewiring (論点 a-2) | 2 |
| 4 | 既存 homes の移行経路 (uid mapping を跨ぐ tar stdin) | 2 |
| 5 | init.sh の CLI 経路 (export/import の `init_script` + 専用 CLI) | — (独立) |

**PR2 が不可分である理由**: 「daemon が workspace HOME に書く経路」が 3 つあり
(marker/lock、init.sh 実行、skills sync)、どれか 1 つだけを volume 側に切り替えると
job container が見る HOME と daemon が書く場所がずれる。初版は 3 (resolveWorkspaceHome) と
4 (init.sh) を分けて「必要なら 1 PR に」と書いていたが、skills sync (論点 e-2) と
`WorkspaceSlug` 導出 (`runner.go:724` が `filepath.Base(workspaceHomeDir)` で slug を得ており、
戻り値が volume 名になると `BOID_WORKSPACE_SLUG` と adapter のエラーメッセージが壊れる) も
同じ切り替えに巻き込まれるため、**「必ず 1 PR」に格上げする**。

初版にあった「PR1: `sandbox.Mount` に volume 型追加」は論点 e の事実誤認に基づいていたので削除し、
label / reap 側の下準備に置き換えた。

## 未解決 / 本 doc の範囲外

- **host_commands の volume-only 対応** (論点 d 後半)。container 内 daemon から host バイナリを
  どう呼ぶのか。broker 経由・廃止・image 同梱のいずれか、設計判断が要る
- **`boid web set-addr` / `set-url` の撤去** (dogfood で判明した silent no-op、
  `scopeLocal` のまま host の config.yaml を書いて成功表示する)
- **`notify.command` の host path 依存** (`/home/nosen/.local/bin/ntfy.sh` が container 内に無い)
- k8s (Phase 7) での PersistentVolumeClaim への読み替え。named volume 前提の設計は
  そのまま PVC に対応付くはずだが、本 doc では扱わない
