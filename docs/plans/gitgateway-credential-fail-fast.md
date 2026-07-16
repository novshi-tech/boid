# git-gateway credential 解決失敗の fail-fast 化 + sandbox git prompt 抑止

ステータス: **実装完了** (2026-07-16、PR #764/#765/#766 全 landed)
作成日: 2026-07-16
親ドキュメント: [git-gateway-cutover.md](git-gateway-cutover.md) — post-cutover 改善候補として (§1 workspace-scoped PAT namespace 導入の運用副作用の根治)

## Landed PR

- **PR-A** #764: `refactor: gitgateway.CredentialProvider に Resolve メソッド追加` — `Inject` を `Resolve` + `SetBasicAuth` に分解、fail-fast の足場
- **PR-C** #765: `feat: sandbox job env に GIT_TERMINAL_PROMPT=0 / GIT_ASKPASS 注入` — defense-in-depth
- **PR-B** #766: `fix: gitgateway で credential 解決失敗時 fail-fast (502) 化` — 主対策
  - PR 内 2 commit 目 `6c56517` で **KnowsHost gate 追加** (e2e regression 修正)

## 実装時の学び (計画からの差分)

**KnowsHost gate の必要性は plan doc に書けなかった落とし穴**。 PR-B 初回 push で e2e 30+ scenario が 502 で大量 fail した。 真因: e2e は `httptest.Server` の動的 port を fake upstream に使うため、 `config.Gateway.HostConfigs()` に事前登録不能 → pre-check の Resolve が `no forge configured for host` err を返し全部 502 で殺してた。

対策: `CredentialProvider.KnowsHost(host) bool` を追加、 `ServeHTTP` の pre-check を **host が config に登録済みの時のみ** 実行するよう gate。 未登録 host は pre-PR-B と等価な fail-open + notify を継続 (test upstream + unregistered forge の 2 shape を保持)。 本命の hang 経路 (known host + secret miss、 `BB_TOKEN` 未設定など) は依然 fail-fast 502 で捕獲。

ローカル E2E 全 44 scenario green で検証してから push した ([[git-gateway-cutover-ci-false-positive-lesson]] 教訓、 CI 前に自前ループで確認)。

## 残タスク

**なし** (P0 完了)。 副次的な P1 対応は本 doc §リスク・非スコープ 参照:

- `ubs` / `bm-next` workspace の PAT 移行漏れは nose 手動対応 (私は書き込まない)
- `boid check` の workspace × forge secret 存在チェックは別 issue (優先度低)

---

## 目的

git-gateway が **credential 解決に失敗した際、無認証で upstream に forward してしまい、
upstream (Bitbucket 等) が返す 401 + `WWW-Authenticate: Basic` に反応して sandbox 内
git が credential prompt を出し、TUI 全体が hang する**設計欠陥を根治する。

一次証拠 (2026-07-15 実測、task `f8e73408-aed6-40f2-acd2-173feebde6c2`):

- `~/.local/state/boid/boid.log`:
  ```
  WARN gitgateway: credential injection failed; forwarding without auth
    host=bitbucket.org err=...
    namespace "khi": secret "khi"/"BB_TOKEN": sql: no rows in result set
  WARN gitgateway: upstream rejected credentials (401); token may be expired or revoked
  ```
- sandbox 内 TUI:
  ```
  Username for 'http://10.0.2.2:37523':
  ```
  → Ctrl-C しないと解けない。

回避策 (`boid secret set -n <workspace> BB_TOKEN`) は既に確立済みで、
BB_TOKEN 移行前後で hang → 完走を検証済み。ただしこれは **新規 workspace 作成時 /
PAT rotate 時に必ず再発する**ので、コード側の根治が必要。

---

## 前提となる決定事項

- **主対策は gateway 側の fail-fast (HTTP 502)**
  (2026-07-15 nose 合意、[[gitgateway-credential-fail-hangs-sandbox]] §根治案)
  - 理由: git は 401 + `WWW-Authenticate` に反応して prompt する。
    502 (Bad Gateway) なら fatal 扱いで prompt しない。gateway 自身の設定不備で
    upstream に届けられなかった、という意味論に一番合う。
- **副対策は sandbox 側 env の defense-in-depth**
  (`GIT_TERMINAL_PROMPT=0` + `GIT_ASKPASS=/bin/false`)
  - 主対策で hang は消えるはずだが、gateway 外経路の 401 (upstream 側 PAT 失効、
    設定漏れで直リンク origin が残ってるケース等) でも hang しないように保険を張る。
- **PR 分割は 3 本 (A/B/C)**
  - PR-A: `CredentialProvider.Inject` の `Resolve` 分解 (refactor、挙動不変)
  - PR-B: `ServeHTTP` の fail-fast (502) 実装 + unit test
  - PR-C: sandbox job env の `GIT_TERMINAL_PROMPT=0` 注入 + test
  - PR-B と PR-C は独立、順不同 OK (defense-in-depth の性質)
- **PR-A は refactor 単独に絞る**
  - Inject の呼び出し側は現行 test (`credentials_test.go`) + 本番の `server.go`
    Rewrite 経路 の 2 か所のみ、Inject を残しつつ内部で Resolve を呼ぶ薄い wrapper に
    することで既存 test の書き換え量を最小化できる。
- **既存の運用回避策は継続適用**
  - `boid secret set -n <ws> <KEY>` の workspace 単位 PAT 移行は nose 手動で継続
    (私の判断で書き込まない、[[gitgateway-credential-fail-hangs-sandbox]] §How to apply)。
  - `boid check` の workspace × forge secret 存在チェックは別 issue (優先度低)。

---

## Phase A: `CredentialProvider.Inject` → `Resolve` 分解 (PR-A)

### 動機

現行の `Inject(req, host, namespace)` は「secret 解決 + username 決定 + `SetBasicAuth`
呼び出し」を 1 つの関数にまとめている (`internal/gitgateway/credentials.go:130-151`)。
PR-B で `ServeHTTP` の Authorize 直後で解決失敗を検知したいので、
**「解決」と「注入」を分離**する必要がある。

### 変更内容

**`internal/gitgateway/credentials.go`**:

```go
// Resolve looks up the forge config for host, resolves its secret in the
// given namespace, and returns the Basic-auth username / token pair. It
// returns an error (leaving both strings empty) if host has no configured
// forge, no resolver is set, or the secret can't be resolved. Callers use
// this either to fail fast before proxying (Server.ServeHTTP) or to
// SetBasicAuth on an outbound request (Inject below).
func (c *CredentialProvider) Resolve(host, namespace string) (username, token string, err error) {
    if c == nil {
        return "", "", fmt.Errorf("gitgateway: no credential provider configured")
    }
    cfg, ok := c.hosts[host]
    if !ok {
        return "", "", fmt.Errorf("gitgateway: no forge configured for host %q", host)
    }
    if c.resolver == nil {
        return "", "", fmt.Errorf("gitgateway: no secret resolver configured for host %q", host)
    }
    tok, err := c.resolver(namespace, cfg.SecretKey)
    if err != nil {
        return "", "", fmt.Errorf("gitgateway: resolve secret %q for host %q (namespace %q): %w", cfg.SecretKey, host, namespace, err)
    }
    user, err := usernameForForge(cfg.Forge)
    if err != nil {
        return "", "", err
    }
    return user, tok, nil
}

// Inject stays as a thin wrapper for backward-compatibility with the
// Rewrite callback path (it still gets called after the pre-check succeeds,
// so the second call is a re-resolve — cheap enough for now).
func (c *CredentialProvider) Inject(req *http.Request, host, namespace string) error {
    user, tok, err := c.Resolve(host, namespace)
    if err != nil {
        return err
    }
    req.SetBasicAuth(user, tok)
    return nil
}
```

**注**: PR-B で `Server.ServeHTTP` が `Resolve` を呼ぶ + Rewrite が `Inject` を呼ぶ
という 2 段呼び出しになる。resolver は SecretStore の DB lookup 1 発なので
実害は小さいが、PR-B で Rewrite 側の再解決を廃止して context 経由で username/token を
持ち回す設計 (下記 Phase B §オプション) にすると 1 発に戻せる。まず PR-A では
最小変更に絞り、Phase B の設計判断は PR-B のレビューで詰める。

### テスト

**`internal/gitgateway/credentials_test.go`**:

- 既存の 6 個の `cp.Inject(...)` 呼び出しは触らない (Inject の外側挙動は不変)
- 新規に `TestResolve_*` を数本追加:
  - success case: 正しい username + token 返却
  - no forge configured for host → err
  - no resolver → err
  - resolver err → err (msg に host + namespace + secret key を含む)

### 進行フロー

1. `Resolve` 実装 + `Inject` を wrapper に薄化
2. `credentials_test.go` に `TestResolve_*` 追加
3. `go build ./... && go test ./internal/gitgateway/... && go vet ./...`
4. PR 作成 (title: `refactor: gitgateway.CredentialProvider に Resolve メソッド追加 (fail-fast PR-A)`)
5. CI green 確認、[[git-gateway-cutover-ci-false-positive-lesson]] の教訓を守る
6. merge (nose 権限 [[nose-pr-merge-authorization]] 範囲内)

---

## Phase B: `ServeHTTP` fail-fast (PR-B)

### 動機

現行 `ServeHTTP` は Authorize + Lookup が成功したら即 `proxy.ServeHTTP` を呼び、
credential 解決失敗は `Rewrite` callback 内で握りつぶして forward してしまう
(`internal/gitgateway/server.go:76-81`)。credential 解決を `ServeHTTP` 側に前倒し、
失敗時は upstream に到達させず 502 を返す。

### 変更内容

**`internal/gitgateway/server.go`**:

```go
// ServeHTTP body、entry lookup 直後 (namespace := entry.Namespace の直後)
// に追加:
if s.credentials != nil {
    _, _, err := s.credentials.Resolve(rt.host, namespace)
    if err != nil {
        slog.Warn("gitgateway: credential resolution failed; refusing to forward",
            "host", rt.host, "namespace", namespace, "err", err)
        s.notifier.NotifyCredentialError(rt.host, repo, err)
        http.Error(w,
            "bad gateway: git gateway credential resolution failed for host "+
                rt.host+" (namespace "+namespace+"): "+err.Error(),
            http.StatusBadGateway)
        return
    }
}
```

**注**:
- `Rewrite` 内の `Inject` 呼び出しは残す (2 段解決)。PR-B の目的は
  「fail-fast による hang 根絶」であり、resolve 呼び出し回数の最適化は別問題。
- `s.credentials.Configured()` の既存 503 チェック (`server.go:158-161`) は
  独立の "resolver 自体が未設定" ガードとして残す。今回の `Resolve` err は
  そこを通過した後に個別 host/namespace で失敗するケースを潰す。
- error message は host + namespace + wrapped err (secret key を含む) を返す。
  boid.log の WARN と併せて nose が原因を特定できる情報を出す。

### テスト

**`internal/gitgateway/server_test.go`**:

新規追加 (既存 `TestServeHTTP_CredentialInjectionFailure_*` 系があれば拡張):

```go
func TestServeHTTP_ResolverError_FailsWith502(t *testing.T) {
    // resolver が特定 key で err を返す fake
    resolver := func(ns, key string) (string, error) {
        return "", errors.New("no rows in result set")
    }
    creds := NewCredentialProvider([]HostForgeConfig{
        {Host: "bitbucket.org", Forge: ForgeBitbucket, SecretKey: "BB_TOKEN"},
    }, resolver)

    // fake upstream の hit counter で「到達してない」を assert
    upstreamHits := 0
    upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        upstreamHits++
        w.WriteHeader(http.StatusOK)
    }))
    defer upstream.Close()

    // ... registry setup + Server ...

    resp, _ := http.Get(gwSrv.URL + "/j/" + token + "/bitbucket.org/owner/repo.git/info/refs?service=git-upload-pack")
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusBadGateway {
        t.Fatalf("status = %d, want 502", resp.StatusCode)
    }
    if upstreamHits != 0 {
        t.Fatalf("upstream got %d hits, want 0 (fail-fast should skip forward)", upstreamHits)
    }
    body, _ := io.ReadAll(resp.Body)
    if !strings.Contains(string(body), "bitbucket.org") {
        t.Errorf("response body missing host: %q", body)
    }
    if !strings.Contains(string(body), "BB_TOKEN") {
        t.Errorf("response body missing secret key hint: %q", body)
    }
}
```

- notifier 呼び出しの assert は `mockNotifier` (既存 test で使ってれば流用) で
  `NotifyCredentialError` が 1 回発火することを確認。

### 進行フロー

1. `ServeHTTP` に Resolve pre-check 追加
2. `server_test.go` に fail-fast test 追加
3. `go build ./... && go test ./internal/gitgateway/... -race && go vet ./...`
4. PR 作成 (title: `fix: gitgateway で credential 解決失敗時 fail-fast (502) 化 (PR-B)`)
5. CI green 確認 + boid.log 経路の手動再現 (nose 手元 daemon で `khi` workspace BB_TOKEN を一時的に別名にリネームして 502 が出るか確認、テスト後戻す)
6. merge

### オプション (レビュー次第で PR-B に含める / 別 PR に分ける)

Rewrite 側の再解決を廃止して context 経由で username/token を持ち回す:

```go
// ServeHTTP で Resolve 後、user/token を routeInfo に格納 or 別 context key
ctx := context.WithValue(r.Context(), routeInfoKey{}, routeInfo{
    host: rt.host, repo: repo, namespace: namespace,
    username: user, token: tok,
})
// Rewrite 側は SetBasicAuth のみ
pr.Out.SetBasicAuth(info.username, info.token)
```

利点: DB lookup 1 発 (現状の Rewrite での再解決を廃止)。
欠点: routeInfo に secret を載せる = context をログ出しする際の redaction 責任が生じる。
現行の DB lookup は cheap なので、まず PR-B は再解決許容 → 別 PR で最適化、が保守的。

---

## Phase C: sandbox job env に prompt 抑止 (PR-C)

### 動機

主対策で gateway 経由の hang は消えるが、gateway 外経路の git 401 でも hang しないよう
sandbox job env に defense-in-depth を張る:

- `GIT_TERMINAL_PROMPT=0` — credential prompt を出さない
- `GIT_ASKPASS=/bin/false` — askpass helper 呼び出しも即失敗

`internal/gitgateway/integration_test.go:63` の test 用 gitTestEnv() が既に
`GIT_TERMINAL_PROMPT=0` を採用しており、実装上の妥当性は確認済み。

### 変更内容

**`internal/dispatcher/sandbox_builder.go`**:

`BuildSandboxSpec` の env 構築ブロック (`sandbox_builder.go:145-181` 付近、
`env["HOME"] = homeDir` の直後) に追加:

```go
// Defense-in-depth: sandbox 内 git が credential prompt を出して TUI が hang
// するのを防ぐ。主経路 (git gateway) は Server.ServeHTTP の fail-fast で
// upstream の 401 に到達しないが、gateway 外の upstream 直リンクや upstream 側
// PAT 失効ケースでも同様に hang しないよう保険を張る。
// spec.Env で明示的に上書きされていれば尊重する。
if _, ok := env["GIT_TERMINAL_PROMPT"]; !ok {
    env["GIT_TERMINAL_PROMPT"] = "0"
}
if _, ok := env["GIT_ASKPASS"]; !ok {
    env["GIT_ASKPASS"] = "/bin/false"
}
```

`env` は `cloneStringMap(spec.Env)` 起源なので、spec.Env に指定があれば
そのまま残る (`ok == true` 分岐で skip)。

### テスト

**`internal/dispatcher/sandbox_builder_test.go`**:

```go
func TestBuildSandboxSpec_InjectsGitPromptSuppression(t *testing.T) {
    spec := &orchestrator.JobSpec{ /* minimal */ }
    got, err := BuildSandboxSpec(spec, SandboxRuntimeInfo{})
    if err != nil {
        t.Fatal(err)
    }
    if got.Env["GIT_TERMINAL_PROMPT"] != "0" {
        t.Errorf("GIT_TERMINAL_PROMPT = %q, want 0", got.Env["GIT_TERMINAL_PROMPT"])
    }
    if got.Env["GIT_ASKPASS"] != "/bin/false" {
        t.Errorf("GIT_ASKPASS = %q, want /bin/false", got.Env["GIT_ASKPASS"])
    }
}

func TestBuildSandboxSpec_RespectsExplicitGitPromptOverride(t *testing.T) {
    spec := &orchestrator.JobSpec{
        Env: map[string]string{
            "GIT_TERMINAL_PROMPT": "1",     // user override
            "GIT_ASKPASS":         "/usr/local/bin/my-askpass",
        },
    }
    got, _ := BuildSandboxSpec(spec, SandboxRuntimeInfo{})
    if got.Env["GIT_TERMINAL_PROMPT"] != "1" {
        t.Errorf("GIT_TERMINAL_PROMPT overridden: got %q, want 1 (spec.Env should win)", got.Env["GIT_TERMINAL_PROMPT"])
    }
    if got.Env["GIT_ASKPASS"] != "/usr/local/bin/my-askpass" {
        t.Errorf("GIT_ASKPASS overridden: got %q", got.Env["GIT_ASKPASS"])
    }
}
```

### 進行フロー

1. `sandbox_builder.go` に env 注入追加
2. `sandbox_builder_test.go` に 2 本追加
3. `go build ./... && go test ./internal/dispatcher/... && go vet ./...`
4. PR 作成 (title: `feat: sandbox job env に GIT_TERMINAL_PROMPT=0 / GIT_ASKPASS 注入 (fail-fast PR-C)`)
5. CI green 確認
6. merge

### 補足: e2e 追加検討 (優先度: 低)

git-gateway-* 系 scenario ([[next-session-script-hook-removal]] 完了時に整備済) に
「secret 未設定 → gateway が 502 + sandbox git が非対話失敗」の期待動作を
記録する scenario を追加する案がある。ただし PR-B の unit test で機能は
検証できる + integration_test の real git + git-http-backend 経路も 502 挙動を
確認できるので、e2e はレビュー次第。まず A/B/C 3 本 land 後に判断。

---

## テスト戦略まとめ

- **PR-A**: `internal/gitgateway/credentials_test.go` に `TestResolve_*` 追加。既存 `TestInject_*` は不変
- **PR-B**: `internal/gitgateway/server_test.go` に fail-fast unit test (fake upstream hit=0 assert)
- **PR-C**: `internal/dispatcher/sandbox_builder_test.go` に env 注入 + override 尊重の 2 本
- **共通**: `go build ./... && go test -race ./... && go vet ./...`、CI green 確認は
  [[git-gateway-cutover-ci-false-positive-lesson]] の教訓を守り green を鵜呑みにしない
- **手動再現** (PR-B merge 前): nose 手元 daemon で `khi` workspace の BB_TOKEN を
  一時的にリネーム → boid.log で 502 + `bad gateway: git gateway credential resolution failed` を確認 → 戻す

---

## リスク・非スコープ

### 非スコープ

- **`boid check` の workspace × forge secret 存在チェック**: 別 issue、優先度低
- **既存 workspace の PAT 移行**: nose 手動 (私は書き込まない、
  [[gitgateway-credential-fail-hangs-sandbox]] §How to apply)
- **routeInfo の secret 持ち回し最適化** (PR-B §オプション): レビュー次第で
  別 PR に分離

### リスク

- **502 error に対する既存 callers の反応**: 現行 gateway callers は sandbox 内 git
  のみ (PR5 以降)。git は 502 を fatal 扱いで exit non-zero するので、上位の
  clone/fetch/push が失敗として伝播する。これは正しい挙動 (silent hang より
  明示的 fail の方が観測可能)。
- **`GIT_ASKPASS=/bin/false` の副作用**: `/bin/false` は POSIX 標準の exit 1
  コマンド。git は askpass の exit non-zero を prompt 失敗として扱い、
  「could not read Username」等の明確な error で exit する。SSH 経路 (`git@...`)
  には影響しない (GIT_ASKPASS は HTTPS auth 用)。
- **sandbox 内で ssh 経路の git を使ってる箇所**: gateway 経由 clone は http、
  `SSH_ASKPASS` (別変数) は触らないので既存 ssh 経路は無影響。念のため
  PR-C の PR 説明で明記。

---

## 関連 memory

- [[gitgateway-credential-fail-hangs-sandbox]] — 今回の設計 memo (詳細のオリジナル)
- [[next-session-gitgateway-fail-fast]] — 次セッション着手メモ (この plan doc の元)
- [[next-session-git-gateway-peer-clone-check]] — PR #753 workspace-scoped PAT namespace 導入の親記録
- [[container-git-gateway-design]] — git gateway 全体設計 (v0.0.10 landed)
- [[nose-pr-merge-authorization]] — git gateway プロジェクトは merge 権限内
- [[git-gateway-cutover-ci-false-positive-lesson]] — CI green 検証規律
- [[evidence-first-infra-suspicion]] — 一次証拠と推論を分離

---

## 進行順序 (recap)

1. PR-A (Resolve 分解、refactor) → merge
2. PR-B (fail-fast 502) → merge、手動再現で hang 消失確認
3. PR-C (sandbox env 注入) → merge、defense-in-depth 完成
4. 既存 workspace の PAT 移行漏れは nose 手動対応 (`ubs`, `bm-next` の BB/GH)
5. plan doc に「完了」ステータス追記 + memory `next-session-gitgateway-fail-fast` に
   完了マーク移動
