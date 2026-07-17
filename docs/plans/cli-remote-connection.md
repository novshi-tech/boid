# CLI リモート接続 実装計画

ステータス: 計画 (planned)
作成日: 2026-07-16
更新日: 2026-07-16 (codex レビュー反映、blocker 1 件 + major 4 件を吸収)
親ドキュメント: [container-based-boid.md](container-based-boid.md) — 移行戦略 **Phase 3**
前提 phase: [workspace-db-consolidation.md](workspace-db-consolidation.md) — Phase 2.5

---

## 目的

boid CLI を Web UI と同格の「ペアリング済みデバイス」に寄せ、UNIX ソケット依存を
外して TCP + device auth 経由でリモート daemon に接続できるようにする。broker 側が
コンテナ / 別ホストに移った時に CLI 操作経路が断絶しないようにする準備。

副産物として、boid の「Linux のみ対応」制約は実質サーバ側 (userns / pivot_root / PTY /
ws_attach.go の `//go:build linux`) だけなので、リモートクライアント経路を
linux-only import から切り離せば **Mac / Windows 向けクライアント専用バイナリが
ほぼタダで得られる**。本 phase では意識だけしておく (実物のクロスビルドは別 phase)。

スコープ外:
- タスクコンテキスト伝搬の boid コマンド化 (Phase 5)
- 認証付き CLI (gh / az / aws / fly) の host command 方式による汎化 (Phase 6)
- Mac / Windows 向けクライアントバイナリのクロスビルド (副産物、別 phase)

---

## 決定事項 (2026-07-16 nose)

1. **connection profile 管理**: `~/.config/boid/config.yaml` に profile map を持つ。
   切替は `BOID_PROFILE=<name>` env / `--profile <name>` flag / `default_profile`
2. **device token 保存**: `~/.config/boid/tokens/<profile>.json` (0600)、親 dir 0700。
   起動時に権限緩ければ警告。system keychain 対応は将来オプション。
   config/token の更新は temp file + `rename` + advisory lock で atomic 化
3. **`boid login <url>` UX**: ローカル `boid web pair` の逆版 (pair code 入力 → device
   token 交換 → token + profile entry 保存)。`boid logout <profile>` もセット追加
4. **transport 判定**: URL scheme で分岐 — `unix://` (UNIX socket) / `https://`
   (TCP + TLS + Authorization ヘッダ)。`http://` は非対応 (localhost debug は unix で足りる)
5. **WebSocket attach を一本化**: ローカル・リモート両方 WS 経路に統一、独自
   `Upgrade: boid-attach` 経路 (server + client 両側) は撤去。二経路併存の保守負担を避ける
6. **CLI コマンド分類は三値必須** (`remote` / `local` / `neutral`)、
   `PersistentPreRunE` で判定、cobra Annotations 未設定は build fail
   (fail-open にしない)。profile が非 unix + `local` → エラー
7. **コンテナ daemon の初回ペアリング**: 特別な機構は導入しない。
   `docker exec boid boid web pair` で code 発行 → CLI で `boid login` で入力、
   の既存フローで十分 (コンテナ exec できる人 = 管理者、認可の起点として自然)
8. **認証機構は Bearer token を新設して既存 cookie と併存** (2026-07-16 nose 判断)。
   現行 auth は Web UI 用の `boid_session` cookie 検証のみで、Bearer token /
   `POST /api/auth/device` / `DELETE /api/auth/devices/{id}` は未実装。
   本 phase の PR で以下をサーバ側新設:
   - `POST /api/auth/device` (公開・rate-limit 付き、pair code redeem)
   - `DELETE /api/auth/devices/{id}` (Bearer 認証済み、自 device revoke)
   - HTTP / WebSocket auth middleware に Bearer 検証を追加 (cookie 経路は温存)
   - 既存 `web_devices` テーブルを Bearer token 用にも拡張 or 別テーブル
