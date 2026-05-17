# 5. GitHub PR ベースの開発ワークフロー

このページでは、 GitHub PR ベースの開発ワークフローを最初から最後まで通して実行します。 AI エージェントが新しいブランチでコードを書き、 PR を作り、 CI 完了を待ち、最後に `boid` が PR を自動マージするところまでを 1 タスクで動かします。 [4. プロジェクトと拡張パッケージ (kit)](04-projects-and-kits.md) の続きです。

このチュートリアルが終わると、 `boid` の主用途の 1 つである **AI エージェントへの開発タスク委譲** の最小構成を 1 本通せたことになります。

## 前提

- [4. プロジェクトと拡張パッケージ (kit)](04-projects-and-kits.md) の手順で `boid kit install github.com/novshi-tech/boid-kits` 済み
- `claude` CLI がサインイン済みで PATH 上にある
- `gh` CLI が `gh auth login` 済み
- 「捨ててよい」 GitHub リポジトリが 1 つあり、 `git clone` 済み (例: `~/src/github.com/<you>/boid-demo-repo`)
- そのリポジトリで GitHub Actions が CI として何か走るとなお良い (無くても動作確認は可能)

## 役割分担

このチュートリアルで作る構成では、 ワークフローの大部分を **agent への instruction** が担います。 `boid` 本体は worktree の作成と最後の自動マージだけを引き受け、 commit / push / PR 作成 / CI 完了待ち は agent が行います。 CI が落ちた場合の判断 (abort するか reopen で再依頼するか) もハーネスではなく agent / オペレータの責務です。

| 担当 | 役割 |
|---|---|
| `boid` 本体 (project トップ `worktree: true`) | **executor** タスクごとに新ブランチで git worktree を作り、片付ける |
| `claude-code` kit (hook) | `executing` 状態で Claude Code エージェントを起動 |
| `github-cli` kit | サンドボックス内から `gh` を使えるようにする |
| **agent への instruction** | 編集 → commit → push → PR 作成 → CI 待ち、 失敗時は abort |
| `github-auto-merge` kit (gate) | executing → done の exit gate で `gh pr merge` を実行 |

instruction が大半の責務を持つことで、ワークフローの中身 — 何を確認するか、どこで止めるか、何をログに残すか — はプロジェクトごとにテキストで自由に調整できます。

## project.yaml を書く

`~/src/github.com/<you>/boid-demo-repo/.boid/project.yaml` を以下の内容で作成します。

```yaml
id: boid-demo
name: boid demo repo

# Project トップ: executor タスクごとに新ブランチで worktree を作る
worktree: true

kits:
  - github.com/novshi-tech/boid-kits/claude-code
  - github.com/novshi-tech/boid-kits/github-cli

task_behaviors:
  executor:
    name: executor
    kits:
      - github.com/novshi-tech/boid-kits/github-auto-merge
    default_instruction:
      type: execution
      agent: claude-code
      message: |
        task の title と description に書かれた内容を実装してください。
        コミット前にユニットテストの通過を確認すること。

        実装が完了したら以下を順次実行する:
        1. `git add` + `git commit` (worktree の現ブランチ上で)
        2. `git push -u origin HEAD` で current branch を origin に push
        3. 既存 PR の有無を確認:
           PR_URL=$(gh pr list --head "$(git branch --show-current)" \
                      --json url --jq '.[0].url // ""')
           空なら `gh pr create --title "<task title>" --body "<summary>"`
        4. `gh pr checks --watch --fail-fast` で CI 完了まで待機
           (CI の無いリポでは即抜ける)
        5. CI が成功したらそのまま正常終了 (`boid job done` の trap で
           状態機械が executing → done に進める)。
        6. CI が失敗したら `gh run view --log-failed` で原因を確認し、
           修正可能ならコードを修正して 1 から再実行。 修正不可能と判断したら
           `boid task abort <task_id> --code ci_failed --message "<要約>"`
           で task を打ち切る。
```

ポイント:

- **project トップの `worktree: true`** で executor タスクごとに別ブランチ・別ディレクトリにする。 このフラグは behavior 単位ではなく project トップに置き、 同じ project の全 executor タスクに適用される。 supervisor タスクはこのフラグに関わらず常に readonly で project root 上を走る
- **トップレベルの `kits:`** で project 全体で使う kit を読み込む (`claude-code` と `github-cli`)
- **`task_behaviors.executor`** が今回の主役。 kit リストには `github-auto-merge` だけ。 PR 作成と CI 確認は instruction 側 (`executor` は canonical 名。 旧 alias `dev` も deprecation warning 付きで受理される)
- **`default_instruction`** (単一 Instruction object) が `executing` で claude-code に渡される。 commit / push / PR 作成 / CI 待ち / 失敗時の判断 までを全部ここに書く
- 検証用の別 instruction (旧 `rework` / `review`) は不要。 失敗時は agent が abort するか、 オペレータが `boid task reopen <id> --message "..."` で新しい指示を渡して再開する

```bash
cd ~/src/github.com/<you>/boid-demo-repo
boid project add .
```

## タスクを作って観察する

例として README に 1 行追記するタスクを作ります。

```bash
boid task create <<'YAML'
project_id: boid-demo
title: Add a one-line "hello from boid" to README.md
behavior: executor
auto_start: true
YAML
```

別ターミナルで状態を流し見ます。

```bash
boid task watch <task-id>
```

期待される流れ:

1. `pending → executing` (`auto_start: true` のため)
2. `executing`: `boid` が新しいブランチで worktree を作成 → claude-code hook が起動
3. agent が編集 → commit → push → `gh pr create` → `gh pr checks --watch`
4. agent が正常終了 → 状態機械が `executing → done` に自動遷移
5. `done` への遷移直前で `github-auto-merge` exit gate が `gh pr merge` を実行 → PR マージ完了

最終状態を確認:

```bash
boid task show <task-id>
```

PR がマージされ、 worktree が片付けられているはずです。

## CI が落ちたとき

agent の instruction で「CI 失敗時は abort する」 ように書いてある場合、 task は `aborted` で終端し、 `lifecycle.abort.code` / `lifecycle.abort.message` に理由が記録されます。

オペレータがリカバリしたい場合は:

```bash
# done のタスクは reopen で再開
boid task reopen <task-id> --message "lint エラーを修正して再 push してください"
```

aborted のタスクを再開したい場合は `boid task rerun <id>` で `pending` に戻して再実行します。

## auto-merge コンフリクト

`github-auto-merge` の exit gate が `gh pr merge` で conflict を検出すると、 gate の exit code が非 0 になり `done` への遷移がブロックされます。 task は `executing` のまま残るので、

```bash
boid task reopen <task-id> --message "main の最新を merge して conflict を解消、 再 push してください"
```

で agent に再修正を依頼します。

## なぜこの構成にするか

この章で示した構成のキモは、 **ワークフローの本体を instruction に書く** ことです。

- 専用の検証 kit を別に組まなくても、 agent に CI 結果の解釈と abort 判断を指示すれば足りる
- プロジェクトごとに「どこまで自動化するか」「どんな失敗を打ち切るか」 を instruction で個別に調整できる
- `boid` 本体は状態機械の駆動と worktree / auto-merge の周辺にだけ責任を持ち、 kit と instruction の組み合わせで多様なワークフローを表現する

## 次に読むもの

- [ワークフロー](../../workflows.md) — エンドツーエンドの 3 つのワークフロー形 (ローカル merge / 1 executor 1 PR / 1 supervisor 1 PR) を `project.yaml` テンプレート付きで紹介
- [概念](../guide/concepts.md) — 出てきた用語の改めての定義
- [状態機械](../guide/state-machine.md) — 状態遷移の正確なルール
- [Web UI](../guide/web-ui.md) — このタスクをブラウザ / スマホから観察する
- [トラブルシューティング](../guide/troubleshooting.md) — 詰まった時に
