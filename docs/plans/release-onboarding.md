# リリース向けオンボーディング 設計ドキュメント

ステータス: 設計中 (Fable レビュー 2 巡実施、全指摘を反映済み・全件一次確認済み、2026-08-01)
作成日: 2026-08-01

---

## 背景

container backend が単一 backend になり (`docs/plans/volume-only-daemon.md` §論点e)、
compose daemon の dogfood も安定してきた。しかしこれは **boid の checkout がある環境**での話で、
`go install` でバイナリだけ入れたユーザは同じ体験に到達できない。

一次的な理由は 1 点に集約される — **サンドボックス/daemon イメージがソースツリーからしか作れない**。
`build/container/Dockerfile` の build context は `COPY . .`、つまり Go ソース全体である。
バイナリに `go:embed` してある fallback (`build/container/assets.go`、`scripts/embed.go`) は
その doc comment 自身が明言しているとおり「**既にビルド済みの `boid-runner:latest` がローカルにある**」
前提の経路であって、イメージ生成問題は解いていない。

ただし調査の結果、イメージ配布だけを解いても**オンボーディングは通らない**ことが分かった
(穴 7 / 穴 8)。`boid project init` と `boid web set-addr` が両方とも「host のファイルシステムを
daemon が見ている」前提のまま残っており、compose daemon 下では機能しない。
本ドキュメントはイメージ配布と併せてそこまでを射程に入れる。

---

## 決定事項 (2026-08-01, nose)

| # | 決定 | 理由 |
|---|---|---|
| 1 | 公開イメージは **arbitrary-uid 対応**にする | 単一イメージで任意の host uid に対応させる。uid 1000 固定にすると VPS / LDAP 環境で原因不明の失敗を踏む |
| 2 | daemon の形は **compose 一本**。`BOID_MODE=container` を撤去し既定にする | dogfood 済み・e2e が通っている構成に絞る。ただし撤去時に builtin 経路の無回帰を確認することが条件 |
| 3 | CLI の配布は **`go install` のまま** | `debug.ReadBuildInfo()` のモジュールバージョンで image tag を決定できる。Releases バイナリ + install.sh は今回作らない |
| 4 | **GHCR は public** にする | private だと pull に `docker login ghcr.io` が必要でオンボーディングが入口で破綻する。repo 自体が既に PUBLIC (`gh repo view` で確認) なので追加の公開判断は発生しない |
| 5 | **amd64 のみ publish**。Mac (Apple Silicon) は狙わない。ただし **arch mismatch の fail-fast は必須** | image が既に arch 中立なので arm64 は後から manifest list を足すだけで後方互換に追加できる (§論点: arm64)。初回リリースの検証対象を 1 arch に絞り、穴 7 / 穴 8 / uid に手を回す。fail-fast が必須なのは、binfmt/qemu のある arm64 ホストが amd64 image を **エミュレーションで動かしてしまう**と激遅 + 原因不明のクラッシュで調査が破綻するため |

### 決定 1 の実装形 (OpenShift 流 arbitrary-uid)

方向は「gid 0 + group=user 権限」。現行 Dockerfile が build 時にやっている
`useradd --uid ${BOID_UID}` + `/home/boid` / `/run/boid/bin` / `/home/boid/.local` の chown
(`build/container/Dockerfile:200-233`) を置き換える。

```dockerfile
# Dockerfile (概略)
RUN chmod g=u /etc/passwd /etc/group \
 && mkdir -p /home/boid && chgrp -R 0 /home/boid && chmod -R g=u /home/boid
USER 1000:0
```

```sh
# 起動時に実行時 uid の passwd エントリを自己登録する処理 (概略)
if ! getent passwd "$(id -u)" >/dev/null; then
  echo "boid:x:$(id -u):0::/home/boid:/bin/bash" >> /etc/passwd
fi
```

**この 3 点を設計として決める必要がある** (Fable M6):

1. **passwd 自己登録の実装位置。** この image に shell entrypoint は存在しない —
   job は `ENTRYPOINT ["/usr/local/bin/boid", "runner-container"]`
   (`build/container/Dockerfile:243`)、daemon は `entrypoint: ["/usr/local/bin/boid"]`
   (`build/container/compose.yml:221`) で、どちらも boid バイナリ直接である。
   wrapper script を新設するか、Go 側 (`cmd/runner_container.go` と `cmd/start.go` の両方) で
   やるかの判断が要る。**Go 側推奨** — wrapper を挟むと決定 2 の start 再定義 (PR5) と
   entrypoint の絡みが増える。

   ただし **Go 側実装では届かない第 3 の consumer がある**: workspace init/import の
   utility container は entrypoint を上書きしていて boid の Go コードが走らない。
   - init.sh 実行 container: `Entrypoint: []string{"/bin/bash", "-s"}`
     (`internal/dispatcher/container_backend_workspace_init.go:234`)、
     `User: b.uid:b.gid` (`:245`)
   - home import container: tar entrypoint
     (`internal/dispatcher/container_backend_workspace_import.go:263`)、
     `User: b.uid:b.gid` (`:278`)

   init.sh は toolchain (npm / volta / claude CLI 等) を入れる場所で `id` や
   ssh/git の passwd lookup を踏む可能性が高いので、**bash スクリプト先頭に登録
   snippet を prepend する**等の対応が別途必要。import 側は tar のみなので不要の可能性が
   高いが要確認。なおこの 2 container の `User:` は `b.uid`/`b.gid` をそのまま使うため、
   PR2 の gid-0 ガード再設計で一緒に直る。
