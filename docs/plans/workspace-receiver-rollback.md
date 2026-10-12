# Workspace receiver 移行のデプロイと復元

migration 0061 は default metaproject の tasks / jobs / trigger_runs / task_identities を workspace 直属へ付け替え、projects / project_workspaces の receiver 行を削除する。action / card request / barrier は task の ID を維持して残す。

**片道ドア: default metaproject 行を削除するデータ移行。デプロイ前 DB backup 必須、戻すには backup 復元のみ。** 旧バイナリは `checkSchemaCeiling` で migration 0061 の DB を拒否する。プレースホルダ project の追加では戻せない。backup 復元時は、デプロイ後に作成・更新された task、job、card、signal 等が失われる。

デプロイ前に次を行う。

1. daemon と DB を開く全プロセスを停止する。compose 運用なら `docker compose -f build/container/compose.yml stop daemon`、直接起動なら `boid stop` を使う。実際に配備した compose ファイルを指定する。
2. 実際の DB パスを確認する。直接起動の既定は `${XDG_DATA_HOME:-$HOME/.local/share}/boid/boid.db`、compose では `boid_state` volume の daemon が使っているファイル。停止時の checkpoint 完了を確認し、SQLite の backup 機能、または停止状態の DB ファイルと存在する `-wal` / `-shm` を一組で保存する。backup、旧バイナリ・旧 image、および PR-5 前の mirror を保持する。
3. backup のコピーで migration 0061 を検証する。workspace の不整合、identity の孤立、workspace 内の一意キー重複、未対応テーブルから receiver への参照がある場合は migration が transaction 全体を rollback して起動を拒否する。エラーに出た行の所属を原資料と照合し、重複の帰属や追加参照の移行方法を決めてから修復・再実行する。行や migration 履歴を削除して検査を回避しない。
4. 新バイナリを起動する。receiver が消え、card / identity / trigger 履歴が残り、workspace trigger と組み込み judge が動くことを確認する。

旧バイナリに戻す場合は次の順序を守る。

1. 新 daemon と DB を開く全プロセスを停止する。
2. 失敗調査用に現 DB と sidecar を別に保存する。
3. **デプロイ前 backup の DB ファイルへ差し替える。** 新 DB の `-wal` / `-shm` を混在させず、backup に sidecar が含まれるなら同じ組だけを復元する。元のファイル所有者・権限を維持する。
4. 保持していた旧バイナリ・旧 image で daemon を起動する。新バイナリを先に起動すると再び migration が走る。
5. backup 時点の task / card / trigger 履歴と project 設定を確認する。

Q22 の実 DB コピーを使った移行・復元の通し確認は、owner がデプロイ前に行う残作業。自動テストの SQLite fixture 検証は、その運用確認の代わりにはしない。
