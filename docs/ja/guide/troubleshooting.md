# トラブルシューティング

メンテナ自身が引っかかった代表的な事例です。一覧にないものでも、 daemon ログ (`~/.local/state/boid/boid.log`) と `boid task show <id>` の組み合わせで原因が分かることがほとんどです。

## daemon が起動しない

```text
Error: boid server already running (socket: /run/user/1000/boid.sock)
```

別の `boid` プロセスがすでに listen しています。 `kill` ではなく `boid stop` で落としてください。

`boid stop` が "no server running" と言うのにこのエラーが出る場合は socket ファイルが残っているだけなので、手動で削除します。

```bash
rm "$XDG_RUNTIME_DIR/boid.sock"
```

## バグ修正をインストールしたのに変化がない

`go install` し直したが daemon を再起動し忘れているケースです。ディスク上のバイナリが置き換わっても、起動時に読み込んだコードはメモリ上に残り続けるため、再起動するまで daemon は古い挙動のままです。

診断:

```bash
# /proc/<pid>/exe が (deleted) と表示されていれば、起動時のバイナリがディスク上にもう無い (置き換えられた) サイン
ps -o pid,cmd -C boid
ls -l /proc/<pid>/exe
```

修正:

```bash
boid stop
boid start
```

「直したのにまだ起こる」の原因として最も多いのがこれです。迷ったら再起動してください。

## タスクが `executing` のまま終わらない

**hook (実行スクリプト) が終了していない。** プロンプト待ち、終わらない対話コマンド、応答停止したエージェントなどでブロックされていると、 daemon 側はそのジョブの完了を待ち続けます。 `boid job list --task <id>` に `running` のままのジョブが見えるはずです。 `boid action send --task <id> --type abort` で打ち切り、 hook スクリプトを確認してください。

## `boid task list` が遅い / ディスクが膨れる

ローカルデータは 2 箇所に蓄積します。

| パス | 管理主体 | 自動 GC |
|---|---|---|
| `~/.local/share/boid/runtimes/<id>/` | `boid` | あり (24h ごと、 30d より古いものを削除) |
| `~/.claude/projects/-workspace-<project-name>-.../` (project ごと) | Claude Code | **なし** — 手動でクリーンアップ |

前者は自動 GC されます。後者は Claude Code 自身が書き込むもので、 `boid` は手を出しません。

> **注意 (git gateway cutover 後の変化)**: cutover 前はタスクごとの host git worktree パス (`-home-...-worktrees-boid-<taskid>-` 形式) が cwd だったため、 セッションログはタスク単位で分かれていました。 cutover 後はジョブの cwd が sandbox 内の `/workspace/<project-name>` (project 名ベース、 task ID を含まない) になったため、 **同一 project の複数タスクが同じログディレクトリに集約されます**。 具体的なディレクトリ名は実際にタスクを 1 つ dispatch してから `~/.claude/projects/` を確認してください。 `~/.claude/projects/` が肥大化しているなら、 巻き込みたくない他プロジェクトのエントリがないか確認した上で該当ディレクトリを手動で削除します。

GC 設定は `~/.config/boid/config.yaml`:

```yaml
gc:
  enabled: true
  interval: 24h
  older_than: 720h    # 30 日
```

**GC が実際に削除するもの:** `runtimes/<runtime_id>/` ディレクトリに加え、 DB 上の終端済みタスク / アクション / ジョブ、 `/tmp/boid-*` 一時ファイル、 revoke 済みデバイスも削除されます。 初回 GC はデーモン起動直後ではなく、**起動から 10 秒後**に実行されます。

## hook 内で "permission denied" や "unknown command" が出る

hook はサンドボックス内で動くため、 project が割り当てられている **workspace** の `host_commands` に無い名前のコマンドを呼ぼうとすると拒否されます。 `host_commands` は二層構造であることに注意してください — workspace が持つのは参照 **名前** のリストだけで、 実際の定義 (`path`/`allow`/`deny`/`env`) は daemon 側の集約レジストリ `~/.config/boid/host_commands.yaml` にあります。 不足している場合は次のどちらか (または両方) が必要です:

- `boid workspace edit <slug> --from-file <yaml>` で workspace の `host_commands` リストに名前を追加する
- 名前がまだ daemon 側レジストリに無ければ `~/.config/boid/host_commands.yaml` に定義を追記し、 `boid host-commands reload` で反映する ([オンボーディング / host_commands を定義する](onboarding.md#host_commands-を定義する-daemon-側の集約レジストリ) 参照)

(`git push`、 `gh` などはこの形で許可します。)

## Web UI: デバイスが何度もログアウトされる

cookie は `HttpOnly; Secure; SameSite=Lax` です。スマホブラウザが「閉じたら cookie 削除」設定だとデバイスログインが残りません。別ブラウザを使うか、当該ホストだけそのポリシーを外してください。

ペアリング後に公開 URL を変更すると、通知から飛ぶマジックリンクが古い URL を指したままになります。 `boid web set-url <new-url>` で更新してください。

## Web UI: ペアリングコードが "expired" / "invalid" になる

コードは発行 5 分後に失効、単回使用です。 `boid web pair` で再発行してください。

## `/proc/<pid>/exe` の boid に `(deleted)` が見える

disk 上のバイナリが置き換わったが daemon はまだ古いコードを動かしています。 [バグ修正をインストールしたのに変化がない](#バグ修正をインストールしたのに変化がない) を参照。

## どこを最初に見るか

- **daemon ログ**: `~/.local/state/boid/boid.log` (ローテーションあり)
- **タスク状態**: `boid task show <id>`
- **ジョブログ**: `boid job show <id>`
- **ライブ更新**: `boid task watch <id>` または Web UI のタスク詳細

バグらしき挙動なら、タスク / ジョブ ID と daemon ログの該当部分を添えて issue を立ててください。