2. **実行時生成ファイルの group パーミッション。** `chmod g=u` は build 時に存在する
   ディレクトリにしか効かない。daemon が実行時に作る `secret.key` (0600) や boid.db は
   「そのとき実行していた uid」の所有になり、umask 022 では group write が付かない。
   つまり**同じ新イメージでも起動ごとに uid が変わると壊れる**。
   **推奨: umask 002 を両 Go entrypoint (`cmd/runner_container.go` / `cmd/start.go`) で
   設定し、併せて「uid は install ごとに固定」を契約として明文化する** (未決 7 の既定案)。
3. **rootless podman の `userns_mode: keep-id` との相互作用。**
   `build/container/compose.podman.override.yml:56` が keep-id を張っている状態で
   `user: "<uid>:0"` を指定したときの実効 uid/gid は未検証。実機確認が必要。
   確認項目: (a) コンテナ内の実効 uid/gid、(b) `boid_state` volume の所有者、
   (c) `docker.sock` (podman.sock) への到達性 — `group_add` の `DOCKER_GID` が
   gid 0 指定と両立するか。

### 決定 1 は Dockerfile だけでは終わらない (Fable B1)

daemon が gid 0 で走ると **job コンテナの uid/gid 決定が壊れる**。

- `internal/server/wire.go:463` — job コンテナの `--user` は daemon 自身の
  `os.Getuid(), os.Getgid()` から `ContainerBackendOptions.UID/GID` に配線される。
- `internal/dispatcher/container_backend.go:497` —
  `case opts.UID != nil && opts.GID != nil && *opts.UID != 0 && *opts.GID != 0:` のみ
  カスタム値を採用し、**gid 0 は「root 化防止」として明示的に拒否**して
  1000:1000 に fallback する (`:495-506`、フィールド doc comment `:141-155` も
  「both resolve to non-zero」と明言)。

したがって compose を `user: "<uid>:0"` にした瞬間、job コンテナは常に 1000:1000 で起動し、
daemon uid ≠ 1000 の環境では workspace HOME (0700、daemon uid 所有) が読めなくなる。
これは `internal/server/wire.go:449-462` のコメントが「GH Actions runner は uid 1001」という
実例込みで記録している、過去に踏んだ穴そのものである。**uid 0 は引き続き拒否しつつ
gid 0 を正当値として通す**ようガードを再設計し、テストも更新する必要がある (PR2 のスコープ)。

---

## 決定 2 の事前確認: サンドボックス内 builtin は無回帰 (確認済み 2026-08-01)

`BOID_MODE=container` を撤去して host mode を既定にすると、`cmd/root.go:118` の
host mode 分岐が全 scope=remote コマンドで無条件に走る。コンテナ内から `boid` バイナリが
起動される経路を全数洗い出して確認した。

| 経路 | 結果 | 根拠 |
|---|---|---|
| サンドボックス内の `boid <sub>` (builtin) | **無影響** | `main.go:96` `shouldRunBoidBuiltinShim` が cobra dispatch より手前で掴んで broker に投げる。`cmd/root.go` の PersistentPreRunE に到達しない。`BOID_BUILTIN_SHIM` は hook (`internal/orchestrator/planner.go:126-130`) / session・exec (`internal/dispatcher/session_job.go:86-90`) の全 job kind で常時セット |
| job コンテナ entrypoint `boid runner-container` | **無影響** | `isReservedRunnerSubcommand` で shim から除外、かつ `cmd/runner_container.go:39` が `scopeLocal` なので `isRemoteScope` にも掛からない (二重) |
| host_commands shim (`gh` 等) | **無影響** | argv0 basename が `boid` でないため `shimMain` 経路 |
| **compose daemon service 自身の `boid start`** | **要 carve-out** | `build/container/compose.yml:221-222` が `entrypoint: ["/usr/local/bin/boid"]` + `command: ["start"]`。PR5 で `boid start` を「compose up」に再定義すると、**コンテナ内の `boid start` が compose up を試みる再帰**になる |

4 番目の carve-out は `cmd/start.go:241-243` の `shouldRunForeground`
(`--foreground` / `BOID_DAEMON_CHILD=1`、compose.yml:267 が後者をセット) を
「実 daemon 起動」経路として温存することで成立する。PR5 の必須要件。

### scope 分類の作り直しが撤去の本体工数

`scopeLocal` は host mode 分岐を**素通りする**設計 (「bare-metal daemon の生殺与奪」という
分類意図、`docs/plans/cli-remote-connection.md:276`)。compose daemon が唯一の形になると
この前提自体が失効する。`cmd/scope_annotations_test.go:173-195` の scopeLocal 全数 14 件を
洗った結果:

| コマンド | 既定 container 化後の扱い |
|---|---|
| `boid start` | **再定義**。bare-metal daemon 起動 → `compose up`。ただし foreground/daemon-child 経路は温存 (上表 4 行目) |
| `boid stop` | **再定義**。現状は `POST /api/shutdown` (`cmd/stop.go:31`) で socket 越しに daemon プロセスだけを止められるため、compose stack と不整合な半端な down になる → `compose down` へ |
| `boid gc` | **host mode 経由に変更**。現状は runtime-dir bind (`compose.yml:525` の source==target bind) 越しに *偶然* container daemon に届いているが、これは §決定4 の相互排他契約に依存した暗黙経路。bind をいつか撤去したら壊れる |
| `boid project reload` | **host mode 経由に変更**。scopeLocal だが `POST /api/projects/reload` を叩く (`cmd/project.go:449-455`) — gc と同型 |
| `boid web set-addr` / `set-url` | **要再設計**。host 側 config.yaml を直接編集するが、compose daemon の config は `boid_state` volume 内 (`compose.yml:287`) なので**書いても届かない**。穴 8 参照 |
| `boid project init` | **要再設計**。「雛形生成のみ」ではない。穴 7 参照 |
| `boid check` | 内容自体が userns 時代の遺物。穴 5 参照 |
| `boid project migrate` | **要注意コマンド**。ローカル yaml 変換だけではない — `--apply` は `cmd/project_migrate.go:251` で `db.Open(dbPath)` により **host XDG パスの boid.db を直接開き**、`:940-956` は `client.DefaultSocketPath()` を ping して届かなければ直 DB merge にフォールバックする (`applyMigratedWorkspaceOffline`)。compose daemon 下では実 DB は `boid_state` volume 内なので、**host 側の別 DB (bare daemon の残骸 or 新規作成) に書いて silent に無効**になる。socket ping 側は runtime-dir bind 越しに届き得るので「半分だけ効く」状態にもなる。legacy 移行専用として deprecate するか、PR5 で container 下の使用を明示的に拒否する |
| `boid fetch` | 現状維持 (ローカル HTTP のみ、`cmd/fetch.go`) |
| `boid reap` | 現状維持 (engine 直叩き、daemon 不要、`cmd/reap.go:37-49`) |
| `boid init` / `boid workspace import` | 現状維持 (deprecated stub) |
| `boid runner-container` | 現状維持 (上表 2 行目) |

### profiles との優先順位が未定義 (Fable M4)

`cmd/root.go:118` の host mode 分岐は `profiles.Resolve` **より前**に scope=remote を
無条件で掴む。`hostModeEnabled()` を常時 true にすると、`--profile <https>` のリモート接続
(`docs/plans/cli-remote-connection.md` Phase 3 の成果) と unix profile が
remote コマンドから到達不能になる。「`--profile` 明示時は host mode を迂回する」等の
共存規則を PR5 で決める必要がある。

---

## 現状の穴

### 穴 1: イメージがソースツリー必須

前述。`cmd/host.go:63-81` の embedded-assets fallback は「イメージは既にある」前提。

### 穴 2: バージョン identity が存在しない (CLI 側・image 側の二経路)

`internal/dispatcher/workspace_home.go:647-651` に
`// No build-time version stamping (ldflags -X) exists in this repo yet` と明記された
`const boidVersion = ""` が置かれている。

**CLI 側**: `go install` では ldflags を指定できないので `debug.ReadBuildInfo()` の
`Main.Version` を使う。ただし判定は「`(devel)` かどうか」では足りない —
Go 1.24 は git checkout からの `go build` にも VCS タグ由来の version を stamp するため
(clean+タグ一致なら `v0.0.12`、それ以外は pseudo-version や `+dirty` サフィックス。
**これは知識ベースの推測で実機未確認**)、dev checkout のバイナリが `v0.0.12+dirty` を名乗って
存在しない (かつ `+` は docker tag として不正な) ref を引きに行く。
判定は「**exact release tag 以外 (pseudo-version / `+dirty` / `(devel)`) はすべて
ローカルビルド経路**」と広く取る。

**image 側**: `.dockerignore:6` が `.git/` を除外しており、`build/container/Dockerfile:70` も
`go build -trimpath` のみで ldflags 無し。したがって **image 内の boid バイナリは
VCS stamping されず `(devel)` 相当**になる。PR1 が置換対象とする `boidVersion` は
daemon 側 (= image 内バイナリ) が completion marker に書く値なので、
ReadBuildInfo 置換だけでは空同然のまま。image build 時に build-arg + `ldflags -X` で
別途焼く必要がある。未決 1 の「`/api/health` に version を載せる」案も同じ前提に乗る
(`/api/health` に現状 version フィールドは無い — `internal/server/wire.go:1723-1730` で確認)。

### 穴 3: uid 焼き込み

`build/container/Dockerfile:97-98, 200-233` が `BOID_UID`/`BOID_GID` build arg 前提。
compose 側も `user: "${BOID_UID:-1000}:${BOID_GID:-1000}"` (`build/container/compose.yml:413`) で
イメージの build arg と一致していることを要求している。決定 1 で解消するが、
Go 側のガード再設計が同時に必要 (前述)。