9. **token は canonical origin に強く bind**: profile URL が token 発行元と不一致なら
   **hard error** (再 login 必須)、warning で通過しない。cross-origin redirect も禁止
10. **profile 名は slug 検証** (`[a-z0-9][a-z0-9_-]*`)、path traversal を排除

---

## Config schema

`~/.config/boid/config.yaml` の profile 部:

```yaml
default_profile: home
profiles:
  home:
    url: unix:///run/user/1000/boid.sock
  work:
    url: https://work.example.com
    # token は tokens/<profile>.json から自動読み込み
  laptop:
    url: https://laptop.tail-xxxxx.ts.net
```

- profile 未指定 → `default_profile` → 未設定なら `DefaultSocketPath()` (現行互換)
- `--profile` flag > `BOID_PROFILE` env > `default_profile` の優先順
- `url` scheme で transport 分岐 (下記)

`~/.config/boid/tokens/<profile>.json` (0600):

```json
{
  "device_id": "dev_xxxxxxxx",
  "token": "tk_xxxxxxxxxxxxxxxx",
  "issued_at": "2026-07-16T12:00:00Z",
  "url": "https://work.example.com"
}
```

`url` は verification 用に保存 (profile の url と一致しなければ警告)。

---

## Transport 分岐

現行 CLI は `internal/client/client.go` の `NewUnixClient(DefaultSocketPath())` を
**多数の cobra command が直接 call** しており、DialContext 1 点変更だけでは接続先が
切り替わらない。root で profile 解決済み client を生成し、各 command に注入する構造に
変える。

### 構造

```go
// rootCmd の PersistentPreRunE で 1 度だけ実行:
func resolveClient(cmd *cobra.Command) (*client.Client, error) {
    profile, err := profiles.Resolve(cmd)  // --profile / BOID_PROFILE / default_profile
    if err != nil { return nil, err }
    token, err := tokens.Load(profile.Name) // profile が unix なら空
    if err != nil { return nil, err }
    return client.NewClient(profile.URL, token)
}
// resolved client を context に載せる or PersistentPreRunE で cmd.SetContext
```

### `NewClient` の scheme 分岐

```go
func NewClient(rawURL string, token string) (*Client, error) {
    u, err := url.Parse(rawURL)
    if err != nil { return nil, err }
    switch u.Scheme {
    case "unix":
        return newUnixClient(u.Path)  // token は無視、autostart 対応
    case "https":
        return newHTTPSClient(u, token)  // Bearer 認証、autostart なし
    default:
        return nil, fmt.Errorf("unsupported scheme: %s", u.Scheme)
    }
}
```

### 影響範囲

- HTTPS 経路: `Authorization: Bearer <token>` ヘッダを全リクエストに乗せる
- WS 経路も同ヘッダ (`internal/api/ws_attach.go` の auth middleware を Bearer 対応に拡張)
- UNIX socket 経路は現行互換 (Authorization ヘッダなし、cookie/Bearer どちらも不要)
- **daemon autostart は unix profile 時のみ動作** (`login` / `logout` は autostart 対象外)
- attach / observe / poll helper / completion 等の CLI ヘルパも profile 解決後の
  client を経由する形に統一

### PR1 スコープ拡張 (codex 指摘反映)

DialContext 1 点だけでは足りない。以下を PR1 に全部含める:
- `internal/client` の `NewClient` 汎化
- profile 解決 helper (`internal/profiles`)
- root の `PersistentPreRunE` で client を context に載せる仕組み
- 既存 command から `client.NewUnixClient(client.DefaultSocketPath())` の直接 call を
  root 注入経由に差し替え (全 CLI command で単純置換)
- attach / observe / autostart helper の profile 対応
- `login` / `logout` は autostart / profile 前提を要求しない例外処理

---

## login / logout フロー

`boid login <url> --profile <name>`:

1. `--profile` 未指定なら URL host から候補生成 (例: `work.example.com` → `work`)
2. profile 名を **slug 検証** (`[a-z0-9][a-z0-9_-]*`)、path traversal 排除
3. CLI が `Enter pairing code: ` prompt (ttyから読む)
4. code 入力 → `POST <url>/api/auth/device` (body: `{code, device_name}`)
   - device_name は hostname デフォルト (`--device-name` で override)
5. daemon が pair code 検証 → device token 発行 → response には token + canonical URL
6. CLI が `~/.config/boid/tokens/<profile>.json` (0600、temp+rename) 保存
   - JSON body に **canonical URL** を含める (URL 変更検出用)
7. `~/.config/boid/config.yaml` の profiles に entry 追加 (or update、temp+rename)
8. `default_profile` 未設定なら本 profile を default に

**token cross-origin 防御** (毎リクエスト時):
- `config.yaml` の profile URL と token JSON の `url` を照合
- **不一致は hard error** (再 login 必須)、warning でスルーしない
- HTTPS リダイレクトも別ホストへの follow は禁止 (`CheckRedirect` で origin 一致検証)

`boid logout <profile>`:

1. `~/.config/boid/tokens/<profile>.json` を読む
2. `DELETE <url>/api/auth/devices/<device_id>` (revoke) — 失敗しても続行 (net 断で warning)
3. token file 削除 (unlink)
4. `config.yaml` から profile entry 削除 (temp+rename)
5. `default_profile` が本 profile なら unset

### コンテナ daemon の初回ペアリング

特別な機構は入れない。運用フロー:

1. コンテナ管理者が `docker exec <container> boid web pair` (または `kubectl exec`) 実行
2. daemon が pair code 発行 (5 分有効、単回)
3. リモート CLI で `boid login <url>` → code 入力
4. 通常のペアリング完了

**Why:** コンテナに exec 可能な人 = 管理者、なので既存 `boid web pair` フローで
認可の起点として十分。env 注入・自動 stdout 発行等の追加機構は初期スコープ外。

---

## WebSocket attach 一本化

現行:

- ローカル attach: `cmd/attach.go` → `internal/client.Client.AttachJob` → 独自
  `Upgrade: boid-attach` (`internal/client/client.go:165`)
- Web UI attach: `GET /api/jobs/{id}/attach/ws` WebSocket (`internal/api/ws_attach.go`)

移行後:

- CLI 両経路とも WS に統一
- サーバ側の独自 Upgrade 経路 (`internal/api/*` の boid-attach ハンドラ) を撤去
- CLI 側の `AttachJob` は WS 経由の raw mode + input/resize/output/exit フレーミングに書き換え
- ローカル (unix socket 上の WebSocket upgrade) も同じコード経路

フレーム構成 (既存 `ws_attach.go` の仕様に準拠):

- `input`: base64 encoded stdin バイト
- `resize`: `{rows, cols}` (SIGWINCH → 送信)
- `output`: サーバ → クライアントの PTY 出力
- `exit`: `{code}` 終端

CLI 側実装は `cmd/attach.go` の `makeRawInput` / `detachReader` (Ctrl+] 検出) を流用、
`io.Copy(conn, stdin)` を base64 input フレーム送信に差し替え。

---

## コマンド分類 (三値必須)

cobra command の Annotations で分類、**未設定は build fail** (fail-open にしない)。

```go
const (
    scopeRemote  = "remote"   // リモート profile でも動作
    scopeLocal   = "local"    // unix profile のみ、非 unix なら明示エラー
    scopeNeutral = "neutral"  // profile 解決不要 (login/logout/completion 等)
)

var startCmd = &cobra.Command{
    Use: "start",
    Annotations: map[string]string{annotationScope: scopeLocal},
    ...
}
```

**未分類検出は build 時テスト**で行う:

```go
// TestAllCommandsHaveScopeAnnotation walks rootCmd and fails on any leaf
// command that lacks the scope annotation.
```

`rootCmd.PersistentPreRunE` で判定 (scope が `local` かつ profile が非 unix → エラー):

