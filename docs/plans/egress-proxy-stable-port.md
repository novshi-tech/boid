# egress プロキシのポート安定化

status: 実装済み
date: 2026-08-11

## 背景 — 実際に起きた事故

default workspace の `ubs-apps` で `pnpm install` が「終わらない」という報告があり、
調査したところ egress プロキシのポートが原因だった。

```
HTTPS_PROXY (env) = http://boid-egress:35547   ← 生きている
~/.npmrc          = http://boid-egress:38865   ← 死んでいる
```

`~/.npmrc` は永続 HOME volume 上にあり、`proxy=` / `https-proxy=` に
**その時点の**プロキシポートが焼き込まれていた。daemon を再起動するとポートが
変わるため、以降 npm / pnpm だけが接続不能になっていた。

観測された症状:

```
WARN GET https://registry.npmjs.org/statuses/-/statuses-2.0.2.tgz
     error (ECONNREFUSED). Will retry in 1 minute. 5 retries left.
...
FetchError: ... connect ECONNREFUSED 10.89.10.23:38865
```

この事故が厄介だったのは次の 3 点。

1. **他のツールは全部動く。** curl / git / boid 自身の通信は env の `HTTPS_PROXY`
   を見るので常に最新ポートを引く。設定ファイルを持つ npm / pnpm だけが壊れる。
   「ネットは生きているのに pnpm だけ死ぬ」という切り分けにくい見え方になる。
2. **失敗が静かで遅い。** pnpm は ECONNREFUSED をリトライ可能エラーとみなし、
   1 分バックオフ × 5 回 × 全パッケージを回す。エラーではなく「ハング」に見える。
3. **時限爆弾になる。** 書いた時点では動く。daemon 再起動という無関係な操作で
   後から壊れる。書いた本人も、踏んだ本人も因果を追えない。

`~/.npmrc` を書いたのは boid ではない (コードにも init.sh にも生成箇所は無く、
mtime から 2026-08-10 のセッションでエージェントが手で書いたものと判断した)。
つまり「boid のバグを直す」話ではなく、**サンドボックス内の誰かがプロキシ URL を
設定ファイルに焼くことは今後も起きる**という前提で構造を変える話になる。

## 現状

`internal/sandbox/proxy_manager.go` の `ProxyManager` が workspace ごとに
`*Proxy` を持ち、`Proxy.Start` が `net.Listen(host+":0")` でポートを取る
(`internal/sandbox/proxy.go:70`)。`:0` はカーネル任せのエフェメラル採番なので、
**プロセスが上がるたびに違うポートになる**。

- bind host は container backend では `0.0.0.0` (`internal/server/server.go` の
  `composeBindHost`)
- workspace ごとの分離は「listener ごとに別ポート、別 allowlist」で成立している
- job からは compose ネットワークの DNS 別名 `boid-egress` で引き、ポートだけが
  workspace を選ぶ

## 目的 / 非目的

**目的:** daemon を再起動しても、ある workspace の egress プロキシのポートが
変わらないこと。焼き込まれた `http://boid-egress:<port>` が生き続けること。

**非目的:**

- プロキシ URL を設定ファイルに焼くこと自体を防ぐ (防げない。前提として受け入れる)
- workspace 間の分離方式を変える (現状のポート分離を維持する)
- bind host を変える (`0.0.0.0` のまま)

## 方式

`:0` をやめ、**workspace ごとに決定的なポートを割り当てて永続化し、起動時に
再利用する**。bind host とネットワーク構成には一切触らない。

```
boid-egress:30412  ← default (再起動しても不変)
boid-egress:30871  ← khi     (再起動しても不変)
```

検討した対案として「全 workspace 同一ポート (例 3128) + daemon の
workspace ネットワーク上の IP で分離」があった。URL が完全に一様になる利点が
あるが、採らない。workspace ネットワークは dispatch 時に遅延生成され、daemon は
その時点で自己アタッチする (`internal/dispatcher/container_backend.go` の
`ensureWorkspaceNetwork`)。daemon が自分の IP を知れるのはアタッチ後なので、
「ネットワークが生えるたびに listener を追加 bind し、作り直されたら張り直す」
機構が要る。今回の目的 (ポートを不変にする) に対して機構が重すぎる。

