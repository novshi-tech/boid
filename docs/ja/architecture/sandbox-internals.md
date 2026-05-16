# サンドボックス内部実装

`boid` のサンドボックスがどう組まれているか、 hook を 1 つ動かすときに何が起きているかを記したページです。 [アーキテクチャ概要](overview.md) の sandbox 節を、ファイルとシステムコールの粒度で掘り下げています。

主な読者は `internal/sandbox/` に手を入れる contributor、サンドボックス絡みの不具合を追っている人、あるいは「なぜホームディレクトリが見えないのか」 を最後まで知りたい人です。

## ねらい

サンドボックスは 4 つの境界をまとめて作ります。

1. **ファイルシステム** — 書き込み可能な領域を worktree (もしくはプロジェクトルート) に絞る
2. **ネットワーク** — kit が宣言したドメインしか出ていけない
3. **ユーザ ID** — ホストの root には触れない (rootless)
4. **コマンド** — host で動かすコマンドは kit の `host_commands` で宣言された分だけ通る

これら全てを Linux の標準機構 (mount namespace / user namespace / chroot / pasta / nftables) で組み合わせます。 Docker のような追加ランタイムは要りません。

## 起動の全体像

`boid` daemon が hook を 1 つ起動するとき、 `internal/sandbox.Prepare` が 3 枚のシェルスクリプトを `/tmp/boid-<job-id>-{outer,setup,inner}.sh` に書き出します。

```
+-------------------------------------------------------------+
| outer.sh  (host で実行)                                     |
|   pasta (network namespace) で囲んで                        |
|     unshare --mount  ------+                                |
+----------------------------|--------------------------------+
                             ▼
+-------------------------------------------------------------+
| setup.sh  (mount namespace 内、 host fs はまだ全部見えている)|
|   bind mount で $ROOT に worktree や kit ファイルを並べ      |
|   nftables で外向きパケットを drop (proxy 経由のみ許可)      |
|   exec unshare --user --map-user --root=$ROOT --            |
|     bash /tmp/inner.sh   ----+                              |
+------------------------------|------------------------------+
                               ▼
+-------------------------------------------------------------+
| inner.sh  ($ROOT 配下しか見えない rootless な環境)          |
|   env を設定し、 handler スクリプトを実行                    |
+-------------------------------------------------------------+
```

実装は [`internal/sandbox/script.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/script.go) と [`internal/sandbox/render.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/render.go) に集まっています。

### 1. `outer.sh`

```bash
pasta --config-net \
    -a 10.0.2.0 -n 24 -g 10.0.2.2 \
    --dns-forward 10.0.2.3 \
    -t none -u none \
    -- unshare --mount -- bash /tmp/boid-<id>-setup.sh
```

`pasta` はユーザモードで動くネットワーク namespace ラッパで、サンドボックス内のプロセスから見たネットワークを完全に独立させます。 ホストの NIC 経由ではなく、 pasta が提供するゲートウェイ (`10.0.2.2`) と DNS フォワーダ (`10.0.2.3`) が経路です。

その内側で `unshare --mount` を発行し、 mount namespace を切ります。これで以降の bind mount は host の他プロセスには見えません。

### 2. `setup.sh`

`unshare --mount` 直後の状態では、ファイルシステムはまだ host のものがそのまま見えています。 `setup.sh` がここで `$ROOT` (例: `/tmp/boid-root-XXXXXX`) を新しいルートとして組み立てます。

主要ステップ:

- **bind mount** — kit の `additional_bindings`、 worktree、 `/usr` や `/lib` 等のシステムディレクトリを `$ROOT` 配下に bind / rbind マウント。これがサンドボックス内から見えるファイルセットを決めます
- **ファイル書き出し** — kit / job 固有の設定ファイルを `$ROOT` 配下に置く
- **nftables ルール** — proxy ポート以外への外向きパケットを drop。 これで pasta 越しに出ていくときに、 proxy を使う場合のみ通す
- **シンボリックリンク** — `boid` shim を `/opt/boid/bin/<command>` のような形でリンクし、サンドボックス内から `boid` バイナリを呼べるようにする