```
'boid start' はローカル専用コマンドだよ。
現在の接続先: https://home.example.com (profile: home)
ローカル操作するときは --profile <local-profile> を指定してね。
```

### 分類一覧 (全 leaf command)

| Scope | Command | 備考 |
|---|---|---|
| `local` | `start` / `stop` / `check` | daemon 生殺与奪 |
| `local` | `fetch` | sandbox 内 web 取得補助、ローカル shim 依存 |
| `local` | `runner-*` | 内部プラミング |
| `local` | `project migrate` | SQLite 直開き |
| `local` | `gc` | ローカル runtime dir 削除 |
| `local` | `init` | 初回セットアップウィザード |
| `local` | `web set-url` / `web set-addr` | daemon 再起動が必要 (Phase 2.5 決定) |
| `remote` | `task` (全 sub) / `job` / `action` / `secret` | HTTP API のみ |
| `remote` | `attach` / `observe` | WS 経由 (本 phase で統一) |
| `remote` | `agent` / `agent-session` | daemon dispatch (exec と同型) |
| `remote` | `exec` | **codex 指摘**: daemon API dispatch なのでリモート可 |
| `remote` | `workspace` (list/show/create/edit/remove/export/import/assign/clear) | Phase 2.5 で API 化済 |
| `remote` | `host-commands list / reload` | Phase 2.5 で API 化 |
| `remote` | `web pair` / `devices` / `revoke` / `revoke-all` | 既存 API |
| `neutral` | `login` / `logout` | profile 前提を要求しない |
| `neutral` | `completion` / `help` | daemon 不要 |
| **境界越えで壊れる** | `project add` / `init` / `reload` | work_dir のパス文字列を daemon 側 FS で解決 |

`project add/init/reload` は本 phase では `local` に分類 (リモート時明示エラー)。
Phase 6 で project = リモート git URL 前提化される時点で `remote` へ移行。

**2026-07-17 追記 (workspace-db-consolidation PR4 codex レビュー2巡目)**:
`cmd/scope_annotations_test.go` の `expectedScopeAnnotations` を全 leaf command
の exact-match 表に強化した際、`project add/init/reload` の実装側 annotation が
本表と食い違っている (当時は `remote` のままだった) ことが判明したため、
PR4 (Phase 2.5) の時点で前倒しでこの表の分類に合わせて `local` に変更した。
本来は PR5 (`PersistentPreRunE` の拒否 UX 込み) でまとめて対応する想定だった
ものの、annotation 自体の食い違いは早期に閉じておく方が安全と判断。PR5 で
やるべき残作業は「リモート profile 時に明示エラーを出す」動作の実装のみ
(`PersistentPreRunE` 側は未着手)。

同じレビューで `gc` と `check` も本表と食い違っていた
(実装は `gc`=`remote`、`check`=`neutral`)。`gc` は実際には daemon の HTTP API
(`POST /api/gc`) を経由するだけの実装で、リモート daemon 相手でも機構的には
問題なく動作する — が、本表の「daemon lifecycle 系はまとめて local」という
分類方針に合わせて `local` に統一した。`check` は exec.LookPath/unshare
プローブが「CLI プロセスを実行しているホスト」を調べる性質上、リモート
daemon 時には無意味な情報になる (daemon 側ホストではなく CLI 側ホストを
見てしまう) ため、本表の分類 (`local`) の方が実は技術的にも妥当と判断し
`local` に変更。詳細な理由は `cmd/gc.go` / `cmd/check.go` の annotation
コメントを参照。

### Phase 2.5 で解消済 (もう論点でない)

- workspace 系: 全 API 化 → `remote`
- workspace configure: 廃止
- kit list/remove/init: 廃止
- attach / agent 系の独自 Upgrade: WS 一本化で解決 (本 phase)

### shim 経路 (sandbox 発) との関係

本 phase の scope annotation は **CLI 経路 (host 発、cobra ツリー)** のみを対象とする。
サンドボックス内のエージェント / shim が叩く boid コマンドは別経路であり、
分類対象に含めない。