### ポート帯

**30000–32767 を既定とする** (2768 ポート)。

この帯を選ぶ理由は、エフェメラルポート帯を避けるため。実測:

```
$ cat /proc/sys/net/ipv4/ip_local_port_range
32768	60999          # host / daemon container とも同じ
```

エフェメラル帯 (32768 以上) から固定ポートを選ぶと、daemon 自身の外向き接続が
たまたまその番号を送信元ポートとして掴んでいる間に再起動が重なると bind に失敗する。
現行の `:0` は毎回空きを取るのでこの問題が無かったが、固定化すると顔を出す。
30000–32767 はエフェメラル帯の下限より下で、かつ well-known / 主要な登録済み
サービスとも当たらない。

`ip_local_port_range` を下げている環境では前提が崩れるため、帯は config で
上書きできるようにする。

```yaml
sandbox:
  egress_proxy_port_low: 20000
  egress_proxy_port_high: 20999
```

既定値の literal は `internal/sandbox` の `DefaultProxyPortRange{Low,High}` に
1 箇所だけ置き、config 側は「未設定 = 0 = sandbox の既定に従う」とする。
片側だけの設定は起動時エラーにする — もう片方が operator の書いた値とは別の
出所から来てしまい、意図しない帯になるため。

### 永続化先

`workspaces` テーブルへの列追加ではなく、**side table** を新設する
(`0036_add_workspace_watchdog.sql` と同じ流儀)。

```sql
CREATE TABLE IF NOT EXISTS workspace_egress_port (
    proxy_key   TEXT PRIMARY KEY,
    port        INTEGER NOT NULL UNIQUE,
    updated_at  DATETIME NOT NULL
);
```

判断の理由:

- **`workspaces` の列にしない。** workspace meta は `boid workspace export` /
  `apply` でユーザが往復させる面であり、daemon が採番した実装都合の値が
  そこに現れると、export した yaml を別環境に apply した時に無意味な、あるいは
  有害な値を持ち込む。ユーザが編集できてはいけない値なので面を分ける。
- **FK を張らない。** キーは workspace slug とは限らない。
  `dispatcher.NoWorkspaceProxyKey` (`__no_workspace__`) という、実在の workspace
  slug には決してならない予約キーの listener がある
  (`internal/dispatcher/runner.go`)。`REFERENCES workspaces(slug)` にすると
  このキーが入らない。列名も `workspace_id` ではなく `proxy_key` とし、
  「これは workspace とは限らない allocator のキーである」ことを型で表す。
- `port` に `UNIQUE` を張り、二重割当を DB 側でも弾く。

### 割当アルゴリズム

`GetOrCreate(key, allowed)` の listener 新規作成パスを次にする。

1. 永続化済みの `port` があり、かつそれが現在の帯の内側なら、その番号で bind を試みる
2. 成功 → その listener を使う (**ここが本命のパス**)
3. 失敗、未割当、または帯の外 → 帯から新しい番号を選んで bind し、結果を永続化
   (既存レコードがあれば上書き)

**帯の外なら再採番する**のは、帯の変更が効くようにするため。永続ポートを
無条件に優先すると、`ip_local_port_range` と衝突していて帯を移した operator に
対して、まさにその移動が必要な workspace だけが古いポートに居座り続ける。

新規採番は `hash(key)` を帯のサイズで割った位置から線形探索する。
乱数ではなく hash にするのは、DB を失った場合でも同じ workspace が同じポートに
戻る確率を上げるため (ベストエフォート、保証はしない)。

探索は 2 パスに分ける。**1 パス目は他のキーが予約済みのポートを飛ばす。**
bind が通るかどうかだけで空きを判定してはいけない — listener は dispatch 時に
遅延生成されるので、前回起動以降 dispatch されていない workspace は
「予約はあるが誰も listen していない」状態にある。これは bind プローブでは
空きポートと区別できず、奪うと**その workspace のポートが黙って変わる**
(奪った側が相手の行を消すので、相手は次の dispatch で「記録なし」として
別のポートを取る)。これはこの機能が防ごうとしている事故そのものであり、
しかも両端どこにもログが出ない。1 パス目で空きが無かった場合だけ、2 パス目で
予約済みポートを奪い、その旨を犠牲になったキー名込みで warn に出す。