最後に `setup.sh` は次のように `unshare` を再発行して、組み立て済みの `$ROOT` を新しいルートにしてから `inner.sh` を起動します。

```bash
exec unshare --user --map-user=1000 --map-group=1000 --root="$ROOT" -- /bin/bash /tmp/inner.sh
```

- `--user` で user namespace を切る (rootless 化)
- `--map-user=1000 --map-group=1000` で sandbox 内の UID/GID は 1000:1000 にマップ
- `--root=$ROOT` で `$ROOT` を sandbox プロセスのルートディレクトリに

これにより sandbox 内のプロセスからは:

- ホームディレクトリ・SSH 鍵・他プロジェクトは存在自体が見えない (`$ROOT` 配下に bind しなければそもそもパスが解決しない)
- 自分は UID 1000 として動いており、ホストの root へのエスカレーションパスはない

### 3. `inner.sh`

ここからは sandbox 内です。 環境変数 (`BOID_TASK_ID` 等の kit / behavior が宣言したもの) を `export` してから、 stdin に渡された TaskJSON を handler の argv に流し込みます。 hook の終了コードはそのまま `exec` で置き換える形で返り、 `setup.sh` を経て `outer.sh` まで伝播します。

handler のプロトコル詳細は [Handler スクリプトプロトコル](../reference/handler-contract.md)。

## ネットワーク制御

ネットワーク境界は 2 段構えです。

### ① pasta (ネットワーク namespace)

pasta はユーザ権限で動くツールで、サンドボックス内に独立したネットワーク namespace を提供します。 ホストの物理 NIC は見えず、外向きの通信は pasta が host 側へリレーします。

### ② nftables による drop ルール

`setup.sh` 内で nftables ルールを書き、 proxy ポート以外への外向きパケットを drop します。 結果として:

- HTTP/HTTPS は環境変数 `http_proxy` / `https_proxy` で `10.0.2.2:<port>` を指定して proxy 経由でしか出られない
- proxy は kit が `kit.yaml` の許可ドメインに該当するホストだけ中継する
- 直接の TCP/UDP は遮断される

