# Phase 6 後続: 旧デプロイ・userns backend・host daemon 起動経路の撤去計画

ステータス: **draft (計画のみ・実撤去は未着手)**。
作成日: 2026-07-23 (Phase 6 PR9 finale の一部として新設)。
親ドキュメント: [phase6-container-backend.md](phase6-container-backend.md) — Phase 6 本体 (全 9 PR landed)。

このドキュメントの scope は撤去の**計画**のみ。実撤去のコード変更は本 doc が定義する PR 群 (未着手)
で行う。PR9 自身は「撤去可能な形の準備 (deprecation skeleton)」までしか行わない —
`docs/plans/phase6-container-backend.md` §PR9 の「実撤去は禁止」を参照。

---

## 背景

[phase6-container-backend.md](phase6-container-backend.md) の目的節が定義した移行戦略:

> container backend が dogfood で安定したら、**旧デプロイ・userns backend・host daemon 起動経路を撤去**する。
> **恒久 2 構成サポートはしない** (nose 決定)。userns backend は「撤去前提の短期 fallback」。

Phase 6 PR1–9 (全 landed, 2026-07-23) で、この移行の**道具立て**は揃った:

- `SandboxBackend`/`SandboxSession` interface (PR1) — userns/container 両実装を同じ契約に集約。
- 共有 base イメージ + container entrypoint (PR2)。
- `sandbox.Spec` → docker realization 層 (PR3)。
- broker/gateway/dockerproxy TCP(mTLS) (PR4)。
- `containerBackend` 実装 (PR5)。
- daemon コンテナ化 + compose スタック (PR6)。
- 起動時 reap + 診断 + `sandbox.backend: container` cutover config (PR7)。
- file fallback 撤去 (PR8、backend 非依存)。
- container e2e CI (`e2e-container` job) + 実装ギャップの修正 (PR9)。

**ここから先の撤去は、まだ実行していない** — `sandbox.backend` は現行 config で `container` を明示
opt-in しない限り既定 `userns` のままで、host 旧デプロイ (bare `boid start`) も引き続き完全にサポート
されている。この doc はその撤去を安全に進めるための段階・依存関係・タイムラインを定義する。

---

## 撤去 3 段階 + config option fold の依存関係

撤去は独立した 3+1 段階に分かれ、**後段は前段の完了 (=一定期間の実運用安定確認) を前提とする**:

```
①dogfood 期間の並走 (container backend で実運用、安定性データ収集)
    ↓ 安定確認 (nose 判断)
②userns backend 撤去 PR 群
    ↓
③host daemon 起動経路撤去 PR
    ↓
④config `sandbox.backend` option 撤去 PR (container only 化)
```

①を経ずに②以降へ進んではならない — [phase6-container-backend.md](phase6-container-backend.md) の
rollback 契約 (「host 旧デプロイへの deploy-level rollback」) は①の期間だけ有効で、②が着手された時点で
その安全網は失われる。

### タイムライン (目安、nose 判断で前後する)

- **stable 2 週** (①): container backend (`sandbox.backend: container` の compose デプロイ) を nose +
  同僚の実運用で走らせ、host 旧デプロイへの rollback を一度も要さずに 2 週間安定稼働することを確認する。
  この期間中は host 旧デプロイを常に起動可能な状態に保つ (バイナリ更新・DB migration は決定 4 の
  「加法的変更に限定」制約を維持)。
- **→ userns 撤去 PR** (②): 下記「userns backend 撤去」参照。
- **1 週** (②のバーンイン): userns 撤去後、container backend のみで最低 1 週間の実運用を確認する。
- **→ host daemon 撤去 PR** (③): 下記「host daemon 起動経路撤去」参照。
- **1 週** (③のバーンイン)。
- **→ config option 撤去 PR** (④): 下記「config `sandbox.backend` option fold」参照。

各段階のバーンイン期間中に問題が出た場合、**その段階の PR を revert し、①の状態 (両 backend 併存) まで
戻す** — 段階を飛ばした部分撤去 revert は複雑さが跳ね上がるため避ける。

---

## ⓪ broker TCP wire completion (PR9 finale gap)