帯が埋まっている場合は **`:0` にフォールバックし、warn を出す**。
その事実はプロセス内に記憶し、以降の新規キーで同じ全走査を繰り返さない
(走査は dispatch が取るロックの下で走るため)。
ポートが不安定になるのは今と同じ状態に戻るだけであり、dispatch 自体を
失敗させる理由にはならない。fail-closed にしない判断はここが「利便性の最適化で
あって隔離の仕組みではない」ため — 隔離は allowlist と workspace ごとの
listener 分離が担っており、ポート番号が固定かどうかは隔離に寄与しない。

### bind 失敗時の扱い

永続化されたポートが他プロセスに取られていた場合 (2 の失敗)、その旨を warn で
出した上で 3 に落ちる。ここで諦めない理由は、ポート固定化が「壊れたら dispatch
できない」機能になってはいけないから。ただし**黙って番号を変えない** —
焼き込まれた設定が腐る瞬間そのものなので、ログに旧ポートと新ポートの両方を出す。

```
WARN egress proxy: the persisted port was unavailable, reallocated
     key=default old_port=30412 new_port=30988
     hint=job-side configs that baked the old port (e.g. ~/.npmrc) need updating to the new one
```

この warn は**再採番が終わってから**出す。走査の前に出すと `new_port` が
まだ存在せず、operator は「`~/.npmrc` が腐った」とだけ告げられて、
何に書き換えればいいのかを知る手段が無い。

## 移行

既存の焼き込み設定に対する自動修復は**やらない**。`~/.npmrc` は boid が書いた
ファイルではなく、boid が勝手に書き換えるのは越権 (`~/.claude/projects/` 配下に
手を出さないのと同じ理屈)。

初回の実装後、既存 workspace は「次の dispatch で採番されて以降不変」になる。
今回の `ubs-apps` の件のように既に腐っている設定は個別に外す (実施済み:
`~/.npmrc` を `~/.npmrc.stale-proxy.bak` に退避)。

## テスト方針

- `ProxyManager` が、永続化済みポートを渡されたらその番号で bind すること
- 永続化済みポートが塞がっている時に、別の番号へ落ちて**かつ永続化を更新**すること
- 帯が枯渇した時に `:0` へ落ちて dispatch が成功すること
- 採番が帯の内側に収まること (エフェメラル帯に踏み込まないことの回帰テスト)
- `__no_workspace__` キーが永続化できること (FK を張らなかったことの回帰テスト)
- store を持たない `ProxyManager` (既存テスト / 非 container 経路) が
  従来どおり `:0` で動くこと

`internal/sandbox` は `internal/db` を import しない。ポートの読み書きは
`ProxyAllocator` と同じ流儀で **interface を注入**する。

```go
type PortStore interface {
	LoadPort(key string) (port int, ok bool, err error)
	SavePort(key string, port int) error
	ReservedPorts() (map[int]string, error)
}
```

nil の場合は現行どおり `:0` — 既存の呼び出し側とテストを一切壊さない。

`ReservedPorts` は「bind できるか」と「予約されているか」が別の問いである
ために要る (上記 2 パス探索を参照)。

さらに、**config → server → ProxyManager の接続そのもの**にもテストを置く。
両端 (config.yaml → server.Config、ProxyManager の採番挙動) だけを検証すると、
接続の 3 行を消しても全テストが緑のまま、全プロキシが黙ってエフェメラルに
戻る。

## 影響範囲

- `internal/sandbox/proxy.go` — `Start` に希望ポートを渡せるようにする
- `internal/sandbox/proxy_manager.go` — `PortStore` 注入、採番と fallback
- `internal/db/migrate/migrations/0039_add_workspace_egress_port.sql` — 新テーブル
- `internal/orchestrator` — `PortStore` の実装 (DB 側)
- `internal/server/server.go` — 配線 (`New`)
- `internal/config` — ポート帯の既定値と config キー

ネットワーク構成、bind host、allowlist、workspace 分離のいずれにも変更は無い。