### 穴 4: pull の既定 ref にレジストリが無い

pull 機構自体は実装済み — `internal/dispatcher/container_backend.go:120-128` の
`ImagePullIfNotPresent` が既定 (zero value、wire.go は PullPolicy 未指定)。
だが `defaultContainerImage = "boid-runner:latest"` (同 `:290`) はレジストリ名を持たない
ローカル tag なので、pull すると docker.io を引きに行って失敗する。

`boid-runner:latest` を参照している箇所の**全数** (`grep -rn "boid-runner"` で確認):

| 箇所 | 役割 | PR4 での扱い |
|---|---|---|
| `internal/dispatcher/container_backend.go:290` | job コンテナの既定 image | GHCR ref + version へ |
| `build/container/compose.yml:216` | daemon service の image | 同上 |
| `scripts/deploy-container.sh:241-251` | build tag | pull-first に反転 (既定 pull、`--build` を開発者向け裏口に) |
| **`cmd/host.go:340` `composeImageTag`** | host mode fallback の**前提条件チェック** (`imageExists`、`cmd/host.go:505-509, 563`) とエラーメッセージ (`:565-567, :595-597`) | **見落とすと「pull で image は取れるのに fallback が『boid-runner:latest が無い』と言って死ぬ」不整合になる** |
| `e2e/run-container.sh:371-385` | ローカル build した latest を compose に食わせる | **PR4 で compose の image ref を変えると参照が切れる → 同 PR 内で tag 整合が必要**。PR9 まで待つと PR4〜PR8 の間 e2e が赤い |
| `.github/workflows/blackbox-e2e.yml:141,153,171` | `boid-runner:${GITHUB_SHA}` | PR3 で push 対応 |

### 穴 5: `boid check` が userns 時代の化石

`cmd/check.go:61` は今も `unshare --user --mount --map-root-user -- true` を叩き、
`:44` の `hostRequiredTools = []string{"passt"}` は pasta (userns backend のネットワーク分離) を
要求している。docker/podman の有無、compose の有無、`podman.socket` の active 状態、
engine socket への到達性、uid、socket path — container 時代に本当に確認すべき項目を
**1 つも見ていない**。一方 `scripts/deploy-container.sh:64-158` には engine 検出・
compose plugin 検出・`podman.socket` preflight が既に揃っているので、
それを Go 側に持ち上げる形になる。

### 穴 6: getting-started が bare-host 前提

`docs/ja/getting-started/01-install.md` は `go install` → `boid start` (host プロセス) →
ホスト側 XDG パス表 という構成で、volume-only + compose の現実と全面的に食い違う。
`docs/ja/reference/cli.md:32-52` の host mode 節も撤去に合わせて書き換え。

### 穴 7: `boid project init` が compose daemon 下で機能しない (新規、実測で確定)

オンボーディング手順の中核が壊れている。

- `cmd/project.go:346-352` — 雛形生成後に `POST /api/projects` へ
  **host パス** (`work_dir`) を渡して登録する。
- 受け側 `internal/api/project.go:87` は `work_dir` を必須とし、
  `internal/api/project_service.go:351` の `CreateProject` は
  `s.Meta.Load(workDir)` でそのパスの `.boid/project.yaml` を読み、
  さらに `CaptureUpstreamURL(workDir)` でそのディレクトリで git を叩く。
- compose daemon のコンテナ内に `~/src/myproject` は存在しないので **400 で落ちる**。
- そして `cmd/project.go:350-353` はそのエラーを warning に潰して **exit 0** で返し、
  「`boid project add .` を実行して」と案内する。ところが `project add` は
  PR-4 以降 **git URL しか受け付けない** (`cmd/project.go:36-68`、
  `boid project add <git-url>`) ため、`.` は拒否される — **案内先が存在しない**。

volume-only モデルの正規登録経路は `POST /api/projects/git`
(handler は `internal/api/project.go:99-123`、body は同 `:66-74` の
`CreateProjectFromGitURLRequest`) 側であり、
e2e も URL 登録を使っている (`e2e/run-container.sh:947-949`)。
`project init` を「雛形生成 → push を促す → git URL 登録」に組み替える必要がある。

### 穴 8: Web UI の bind bootstrap が compose daemon に届かない (新規)

`build/container/compose.yml:481-487` が明記している:
port publish は必要条件であって十分条件ではなく、fresh volume では daemon が
コンテナ内 127.0.0.1 に bind するため `boid web set-addr 0.0.0.0:8080` を一度実行する必要がある。
ところがその `web set-addr` 自身が host 側 config.yaml を書く scopeLocal コマンドなので、
volume 内の config には届かない (穴 4 の scope 表参照)。

実経路も確認済み: fresh volume → `config.Load` が空 → `cmd/start.go:197-200` で
`defaultStartHTTPAddr` (`:28`、`127.0.0.1:8080`) → コンテナ内 loopback bind →
`compose.yml:488-490` の publish では到達不能。つまり新規ユーザは
`http://localhost:8080` が無応答のまま詰まる。

