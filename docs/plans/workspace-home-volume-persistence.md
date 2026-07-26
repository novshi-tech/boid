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

container backend ではここが変わる。`WorkspaceHomesDir(runtimesDir)` は
`filepath.Dir(runtimesDir)/homes` を返し、container backend の runtimesDir は
`internal/server/wire.go` の `hostVisibleRuntimesDirFor(cfg)` =
`filepath.Dir(cfg.SocketPath)/runtimes` に解決される。実デプロイではこれが
`BOID_RUNTIME_DIR` = `/run/user/<uid>` 配下、すなわち **tmpfs** になる。

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
**volume のルート**がマウントされた (`homes/` がそのまま見えた)。Subpath は docker 25.0+ /
podman 5.0+ の機能であり、podman 4.x は当該フィールドを黙って捨てる。

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
| C | compose に `~/.local/share/boid/homes` の bind mount を足し host 永続 dir に戻す | 不採用 (host filesystem 依存の積み増しは pivot の目的に逆行) |

C を退けた理由は `volume-only-daemon.md` の pivot 判断そのものである。「シークレットをバインドするのは
ホスト側に必要なファイルがある前提で、k8s 上でお客様環境という将来目標と衝突する」。
workspace HOME は認証情報を抱える最も永続性を要求される資産であり、`BOID_RUNTIME_DIR` と同じ
「k8s で成立しない負債」カテゴリに置くのは筋が悪い。

## 論点

### 論点 a: volume の命名と識別

`containerWorkspaceNetworkName` の既存規約に倣う。install_id で scope し、
workspace slug をサニタイズして連結する:

```
boid-ws-home-<installID8>-<sanitized-slug>
```

reap のために label を付ける (`internal/reap` は既に `VolumeList`/`VolumeRemove` と
`DestroyedVolumes` を持っており、label ベースの掃除機構がある)。ただし
**workspace HOME volume は job 用の使い捨て volume とは掃除ポリシーが違う** —
job volume は終了時に消してよいが、workspace HOME は消してはならない。
label を分けるか、reap 側に除外規則を持たせるかは実装時に決める。
**Phase 4 の「homes/ は GC 対象外」契約をここで再度落とさないこと。**

### 論点 b: marker / lock の置き場

現行は `homesDir/<slug>.init.json` と `homesDir/<slug>.lock` (= workspace HOME と同じ親)。
volume 化すると daemon から直接読み書きできなくなる (volume の中身は container 越しにしか触れない)。

**方針**: marker / lock は daemon 自身の永続領域 = `boid_state` volume 内
(`/home/boid/.local/share/boid/homes-meta/<slug>.{init.json,lock}`) に置く。
daemon は自分の volume なので普通の file I/O で扱え、flock もそのまま使える。
「どの volume に対して init 済みか」の記録なので、実体と別の場所にあってよい。

注意: 現行の `acquireWorkspaceHomeLock` の flock 意味論をそのまま維持すること
(PR #787 の TOCTOU fix — lock 取得後に script を再読込・再ハッシュする二重チェック)。

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
- script の渡し方: daemon が volume 内 config から読んだ bytes を、一時 mount か stdin で渡す
- 出力: 現行同様に集約して `slog` へ。失敗時は exit code と tail をエラーに含める

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
  **host のパスに見えて誤誘導**する (`internal/adapters/claude/run.go:50`)

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

### 論点 e: sandbox.Mount への volume 型追加

`internal/sandbox/types.go` の `MountType` は `bind` / `rbind` / `tmpfs` のみ。
volume mount を表現できないので追加が要る。`homeMounts`
(`internal/dispatcher/sandbox_builder.go:979`) が bind の代わりに volume を返すようにし、
container backend 側の realization で docker の
`mount.Mount{Type: mount.TypeVolume, Source: <volume name>}` に変換する。

userns backend は撤去済みなので、bind 版の workspace HOME 経路を残す必要はない
(`workspaceHomeDir == ""` の tmpfs fallback は、workspace 未解決時の挙動として維持)。

### 論点 f: 既存 workspace HOME の移行

dogfood 環境では認証済み workspace HOME (1.5GB) を
`~/.local/share/boid/homes-backup-20260726/default` に退避済み。

新方式に移る際、既存の host 側 `homes/<slug>` から volume へ内容をコピーする経路が要る。
使い捨て container に volume をマウントして `tar` で流し込むのが素直
(daemon から volume に直接書けないため)。一度きりの移行なので CLI
(`boid workspace import-home <slug> --from <dir>`) でもよいし、
起動時の自動移行でもよい。実装時に決める。

## PR 分割案

| PR | 内容 | 依存 |
|---|---|---|
| 1 | `sandbox.Mount` に volume 型追加 + container backend の realization 対応 (単体では挙動不変) | — |
| 2 | per-workspace volume の作成・命名・label + reap の除外規則 | 1 |
| 3 | `resolveWorkspaceHome` を volume ベースへ + marker/lock を daemon 永続領域へ | 2 |
| 4 | init.sh を使い捨て container 実行へ + 契約 doc 更新 | 3 |
| 5 | init.sh の CLI 経路 (export/import の `init_script` + 専用 CLI) | — (独立) |
| 6 | 既存 homes の移行経路 | 3 |

3 と 4 は同時に入らないと整合しない (init.sh と job container が同じ HOME を見る必要がある) ため、
分けるなら 3 で「volume を作るが init.sh は現行のまま host path に書く」中間状態を作らないこと。
必要なら 3+4 を 1 PR にまとめる。

## 未解決 / 本 doc の範囲外

- **host_commands の volume-only 対応** (論点 d 後半)。container 内 daemon から host バイナリを
  どう呼ぶのか。broker 経由・廃止・image 同梱のいずれか、設計判断が要る
- **`boid web set-addr` / `set-url` の撤去** (dogfood で判明した silent no-op、
  `scopeLocal` のまま host の config.yaml を書いて成功表示する)
- **`notify.command` の host path 依存** (`/home/nosen/.local/bin/ntfy.sh` が container 内に無い)
- k8s (Phase 7) での PersistentVolumeClaim への読み替え。named volume 前提の設計は
  そのまま PVC に対応付くはずだが、本 doc では扱わない
