---
name: boid-cli-workspace
description: >
  boid の workspace / project / secret / login をホスト側 (boid CLI を直接叩ける
  マシン上、job サンドボックスの外) から操作する。「boid でプロジェクトを登録して」
  「workspace を作って」「project.yaml を反映して」「secret を設定して」
  「host_commands を workspace に足して」など、boid 自身のワークスペース/
  プロジェクト構成を CLI で操作するときに使用。サンドボックス内の boid-task /
  boid-orchestrate とは別物 — こちらはタスクの外、boid デーモンの外部管理者として
  動く。
---

# boid-cli-workspace — workspace / project 管理 (host 側)

このスキルは **boid job サンドボックスの外**、boid CLI (`boid`) をホストに
インストール済みの環境で使う。前提: `boid` コマンドが PATH にあり、daemon が
起動していること（大半のコマンドは daemon 未起動でも自動起動する。`boid gc`
は daemon の HTTP API を叩く点では他の workspace/project 系コマンドと同じだが、
「gc するためだけに daemon を起動する」を避けるため自動起動だけスキップする —
scope 分類 (remote/local/neutral) とは別軸）。

## 最初にやること

各コマンドの正確なフラグは `boid <command> --help` / `boid <command> <sub> --help`
で確認する。このファイルはコマンド一覧と「`--help` だけでは分からない手順・罠」
をまとめたもので、フラグの網羅表ではない（バージョンで変わりうるフラグを
ハードコードすると陳腐化するため）。より詳しい説明が要る場合は、boid リポジトリ
内で作業しているなら `docs/ja/reference/cli.md` を読む。

## コマンド一覧

### Project

| コマンド | 役割 |
|---|---|
| `boid project add <git-url> --workspace=<name> [--name=<project-name>]` | git remote URL を登録して daemon 管理下に clone |
| `boid project list` | 登録済みプロジェクト一覧 (status: ready / degraded) |
| `boid project show <ref> [--explain]` | プロジェクト詳細。`--explain` でフィールド単位の由来 (project.yaml / workspace default / unset) |
| `boid project remove <ref>` (alias `rm`) | 登録解除 (DB row 削除の唯一の入口。git-URL 登録なら bare repo も削除) |
| `boid project reload` | 全プロジェクトの `project.yaml` を再読込 |
| `boid project fetch <ref>` | bare repo で `git fetch --all` してから `project.yaml` を再読込 |
| `boid project behaviors <ref>` | そのプロジェクトの task_behaviors 一覧 |

### Workspace

| コマンド | 役割 |
|---|---|
| `boid workspace list` | ワークスペース一覧 |
| `boid workspace show <slug>` | 定義 (host_commands/env/capabilities) + 割当済み project |
| `boid workspace create <slug> [--from-file <yaml>]` | 新規作成 (省略時は空) |
| `boid workspace edit <slug> --from-file <yaml>` | 丸ごと置き換え (自動 If-Match) |
| `boid workspace export <slug>\|--all [-o FILE]` | `apiVersion: boid.dev/v1 / kind: Workspace` yaml で書き出し。`--all` が唯一の正式 backup 経路 |
| `boid workspace apply -f FILE [--dry-run]` | export した yaml を適用 (upsert、フィールド単位マージ) |
| `boid workspace assign <project-ref> <workspace-id>` | project を workspace に紐付け |
| `boid workspace clear <project-ref>` | 紐付けを `default` にリセット |
| `boid workspace remove <slug> [--force\|--yes]` (alias `delete`) | 削除 (割当 project は `default` へ再割当、`default` 自体は削除不可) |
| `boid workspace get-init-script <slug> [-o FILE]` | `init.sh` を表示 |
| `boid workspace set-init-script <slug> -f FILE [--force]` | `init.sh` を登録 (上限 128 KiB、空ファイルは削除扱い) |
| `boid workspace edit-init-script <slug> [--force]` | `$EDITOR` で編集して保存 |
| `boid workspace unset-init-script <slug> [--force]` | `init.sh` を削除 |
| `boid workspace services add\|remove\|list <slug> [service...]` | API gateway service の有効化/一覧 |

### Secret / Login

| コマンド | 役割 |
|---|---|
| `boid secret set\|get\|list\|delete <key> [-n NAMESPACE]` | secret の CRUD (鍵は `~/.local/share/boid/secret.key`) |
| `boid secret oauth login <service> [-n NAMESPACE]` | API gateway の OAuth2 サービス初回認証 |
| `boid login` / `boid logout` | リモート daemon profile への認証 (bare-metal ローカル運用では通常不要) |

### Config

| コマンド | 役割 |
|---|---|
| `boid config get\|set <key> [<value>]` | daemon 設定の読み書き (例: `boid config set web.http_addr :9090`) |
| `boid config unset <key>` | 設定を既定値に戻す |
| `boid config apply -f FILE` | まとめて適用 |
| `boid config edit` | `$EDITOR` で編集 |

## 手順・落とし穴

### 新規プロジェクトを登録する

```bash
boid project add git@github.com:org/repo.git --workspace dev
# project.yaml が repo 側にあれば自動で読まれる。無ければ作る (docs/ja/getting-started/02-init-project.md)
```

### `project.yaml` を編集したら反映を忘れずに

boid 自身は project.yaml の変更を自動検知しない。編集後は必ず:

```bash
boid project fetch <ref>   # git-URL 登録の場合: fetch + reload
# または
boid project reload        # 全プロジェクト一括 reload のみ (fetch はしない)
```

### workspace の唯一の正式バックアップ経路

`boid workspace export --all -o backup.yaml` が唯一の正式な backup。DB の生コピーは
復元手段として不十分（daemon が動いてる前提の運用のため）。復元は
`boid workspace apply -f backup.yaml`。

### `export`/`apply` は slug に使えない予約語

**workspace の slug に `export` / `apply` を使わないこと。** HTTP API 側で
workspace 名と同じ階層の静的ルートになっているため、作成はできてしまうが
`boid workspace show/edit/remove` から到達できなくなる。既に作ってしまったら
`boid workspace export <slug>` (これは `?name=` 経由なので通る) → `metadata.name`
を別名にリネーム → `boid workspace apply -f` → `boid workspace remove <旧 slug>
--force` で退避する。

### `set-addr` は daemon 再起動が要る

`boid web set-addr :9090`（web.md 参照）や `boid config set web.http_addr ...`
系は設定を書くだけで、反映には `boid stop && boid start` が要る。

### Kit / `workspace configure` は退役済み

`boid kit init/list/remove`・`boid workspace configure` は Phase 2.5 PR6 で撤去済み。
`host_commands` は「workspace 側は参照名の一覧 (`host_commands: [gh, aws]`) だけ、
実体定義 (`path`/`allow`/`deny`/`env`) は `~/.config/boid/host_commands.yaml`
に daemon 側で集約管理」という二層構造。定義自体は手で `host_commands.yaml` を
編集し、`boid host-commands reload` で反映する ([`boid-cli-daemon`](../boid-cli-daemon/SKILL.md) 参照)。

## 関連スキル

- [`boid-cli-daemon`](../boid-cli-daemon/SKILL.md) — daemon ライフサイクル・Web UI ペアリング・host_commands
- [`boid-cli-task`](../boid-cli-task/SKILL.md) — task/job の作成・追跡