**修正の設計選択**: 「API 経由の config 書き換え」は既に存在する —
`boid config set` / `config apply` は scopeRemote で `POST /api/config/mutate` を叩き
(`cmd/config.go:138, 236`)、daemon 側の config.yaml を書く。したがって
`boid config set web.http_addr 0.0.0.0:8080` + 再起動が**現時点でも復旧手段として機能する**
(初見でこの経路に辿り着けないだけ)。よって取るべき形は新 API の追加ではなく:

- (a) `cmd/start.go:28` の `defaultStartHTTPAddr` を container 実行時のみ `0.0.0.0:8080` にする、または
- (b) `web set-addr` / `set-url` を既存の config API 経路に統合 (あるいは廃止して `config set` に畳む)

(a) が単独で穴を閉じるので優先。(b) は scope 分類の整合として PR5 と併せて行う。

なお loopback からのペアリング不要ブートストラップも compose daemon では効かないため
(既知)、`boid web pair` は必須手順として扱う。

---

## 目標オンボーディングフロー

```bash
# 1. インストール (Go 1.24+ が前提)
go install github.com/novshi-tech/boid@latest

# 2. 前提チェック (engine / compose / socket / uid を人間に分かる言葉で報告)
boid check

# 3. 起動 — engine 検出 → 自分のバージョンに対応する image を pull → compose up → health 待ち
#    (この時点で Web UI が外から見える bind になっていること = 穴 8 の解消が前提)
boid start

# 4. Web UI をペアリング (compose daemon では loopback 例外が効かないので必須)
boid web pair

# 5. project 登録 — push 済みの git URL を渡す (host パスではない)
boid project add https://github.com/me/myproject --workspace=default

# 6. workspace home の初期化 (ツール install は init.sh、認証は対話セッション)
boid workspace set-init-script default -f init.sh
boid agent claude -p myproject     # ここで claude login
```

山は 6 番。ツールの install は `init.sh` が初回 dispatch で自動実行するが
(`docs/ja/guide/workspace-home.md:38`)、claude / codex の認証は対話が要るので
`boid agent` で 1 回入る必要がある。ここは新規ユーザが最も迷う箇所なので、
`boid start` 直後に「次にやること」を出す誘導が要る。

5 番は穴 7 の解消次第。新規プロジェクトを作りたいユーザには
「雛形生成 → 自分で push → その URL を登録」という 3 手を踏ませることになるので、
`boid project init` がその 3 手を案内する形に組み替えるのが自然。

---

## PR 分割案

| PR | 内容 | 依存 |
|---|---|---|
| PR1 | version identity: `debug.ReadBuildInfo()` ベースの `internal/version` 追加 (判定は exact release tag のみ)、image 側は build-arg + `ldflags -X`、`boid version` 追加、`boidVersion` 定数を置換。**`boid version` は新 leaf command なので `cmd/scope_annotations_test.go:227-240` の期待表更新が必要** (未記載の live leaf は fail-closed で落ちる) | — |
| PR2 | arbitrary-uid: Dockerfile を gid 0 + `g=u` 化、passwd 自己登録を Go 側に実装、compose の `user:` を `<uid>:0` へ、**`container_backend.go:497` の gid-0 拒否ガード再設計 + `wire.go:463` 配線確認 + テスト更新**、実行時生成ファイルの group パーミッション戦略 | — |
| PR3 | GHCR publish: `.github/workflows/blackbox-e2e.yml` の Container image build job (`:120`) に **tag push トリガ追加** (現 `on:` にタグ無し、`:3-17`) と **`permissions: packages: write`** (現在 `contents: read` のみ、`:19-20`) を足し、`ghcr.io/novshi-tech/boid-runner:<tag>` へ **public** で push (決定 4) | PR1, PR2 |
| PR4 | pull 既定化: 穴 4 の表の全 6 箇所 (`container_backend.go:290` / `compose.yml:216` / `deploy-container.sh` / **`cmd/host.go:340`** / **`e2e/run-container.sh` の tag 整合**)。e2e は compose.yml を deploy script 経由だけでなく直接 `-f` でも叩く (`e2e/run-container.sh:129,136,238`) ので、image ref は **`image: ${BOID_IMAGE:-<GHCR ref>}` + deploy/e2e 側で env export** の形にする (これが「e2e を赤くしない」の実装条件)。併せて **`resolveImage` に arch mismatch の fail-fast** を入れる (§論点: arm64 参照 — arm64 を出すか否かに関わらず必須) | PR3 |
| PR5 | host mode 既定化: `BOID_MODE` 撤去、scope 再分類 (`cmd/scope_annotations_test.go` の期待表更新)、`start`/`stop` を compose up/down へ再定義 + **foreground/daemon-child carve-out**、`gc`/`project reload` を host mode 経由へ、**profiles との優先順位決定**、**`e2e/run-container.sh` の CLI 呼び出しを host-mode 化** (`:921-924` の BOID_MODE 検証行含む、`:942-1101` が unix socket 経由で scope=remote を大量に叩いている) | PR4 |
| PR6 | オンボーディング経路の修復: `project init` を git URL 登録へ組み替え (穴 7)、Web UI bind bootstrap の解消 (穴 8 の (a)) | — (独立、早期着手可) |
| PR7 | `boid check` 刷新: deploy script の preflight を Go に持ち上げ、userns probe (`:61`) と `passt` 要求 (`:44`) を撤去、**host arch と image arch の照合**を追加 | PR5 |
| PR8 | docs 全面更新: `getting-started/01-install.md`、`guide/onboarding.md`、`reference/cli.md` (`:55` の `boid-runner:latest` 言及含む)。`boid start` 直後の次アクション誘導の文言もここ (start 再定義後でないと文言が定まらないため) | PR5, PR6, PR7 |
| PR9 | e2e: 「checkout 無しのバイナリだけ」経路 (pull → compose up → dispatch) を追加 | PR4, PR5 |