boid バイナリは `main.go` 段階で env を見て 2 つの経路に分岐する:

| 経路 | 分岐条件 | パーサ | 通信先 |
|---|---|---|---|
| **CLI 経路 (host 発)** | 通常起動 | `cmd/*.go` の cobra ツリー | daemon HTTP API |
| **shim 経路 (sandbox 発)** | `BOID_BUILTIN_SHIM=1` | `internal/sandbox/boid_shim.go` の独自パーサ | broker RPC (`BOID_BROKER_SOCKET`) |

shim 経路は cobra を完全にバイパスするため、scope annotation の付与先自体が
存在しない。ルーティングは env による経路分岐 (transport 層) で行われ、
scope annotation の層 (`PersistentPreRunE` 判定) より下にある。

**Phase 5 で追加される boid コマンドの扱い** (`boid task context` /
`boid workspace env` 等):

- 原則として **shim 経路にのみ追加**する (`parseBoidRequest` に case を足す)。
  cobra 側に出さない限り scope annotation は不要
- 既にホスト cobra 側と shim 側で `boid task` の subcommand セットは分離
  (cobra 側 = `create/list/show/update`、shim 側 = `ask/notify/answer/reopen` 等)。
  同名 top-level noun を共有しつつ subcommand で用途を分けるパターンは維持する

**dual-facing (両経路に置くケース) の判断基準**: ホスト側からの debug / 観察
用途が実運用で必要になった場合のみ、cobra 側にも同名 command を追加する。
その場合の cobra 側は `remote` scope (daemon HTTP API 経由) とし、shim 側とは
別実装として維持する (実装共有はしない)。**維持コストが 2 倍になる自覚を
持って追加する** — 安易に両方置くと、両宇宙で subcommand セットが drift して
混乱の元になる。

---

## PR 分割案

| # | 内容 | 依存 |
|---|---|---|
| PR0 | **サーバ側 Bearer auth 新設** (先行) | — |
|    | `POST /api/auth/device` (public、rate-limit、pair code redeem) | |
|    | `DELETE /api/auth/devices/{id}` (Bearer 認証、自 device revoke) | |
|    | HTTP / WebSocket auth middleware に Bearer 検証追加、cookie 経路温存 | |
|    | Bearer token 用の schema 拡張 (既存 `web_devices` に列追加 or 別テーブル) | |
| PR1 | **profile 基盤 + transport swap + client 注入構造** | PR0 |
|    | `~/.config/boid/config.yaml` の profile 定義パース (`internal/profiles`) | |
|    | `NewClient(url, token)` scheme 分岐 (unix + https)、HTTPS で Bearer ヘッダ | |
|    | `--profile` flag + `BOID_PROFILE` env + `default_profile` 解決 | |
|    | root `PersistentPreRunE` で profile 解決 → client 生成 → context 注入 | |
|    | 既存 command から `client.NewUnixClient(client.DefaultSocketPath())` 直接 call を | |
|    |   注入経由に一括置換 | |
|    | daemon autostart は unix profile 時のみ動作 | |
|    | token URL bind の hard error 検証 (再 login 促す) | |
|    | profile 名 slug 検証 | |
| PR2 | `boid login` / `boid logout` + token 保存 | PR0 + PR1 |
|    | pair code prompt + `POST /api/auth/device` 呼び出し | |
|    | `~/.config/boid/tokens/<profile>.json` (0600、temp+rename) 保存 (canonical URL 含む) | |
|    | `config.yaml` の profile entry 追加 / 削除 (temp+rename+lock) | |
|    | logout 時の daemon 側 revoke 呼び出し | |
|    | `login` / `logout` は `neutral` scope (profile 前提を要求しない) | |
| PR3 | WebSocket attach 一本化 | PR0 + PR1 |
|    | CLI 側 WS attach クライアント実装 (raw mode + フレーミング) | |
|    | 独自 Upgrade 経路 (server 側 handler + client 側 AttachJob) 撤去 | |
|    | Bearer 認証を WS handshake で通す | |
|    | e2e: ローカル attach regression + リモート attach smoke | |
| PR4 | コマンド分類の三値必須化 + `PersistentPreRunE` 拒否 UX | PR1 |
|    | 全 leaf CLI コマンドに `scope` Annotations 付与 | |
|    | 未分類検出テスト (build 時 fail) | |
|    | エラーメッセージ実装 | |
| PR5 | `project add` / `init` / `reload` のリモート時明示エラー | PR1 + PR4 |
|    | 境界越えで壊れるコマンドを `local` に分類 (現時点の妥協) | |
|    | Phase 6 で project = リモート git URL 前提化される時点で `remote` に移行 | |