PR9 の実 docker verification (`e2e-container` job) で判明した、 PR4 の broker TCP mTLS listener
skeleton が job container からのアクセスを想定した状態まで詰められていなかったギャップ。 ①
(dogfood 期間) の**前**に埋める必要がある。 埋まるまで `e2e-container` job は `continue-on-error: true` の
advisory 状態のまま (`.github/workflows/blackbox-e2e.yml` の該当節参照)。

対象ギャップ:

- **broker TCP listener の bind address**: 現行は `127.0.0.1:<port>` (loopback 限定)。 daemon container 内
  だけで到達可能で、 sibling network 経由の job container からは到達できない。 `[::]` or compose network
  interface に bind するよう変更、 broker service name (compose service alias `boid-broker`) が sandbox
  side から DNS resolvable な形にする。
- **broker への per-job client cert 発行 + delivery**: dockerproxy と同じ per-job 短命 client cert パターン
  (PR6 で dockerproxy 側は完成、 broker 側は未実装)。 job launch 時に `mtls.CA.IssueShortLivedClientCert`
  で発行、 job container 内に materialize (現行の dockerproxy の TLS material 経路を再利用可能)、
  `BOID_BROKER_TLS_CERT`/`BOID_BROKER_TLS_KEY` env で配送。 UNIX socket 経路の broker client (現行 userns
  経路) は無改変。
- **broker client (sandbox 内)** が backend 別に UNIX/TCP を選ぶ: 現行の `internal/broker/client.go`
  (or 相当) は UNIX socket 決め打ち。 container backend 選択時は TCP + mTLS を使う分岐を追加。
- **container e2e で「job → broker → daemon の RPC が実際に往復する」ことを pin**: 現状 `e2e-container`
  job は sibling 疎通 3 要件のみで、 broker RPC 経路の e2e カバレッジは無い。 gap 埋め後に `boid task update
  --payload-patch` が job → broker → daemon で到達することを scenario で pin。

**参考**: gateway TLS 側の同種 gap は PR9 の commit 577f9a8 で `mtls.CA.ServerOnlyTLSConfig` (server-only、
per-job token で application-layer authorization) + gateway CA PEM の sandbox 伝搬で解決済み。 broker 側も
同じ pattern が使える (broker は per-job token を既に持つ = `BOID_BROKER_TOKEN` env)。 ただし broker は
mTLS の client cert 認証を保ちたい場合もあり、 (a) gateway と同じく server-only に降格 or (b) per-job
client cert を発行する、 の 2 択は着手時に判断。

**前提条件**: これが埋まるまで `sandbox.backend: container` の実 opt-in を production で有効化してはならない
(container job が broker RPC できない → payload_patch 経路が壊れる)。 `docs/plans/phase6-container-backend.md`
§PR7 の「cutover gate」 (container e2e green + rollback rehearsal) も、 実質的にはこの gap 完了までは満たされない。

## ① dogfood 期間の並走

- container backend で 2–4 週のホスト実運用 (nose + 同僚)。stability metrics (dispatch 失敗率・reap
  ログの異常検出頻度・sibling 疎通の実障害有無) を収集する。指標の具体的な収集方法・閾値は着手時に決める
  (本 doc は「安定確認してから進む」というゲートの存在のみを規定する)。
- **PR9 の e2e-container job が CI で継続的に green であること**を①の前提条件とする — CI が落ちている
  状態で dogfood 期間を開始しない。
- 期間中に host 旧デプロイへの rollback (`boid reap` + host daemon 起動) を実際に一度リハーサルする
  (deploy-level rollback 契約が机上の空論でないことを確認する — [phase6-container-backend.md](phase6-container-backend.md)
  の「移行中の安全網」節が要求する rollback 契約の実地検証)。

## ② userns backend 撤去

撤去対象:

- `internal/dispatcher/userns_backend.go` (`usernsBackend` 型と `newUsernsBackend`)。
- `internal/dispatcher/runtime_local_linux.go` (`LocalRuntime` とその関連ファイル: `runtime_subscriber_export.go`
  など userns 専用の attach/resize/signal 実装)。