**PR1 / PR2 / PR6 は互いに独立で並行着手可**。残り (PR3 → PR4 → PR5 → PR7 → PR8 → PR9) は直列。
**PR2 / PR4 / PR5 は e2e を赤くしない形で自己完結させること** — 前 doc の分割案は
e2e 修正を最終 PR に置いていたため、PR4〜PR8 の間 CI が赤い期間が生じていた。
なお PR5 の e2e host-mode 化は、`e2e/run-container.sh:332-348` で
`XDG_CONFIG_HOME` 隔離 + `cli-token` の pre-seed が既に済んでいるため機械的に成立する。

---

## 未決の論点

1. **CLI と daemon の version skew をどう扱うか。** `go install @latest` した CLI と、
   稼働中の古い image daemon が食い違う状況は普通に起きる。`/api/health` に version を
   載せて CLI 側で warn するのか、refuse して `boid start --upgrade` を促すのか。
   `cmd/host.go:596` には既に「イメージが古い」旨のエラーメッセージ素地がある。
   前提として穴 2 の image 側 version 注入が必要 (依存関係)。
2. **exact release tag 以外のローカル判定の具体形。** pseudo-version / `+dirty` を
   どう検出するか、checkout があればビルドを促すのか `latest` に落とすのか。
3. **podman-compose の入手を案内するか。** compose 一本化すると
   `docker compose` plugin か `podman-compose` (pip) がユーザ側前提に加わる。
   `boid check` のエラーメッセージに何を書くか。
4. **multi-arch (arm64) を出すか。** → 「§論点: arm64」に分離。
5. **既存 install の移行。** uid 焼き込みイメージで作られた既存 named volume の所有者と、
   arbitrary-uid イメージの実行時 uid/gid が食い違うケースの移行手順。
   khi workspace など実データがある環境で検証が必要。
6. **新イメージ運用下で uid を安定させる契約。** 論点 5 は旧→新の移行だが、
   新イメージ運用中に uid が変わった場合 (実行ユーザ変更、別ホストへの volume 移送) も
   同じ問題を起こす。決定 1 の実装形 2 番目とセットで決める。
7. **`scripts/deploy-container.sh` の engine-state 機構 (PR5, codex round-9〜14 由来) の
   残エッジケース。** PR5 で `boid stop` (`--down`) が「up 時に実際に使った engine を
   記録し、down 時に pin して検証する」設計 (`COMPOSE_ENGINE_STATE_FILE`,
   `$XDG_STATE_HOME/boid/compose-engine`) に落ち着くまで 14 round のレビューを要し、
   round-1〜13 で見つかった Blocker/Major は全て対応済み — ただし round-13/14 で
   以下が「single-user personal orchestrator である boid の実運用では発生確率が低い
   エッジケース」と判断され、対応を先送りにしたまま残っている:
   - **state directory 自体が dangling symlink の場合の fail-open。**
     `$XDG_STATE_HOME` または `$XDG_STATE_HOME/boid` が dangling symlink だと
     `mkdir -p` が失敗し、続く `stat` は ENOENT を返すため best-effort な
     engine 自動検出へフォールバックしてしまう (`scripts/deploy-container.sh` の
     state directory 作成箇所と `stat` 判定箇所)。round-13 で入れた
     ENOENT-vs-その他 の切り分けだけでは閉じない。
   - **`DOCKER_CONTEXT`/`DOCKER_HOST` が両方 unset でも `docker context use` で
     実効 context が変わり得る。** round-13 で入れた fingerprint は環境変数の値
     しか見ておらず、両方 unset のまま `docker context use` で active context を
     切り替えられると fingerprint が変化せず検出できない
     (`scripts/deploy-container.sh` の `context_fingerprint()`)。podman の
     default connection 切り替えも同型。
   - **通常の `boid start` が既存 identity を検証せず上書きする。**
     engine/context の記録・照合は `--down` 専用で、`up` 側は現在選択された
     engine/context でそのまま up し、記録を無条件に上書きする
     (`scripts/deploy-container.sh` の up 成功後の state 書き込み箇所)。
     context=A で起動後、context=B に切り替えて再度 `boid start` すると
     A を確認せず B を起動して記録を B で上書きし、以後 `boid stop` は B だけを
     正常終了させ A は管理不能なまま残る。
   - **root-uid 検査は本人の実行環境の `BOID_UID`/uid のみを見る。** 別ホスト/
     別 uid からの多重起動や、multi-instance の並行運用は元々サポート対象外
     (単一ホスト・単一 daemon が前提) — この前提が崩れた場合の防御は未実装。

   対応する codex round-9〜14 の生ログは PR #892 の会話に残る。boid の想定運用
   (単一ホスト・単一ローカル docker/podman engine・単一 context) の前提が崩れる
   ケースが実際に発生したら、上記を re-open して対応する。