PR0 (サーバ側 Bearer 新設) が先行必須。PR1-4 で本 phase の実質実装、
PR5 は境界越えの明示エラー (Phase 6 まで持ち越しの繋ぎ)。

---

## e2e 影響

- ローカル UNIX socket 経路 regression: 既存全 e2e シナリオが profile 未指定
  (default) で通ること
- HTTPS 経路 smoke: pair → login → task 作成 / attach / observe の基本フロー
- ローカル専用コマンド拒否: HTTPS profile で `boid start` 等叩いた時の明示エラー
- 独自 Upgrade 経路撤去の regression: attach まわりの全 e2e

---

## 未解決論点

- **CLI クライアントの linux-only 依存の切り離し具体**: 副産物として Mac/Win バイナリ
  を出す前提を守るなら、`internal/client` と CLI cmd から linux-only import
  (userns / PTY 系) を除去する必要。本 phase では意識だけ、実物のクロスビルドと
  build tag 整備は別 phase
- **profile 補完 (シェル completion)** の要否: profile 名の tab 補完は Phase 3 スコープに
  入れるか、後付けか
- **pair code の入力 UX**: パスワード入力風にエコー抑制するか、平文表示か
  (5 分失効・単回・肩越し覗き見リスク低なので平文でも問題は小さい)
- **リモート attach 時の SIGINT / SIGQUIT の意味論**: raw mode で Ctrl+C はサーバ側
  PTY にそのまま入力バイトとして渡す (SSH と同じ)。CLI プロセス自体の signal handling
  との干渉確認

---

## Phase 5 との関係

Phase 5 でタスクコンテキスト伝搬が boid コマンド (shim → broker RPC) 一本化される。
本 phase (Phase 3) の CLI は broker (= daemon) との通信路の transport を差し替える
だけなので、Phase 5 と直交する。ただし Phase 5 で `boid task context` / `boid
workspace env` 相当が追加されるので、それらもリモート可コマンドとして
Annotations 分類対象に入る (Phase 5 実装時に追加)。

---

## 親 phase との関係

- ① host command 契約 (完了)
- ② git gateway + branch policy (完了)
- 2.5 workspace DB 一元化 (前提 phase、[workspace-db-consolidation.md](workspace-db-consolidation.md))
- **③ CLI リモート (本 phase)**
- ④ $HOME volume
- ⑤ shim + context RPC
- ⑥ container backend

**Why:** CLI が UNIX socket 前提のままだと Phase 6 (broker がコンテナ / 別ホスト) で
一切の CLI 操作が断絶する。前倒しで transport swap を済ませておくことで、
Phase 6 は enforcement 差し替えに集中できる。副産物として Mac/Win クライアントの
選択肢もほぼタダで手に入る。

**How to apply:** 本 phase 完了後は「CLI = ペアリング済みデバイス」が既定モデル。
新しい CLI コマンドを追加する時は cobra Annotations の local_only 分類を必ず
検討する (デフォルトはリモート可)。ローカルパス引数を daemon に渡す設計は避け、
CLI プロセス側で読み書きしてバイト列を body に載せる (既存全コマンド準拠)。