- `internal/dispatcher/preparer.go` の `SandboxPreparer` interface とその実装 (`sandbox_preparer.go`)。
- `internal/sandbox/runner/runner_linux.go` (clone(NEWUSER)+uid_map / pivot_root / mount syscall / nft / pasta)
  と `internal/sandbox/plan.go` (`BuildPlan` — base rbind + nft drop + DNS stub)。
- `Runner.sandboxBackend()` のデフォルト分岐 (`newUsernsBackend(...)` 呼び出し) — `r.Backend` が常に
  container backend を指すよう `internal/server/wire.go` を書き換え、config `sandbox.backend: userns` の
  実体を削除する (④ で config option 自体も畳む)。
- `docker-proxy-*` / `git-gateway-*` など `requires-sandbox` marker を持つ e2e scenario 群 —
  container backend 相当のシナリオが `e2e-container` job 側に無いものは、撤去前に移植する
  (userns 固有の attach/resize/signal 意味論を検証しているシナリオは、container backend の同等
  カバレッジが無い限り撤去してはならない)。

**前提条件**: ①のバーンイン完了 + `e2e-container` job のカバレッジが `requires-sandbox` シナリオ群と
機能的に同等であること (attach ストリーム・resize 3 経路・agent-stop signal・reap-before-reopen の
container backend 版の e2e が揃っている)。PR9 時点の `e2e-container` job は sibling 疎通 3 要件のみを
検証しており、この同等性はまだ無い — ②着手前に埋める。

## ③ host daemon 起動経路撤去

撤去対象:

- `cmd/start.go` の `runDaemonParent` (bare 二重 fork 起動) と `shouldRunForeground` の
  「フラグ無し = 二重 fork」分岐。`boid start` は常に foreground 実行 (compose 起動が唯一のデプロイ経路)
  になる。
- `printBareStartDeprecationNotice` (PR9 で追加した通知) はこの PR で不要になる (通知対象の経路自体が
  消えるため) — 削除する。
- host 旧デプロイの rollback 契約 (`boid reap` を「deploy-level reaper」として使う設計) — ②の撤去後は
  rollback 先の userns backend 自体が存在しないため、rollback 契約は「container イメージの前バージョンに
  再デプロイ」に置き換わる (通常のコンテナデプロイのロールバックと同じ形になる)。

**前提条件**: ②の撤去 + バーンイン完了。②が終わっていない段階で③に進むと、rollback 先の userns backend
が中途半端に壊れた状態で host daemon 起動経路だけ残る最悪の組み合わせになる。

## ④ config `sandbox.backend` option fold

撤去対象:

- `internal/config/config.go` の `SandboxConfig.Backend` / `SandboxBackendKind` (`userns`/`container` の
  2 値 enum) — container 一択になるため option 自体が意味を失う。config.yaml の `sandbox.backend` キーは
  黙って無視するのではなく、**明示的に「このキーは撤去された」という parse エラーを返す**か、**default
  化して残す** (後方互換で既存 config.yaml が壊れないようにする) かは着手時に決める — 少なくとも
  「設定したつもりが黙って無視される」は避ける。
- `internal/dispatcher/container_backend.go` の `IsContainerBackend` (「container backend かどうか」の
  判定自体が常に true になるため、判定コードは残しても呼び出し側の分岐は削れる) — 呼び出し箇所
  (`internal/server/server.go` の `usingContainerBackend`、`internal/server/wire.go` の
  `gatewayBindHost`/`gatewayURLFor` など) を精査し、「container 前提」の分岐に統合する。

**前提条件**: ③の撤去 + バーンイン完了。

---

## 撤去計画に含まれないもの

- **k8s backend / 別ホスト構成** ([container-based-boid.md](container-based-boid.md) の移行戦略ステップ 7) —
  この doc の対象外。userns backend 撤去後の container backend を前提に、別途計画する。
- **DB (SQLite → Postgres/PVC)** — 同上、チーム共有の論点として別途。

---

## 関連ドキュメント

- [phase6-container-backend.md](phase6-container-backend.md) — Phase 6 本体、この doc が撤去対象とする
  userns backend / host daemon 起動経路の実装元。
- [container-based-boid.md](container-based-boid.md) — 移行戦略全体 (①–⑥ 完了、⑦⑧ は Phase 7)。