---

## PR8 完了時点の全体まとめ (release-onboarding プロジェクト完了、2026-08-02)

PR1〜PR9 が全て main にマージされ、`docs/plans/release-onboarding.md` の
「目標オンボーディングフロー」節に書いた 6 ステップ (`go install` → `boid check` →
`boid start` → `boid web pair` → `boid project add <git-url>` →
`boid workspace set-init-script` + `boid agent claude`) が実際に docs 通り動く状態になった
(getting-started 4 ページ・`guide/onboarding.md`・`reference/cli.md`・英語版を全面更新、
`boid start` 直後の「次にやること」誘導文言も実装済み — 本 PR の scope)。

このプロジェクトを通じて意図的に先送り・未対応のまま残っている既知の限界事項を一覧化する
(いずれも別 issue/PR での対応が要る):

1. **GHCR image の公開範囲は手動設定のまま。** 決定 4 で public にする方針は固まったが、
   実際に GHCR パッケージの visibility を public に切り替える操作自体は
   `.github/workflows/blackbox-e2e.yml` の push では自動化されておらず、初回 publish 後に
   GitHub 側の Package settings から手動で行う必要がある (未検証: 現時点でのリポジトリの
   実際の visibility 設定)。
2. **`boid project migrate <dir> --apply` は compose daemon 下で「半分だけ効く」。**
   穴 4 の表で洗い出した通り、socket 経由で daemon に届けば正しく適用されるが、
   届かない場合は host 側の別 DB (存在しなければ新規作成) に silent に書き込んでしまう
   フォールバック経路が残っている。`docs/ja/guide/onboarding.md` / `migration.md` に注意書きを
   追加したが (本 PR)、コード側の是正 (compose daemon 下での `--apply` の明示的な拒否、または
   届かない場合のエラー化) は未着手。
3. **PR5/PR7 で見送った edge case 群。** 上記「`scripts/deploy-container.sh` の
   engine-state 機構の残エッジケース」(dangling symlink な state dir、`docker context use`
   による fingerprint 外の context 切り替え、multi-instance 非対応) はいずれも
   「single-user personal orchestrator の実運用では発生確率が低い」と判断され明示的に
   先送りされている。round-9〜14 の生ログは PR #892 の会話に残る。
4. **multi-arch (arm64) は決定 5 により今回出さない。** Mac (Apple Silicon)・安い arm64 VPS
   向けの需要が実際に顕在化したら「論点: arm64」節の「将来 arm64 を足すときにやること」から
   再開する。
5. **CLI/daemon の version skew ハンドリングは未決 1 のまま。** `go install @latest` した
   CLI と古い image で動いている daemon が食い違う状況を検知して warn/refuse する仕組みは
   実装されていない (`/api/health` に version フィールドを足すところから)。
6. **既存 (旧 uid 焼き込みイメージ時代) install の移行手順は未検証。** 未決 5/6 (uid 安定契約)
   は実データを持つ環境 (khi workspace 等) での実地検証が必要なまま。
7. **`boid host-commands` の daemon 側ファイルを直接編集する経路が compose daemon 下では
   host から届かない。** `~/.config/boid/host_commands.yaml` は daemon コンテナの
   `boid_state` volume 内にあり、`boid host-commands list/reload` の read/reload API はあるが
   直接書き込む API/CLI コマンドは無い — 現状は `docker exec`/`podman exec` で daemon
   コンテナに入って編集する運用になる (本 PR で `guide/onboarding.md` に明記)。専用の
   API/CLI を追加するかは未検討。

これで release-onboarding プロジェクトの PR1〜PR9 は完了。今後のフォローアップは
上記のいずれか、または新たに見つかった穴を新規の plan doc として起票すること。

---

## 論点: arm64 を出すか → 決定: 出さない (決定 5、2026-08-01)

初回リリースは amd64 のみ。以下は将来 arm64 を足す判断をするときの材料として残す。
**「後から後方互換に追加できる」という判断の根拠が下記の「確認済みの事実」なので、
それが失効した場合 (Dockerfile に arch 依存が入る、CI から arm64 ランナーが消える等) は
この決定自体を再検討すること。**

なお決定 5 のうち **arch mismatch の fail-fast は arm64 を出さない場合こそ必須** で、
PR4 (`resolveImage`) と PR7 (`boid check`) に既に組み込んである。

### 確認済みの事実

