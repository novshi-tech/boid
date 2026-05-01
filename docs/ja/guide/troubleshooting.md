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

2 つの可能性があります。

1. **hook (実行スクリプト) が終了していない。** プロンプト待ち、終わらない対話コマンド、応答停止したエージェントなどでブロックされていると、 daemon 側はそのジョブの完了を待ち続けます。 `boid job list --task <id>` に `running` のままのジョブが見えるはずです。 `boid task abort <id>` で打ち切り、 hook スクリプトを確認してください
2. **`done` の exit gate が失敗を返している。** `gh pr merge` の conflict など host 側 gate が exit code 非 0 を返すと、 状態機械は `executing → done` の遷移をブロックします。 `boid job list --task <id>` で gate ジョブの exit code を確認し、 conflict 等であれば `boid task reopen <id> --message "..."` で agent に再修正を依頼してください

## `boid task list` が遅い / ディスクが膨れる

ローカルデータは 2 箇所に蓄積します。

| パス | 管理主体 | 自動 GC |
|---|---|---|
| `~/.local/share/boid/runtimes/<id>/` | `boid` | あり (24h ごと、 30d より古いものを削除) |
| `~/.claude/projects/-home-...-worktrees-boid-<taskid>/` | Claude Code | **なし** — 手動でクリーンアップ |

前者は自動 GC されます。後者は Claude Code 自身が書き込むもので、 `boid` は手を出しません。 `~/.claude/projects/` が肥大化しているなら手動で消します:

```bash
rm -rf ~/.claude/projects/-home-*-worktrees-boid-*
```

(他プロジェクトのエントリを巻き込まないよう注意してください。)

GC 設定は `~/.config/boid/config.yaml`:

```yaml
gc:
  enabled: true
  interval: 24h
  older_than: 720h    # 30 日
```

## hook 内で "permission denied" や "unknown command" が出る

hook はサンドボックス内で動くため、 kit が `host_commands` に宣言していないコマンドを呼ぼうとすると拒否されます。直し方は 2 通りあります。

- 不足しているコマンドを kit の `host_commands` リストに追加する (`git push` のような汎用ツール向け)
- その作業を gate (状態遷移時に host 側で動くスクリプト) に移す (`systemctl restart` のような環境依存操作向け)

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