proxy の実装は [`internal/sandbox/proxy.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/proxy.go) で、 daemon の goroutine として動きます。

## host commands と broker

サンドボックスから host のコマンドを呼ぶには、 `boid` shim と broker のペアが要ります。

```
sandbox 内: boid <subcommand>     (shim バイナリ)
              |
              | UNIX socket (host 上)
              v
host: boid daemon の broker (internal/sandbox.Broker)
              |
              | コマンドポリシーを評価
              v
host: 許可されたコマンドを実際に exec
```

shim は sandbox 起動時に bind mount で sandbox 内に持ち込まれます (実体は `internal/sandbox/boid_shim.go` でビルドされる薄いバイナリ)。 `boid task update` や `boid job done`、 kit が `host_commands` 宣言した `gh` / `git push` 等は、すべてこの経路で host へ流れます。

broker は `internal/sandbox/broker.go` にあり、次の責務を持ちます:

- shim からの要求を UNIX socket で受ける
- リクエストにくっついている **トークン** を見て、どの job が呼んでいるかを特定する
- そのジョブが許可されたコマンド・サブコマンド・引数パターンに合致するかを `policy.go` の `CheckPolicy` で判定する
- 許可されていれば host 側で実際に exec し、 stdout / stderr / 終了コードを shim に返す

トークンは sandbox 起動時に発行され、 sandbox 内の環境変数 `BOID_BROKER_TOKEN` 等で受け渡されます。 sandbox の外からはトークンを知ることができないため、たとえ broker socket のパスが漏れても、別 job のコマンドを許可させることはできません。

## 後片付け

`setup.sh` の冒頭には EXIT トラップが仕掛けられています。

```bash
cleanup() {
    case "$ROOT" in
        /tmp/boid-root-*) ;;
        *) echo "FATAL: ROOT=$ROOT is not a boid tmpdir, refusing cleanup" >&2; return 1 ;;
    esac
    umount -R "$ROOT" 2>/dev/null || true
    if findmnt --noheadings --output TARGET 2>/dev/null | awk -v r="$ROOT" '$0 == r || index($0, r "/") == 1 { found=1 } END { exit !found }'; then
        echo "WARNING: mounts still active under $ROOT, skipping rm" >&2
    else
        rm -rf "$ROOT"
    fi
    rm -f <outer.sh> <setup.sh> <inner.sh>
}
trap cleanup EXIT
```

ポイントは 2 つの安全弁です。

1. **`$ROOT` が `/tmp/boid-root-*` プレフィックスでなければ削除しない**。 環境変数の取り違えやバグで意図しないパスが入っても、 `rm -rf` が host を壊さないようにする
2. **`$ROOT` 配下に live なマウントが残っていたら削除しない**。 bind mount が残ったまま `rm -rf` するとマウント先のホストファイルが消えるので、 マウントを全部 unmount できなかった場合は警告ログを残して削除をスキップする

過去にこの 2 段ガードが破られた事例があり (memory: "feedback: bind_rm_traverses_source")、現在の実装は own ns / cross ns / chroot holder の 3 経路すべてで安全側に倒すようになっています。

## サンドボックス内から呼べる boid builtin 一覧

サンドボックス内のハンドラ (hook / gate / exec) は `boid` と `git` の 2 つの builtin を呼ぶことができます。
いずれも自動的に注入されるため、 `project.yaml` / `kit.yaml` での宣言は不要です。

### boid builtin

role (hook / gate) による分岐はなく、全 role で同じ op セットが許可されます。

| Op (sandbox protocol) | 対応 CLI | 用途 |
|---|---|---|
| `job_done` | `boid job done <id>` | 自 job の終了を daemon に通知する |
| `job_list` | `boid job list --task <id>` | task に紐づく job を列挙する |
| `job_show` | `boid job show <id>` | job の詳細を表示する |
| `job_log` | `boid job log <id>` | job 実行ログを取得する |
| `action_send` | `boid action send` | 手動アクションを発行する |
| `task_create` | `boid task create` | サブ task を作成する |
| `task_get` | `boid task show <id> --field <path>` | task の 1 フィールドを dotted JSON path で取得する |
| `task_update` | `boid task update <id>` | task のフィールドを更新する |
| `task_import` | `boid task import` | task を一括 import する |
| `task.reopen` | `boid task reopen <id>` | done の task を executing に戻す |
| `task_list` | `boid task list` | workspace 内の task を列挙する |
| `task_notify` | `boid task notify <id>` | 通知または Q&A (`--ask`) を送信する |
| `task_answer` | `boid task answer` | awaiting → executing に遷移させる |
| `task_delete` | `boid task delete <id>` | task を削除する |

> **注記:** `task.reopen` だけが歴史的事情で `.` 区切りになっています。 他の op は `_` 区切りです。

### git builtin

全 role で同じ op セットが許可されます。

| Op | 対応 CLI | 用途 |
|---|---|---|
| `fetch` | `git fetch ...` | リモートから取得する |
| `push` | `git push ...` | リモートへ反映する |
| `push_delete` | `git push origin --delete <branch>` | リモートブランチを削除する |

### 設計上の注記

- **role 分岐なし** — `boid` / `git` ポリシーは `_ Role` で受け、全 role に同一 op セットを与えます。
  新しい builtin で role 固有の制限が必要になった場合のみ、 `policyFor` 内に `switch` を追加してください。
- **情報源** — `internal/orchestrator/policy.go` の `boidPolicy` / `gitPolicy` 関数が source of truth です。
- **サンドボックス側 enum 定義** — `internal/sandbox/protocol.go`
- **workspace / project 越えのアクセス** は broker (`internal/sandbox/broker.go` `handleBoidBuiltin`) が
  `entry.Context.AllowsProject(...)` 等で拒否します。 上記 op セットはこのチェックをバイパスしません。

## 関連ドキュメント

- [アーキテクチャ概要](overview.md) — sandbox レイヤの位置づけ
- [概念 / サンドボックス](../guide/concepts.md#サンドボックス-sandbox) — ユーザ視点での意味
- [Handler スクリプトプロトコル](../reference/handler-contract.md) — sandbox 内 handler の I/O
- [`project.yaml` リファレンス](../reference/project-yaml.md) — `host_commands` / `additional_bindings` / `env` の宣言