**image 側は既に arch 中立に書けている。**
`build/container/Dockerfile` は base が multi-arch (`golang:${GO_VERSION}-bookworm`、
`debian:bookworm-slim`)、追加インストールは全部 apt で、gh の apt source 行も
`deb [arch=$(dpkg --print-architecture) ...]` (`:170`) と arch を動的に埋めている。
`grep -n "amd64\|arm64\|GOARCH\|platform\|buildx"` を Dockerfile / compose.yml /
deploy-container.sh / CI workflow に掛けて **ヒット 0**。
つまり **arm64 ビルドのための Dockerfile 変更は要らない見込み** (実ビルドでの検証は必要)。

**Go 側の変更はゼロ。** boid CLI は `go install` でユーザ環境がネイティブビルドするので
arch に無関係。image は manifest list を publish すれば
`internal/dispatcher/container_backend.go` の `resolveImage` → `ImagePull` が
ホスト arch を自動解決する。**multi-arch は publish (CI) だけの問題**。

**repo は PUBLIC** (`gh repo view` で確認)。したがって GitHub の arm64 ホストランナー
(`ubuntu-24.04-arm`) が無料で使える = **QEMU エミュレーションを避けてネイティブ 2 本立て +
manifest list** が組める。ここが一番効く事実で、buildx + QEMU 前提だった
「arm64 は高コスト」という見立ては成立しない。

**ただし workspace HOME 側は arm64 対応していない。**
リファレンス init.sh が `: "${GO_ARCH:=linux-amd64}"`
(`docs/examples/workspace-home-init.sh:46`) を既定にしており、
`:205` で `https://go.dev/dl/go${GO_VERSION}.${GO_ARCH}.tar.gz` を取る。
env で上書きできるが、既定のまま arm64 で回すと amd64 の Go tarball を落として壊れる。
volta (`:233` `get.volta.sh`) と claude (`:314` `claude.ai/install.sh`) は
インストーラ側が arch 検出するが、**volta の linux-arm64 サポートは要確認**。

### やる場合のコスト

- CI: image build job を 2 本 (amd64 / arm64 ネイティブランナー) + manifest list 作成 step。
  ネイティブなので wall-clock は並列で伸びない。
- **e2e-container を arm64 でも回すなら +1 job**。回さないと「arm64 対応」は未検証の主張になる。
- init.sh: `uname -m` 分岐の追加 (数行)。
- 継続コスト: arm64 固有の壊れ方を踏んだときの調査。既知の候補は
  Playwright chromium の arm64 提供状況 (`Dockerfile:133-141` が入れている native deps は
  apt に arm64 版があるが、Playwright 本体が落とす chromium バイナリ側は要確認)、
  および .NET の arm64 ICU。

### やらない場合に**必須**になるもの

**arch mismatch の fail-fast。** amd64 only の image を arm64 ホストが pull すると、
binfmt/qemu が入っている環境では **エミュレーションで動いてしまう** —
激遅 + 原因不明のクラッシュで調査が最悪になる。`boid check` と `resolveImage` で
「image の arch != host の arch」を検出して即座に失敗させる必要がある。
なおこれは **arm64 をやる場合でも要る** (manifest list に無い arch 向け)。

### 判断材料

| 対象環境 | arm64 が必要か |
|---|---|
| Linux デスクトップ / x86 VPS | 不要 |
| Mac (Apple Silicon) の Lima / OrbStack | **必要**。`docs/plans/vibe-coder-provisioning.md:97` は Mac ローカル VM を第二優先に置いていた |
| 安い arm64 VPS (Hetzner CAX / Oracle Ampere / AWS Graviton) | **必要**。同 doc はコスト最優先を論点にしていた |
| Windows WSL2 | 不要 (x86 が主) |

### 将来 arm64 を足すときにやること

決定 5 により今回は着手しない。足す場合の作業は publish 側に閉じている:

1. CI: image build job を amd64 / arm64 ネイティブランナーの 2 本にし、manifest list を作る
2. e2e-container を arm64 でも回す (回さないと「arm64 対応」は未検証の主張になる)
3. リファレンス init.sh の `GO_ARCH` を `uname -m` 分岐に変える
   (`docs/examples/workspace-home-init.sh:46`)
4. volta の linux-arm64 サポートと、Playwright chromium の arm64 提供状況を実機確認

1 と 2 だけでは足りず 3 が要る点に注意 — 「後から足せる」のは image の publish であって、
workspace HOME 側の toolchain インストールは別の穴として残っているため。

---

## 参照

- `docs/plans/volume-only-daemon.md` — volume-only 再設計、host mode (§論点c)、
  git URL 登録 (§論点a)、backend 単一化 (§論点e)
- `docs/plans/phase6-container-backend.md` — container backend 本体、共有 base イメージ (§決定2)、
  job コンテナの非 root 実行 (§決定4)
- `docs/plans/vibe-coder-provisioning.md` — 非エンジニア向けプロビジョニング。
  ただし内容は userns backend 時代のもので、ホストディストリ固定の根拠
  (`:129-158`「ホストの `/usr` を bind mount するのでホスト = 実行環境」) は
  container backend で失効している
- `docs/plans/cli-remote-connection.md` — scope 分類 (local / remote / neutral) の原典 (`:276`)
