"""boidmeta.boid_store のテスト。

boid CLI (sandbox 内の shim) の薄いラッパ。**ここでテストするのは「どんな引数を組み立てて
何を読み取るか」**であって、boid 側の挙動ではない —— それは boid のソースで確認した事実を
コメントに残す形で持っている。

過去に踏んだ事故がそのまま回帰テストになっている: project 名をそのまま送る / `--status` を
渡す / payload を消費しない action type に payload を付ける / **返り値の形を読み違える**。

この層は「argv を組み立てて応答を読む」だけなので、**argv の中身と返り値を実際に assert
しないとテストが何も固定しない**。レビュー (2026-08-22) でそこに穴が集中していたので、
旗の名前だけでなく**どの旗にどの値が載るか**まで見る。
"""
from __future__ import annotations

import json
import unittest

from boidmeta.boid_store import (
    DEFAULT_SIGNAL_LIST_LIMIT,
    IDENTITY_NOT_FOUND_EXIT_CODE,
    PAYLOAD_CONSUMING,
    BoidCLI,
    BoidError,
)


class FakeProcess:
    def __init__(self, returncode: int, stdout: str, stderr: str) -> None:
        self.returncode = returncode
        self.stdout = stdout
        self.stderr = stderr


class FakeRun:
    """`subprocess.run` の差し替え。呼ばれた引数を記録し、用意した応答を順に返す。"""

    def __init__(self, *responses) -> None:
        self.responses = list(responses)
        self.calls: list[tuple[list[str], str | None, dict]] = []

    def __call__(self, argv, *, input=None, **kwargs):  # noqa: A002
        self.calls.append((list(argv), input, kwargs))
        if not self.responses:
            return FakeProcess(0, "", "")
        item = self.responses.pop(0)
        if isinstance(item, tuple):
            return FakeProcess(*item)
        return FakeProcess(0, item, "")

    @property
    def args(self) -> list[str]:
        return self.calls[-1][0]

    @property
    def stdin(self) -> str | None:
        return self.calls[-1][1]

    @property
    def kwargs(self) -> dict:
        return self.calls[-1][2]

    def value_of(self, flag: str) -> str | None:
        """`--flag VALUE` の VALUE。旗の名前だけでなく**どの値が載ったか**を見るため。"""
        args = self.args
        return args[args.index(flag) + 1] if flag in args and args.index(flag) + 1 < len(args) else None


def cli(*responses, **kwargs) -> tuple[BoidCLI, FakeRun]:
    run = FakeRun(*responses)
    return BoidCLI(run=run, **kwargs), run


class RunTest(unittest.TestCase):
    """`_exec`/`_run` の共通の振る舞い。呼び出しの実体は何でもよいので `notify_progress`
    (payload を持たない単純な書き込み) を使う。"""

    def test_a_non_zero_exit_raises_with_the_stderr(self):
        client, _ = cli((1, "", "boom"))
        with self.assertRaises(BoidError) as caught:
            client.notify_progress("t1", "x")
        self.assertIn("boom", str(caught.exception))

    def test_the_binary_leads_the_argv(self):
        client, run = cli()
        client.notify_progress("t1", "x")
        self.assertEqual(run.args, ["boid", "task", "notify", "t1", "--progress", "x"])

    def test_encoding_is_pinned_to_utf8(self):
        """`text=True` だけだとロケール依存になり、C ロケールでは日本語の payload が
        UnicodeEncodeError になる。この層は日本語をそのまま通す設計なので入口も固定する。"""
        client, run = cli()
        client.notify_progress("t1", "x")
        self.assertEqual(run.kwargs.get("encoding"), "utf-8")

    def test_the_timeout_is_passed_through(self):
        client, run = cli(bin="boid", timeout=12.5)
        client.notify_progress("t1", "x")
        self.assertEqual(run.kwargs.get("timeout"), 12.5)


class SendActionTest(unittest.TestCase):
    def test_payload_goes_through_stdin(self):
        """`--payload` はファイルパス (または `-`) を取る。インライン JSON ではない
        (`internal/sandbox/boid_shim.go` / `cmd/action.go`)。"""
        client, run = cli()
        client.send_action("t1", "attrs_set", {"urgency": "week"})
        self.assertEqual(run.args, ["boid", "action", "send", "--task", "t1", "--type", "attrs_set", "--payload", "-"])
        self.assertEqual(json.loads(run.stdin), {"urgency": "week"})

    def test_japanese_is_not_escaped(self):
        """人が timeline で読む値でもある。`\\uXXXX` に化けると読めない。"""
        client, run = cli()
        client.send_action("t1", "attrs_set", {"summary": "障害の調査"})
        self.assertIn("障害の調査", run.stdin or "")

    def test_no_payload_means_no_flag(self):
        client, run = cli()
        client.send_action("t1", "triage")
        self.assertEqual(run.args, ["boid", "action", "send", "--task", "t1", "--type", "triage"])
        self.assertIsNone(run.stdin)

    def test_a_payload_on_a_non_consuming_type_is_refused(self):
        """daemon はこの集合に無い type の payload を消費せず `task.Payload` へ merge
        してしまう。送ってしまうと task の payload が汚れ、しかもエラーにならない。"""
        client, _ = cli()
        with self.assertRaises(BoidError) as caught:
            client.send_action("t1", "triage", {"anything": 1})
        self.assertIn("triage", str(caught.exception))

    def test_every_consuming_type_is_accepted(self):
        """**設計が使う型が全部通ること。** 集合を縮めると、送るべき action が送れなくなる
        (`child_added` / `child_specced` が落ちると子が立たない)。"""
        for action_type in ("park", "attrs_set", "child_added", "child_specced", "child_dropped",
                            "noted", "answered"):
            client, _ = cli()
            client.send_action("t1", action_type, {"k": "v"})

    def test_the_consuming_set_matches_the_daemon(self):
        """**2026-08-25 訂正 (PR-K レビュー LOW 6)**: `internal/api/workflow_action.go` の
        `sideEffectConsumesPayload` map は **6 つ** (park/attrs_set/child_added/
        child_specced/child_dropped/noted) —— `answered` はここに無い。`answered` は
        card 遷移 (accept/reject) の専用実装 (`applyAnswered`,
        `internal/api/suggestion_accept.go`) に完全にバイパスされ、
        `sideEffectConsumesPayload` を経由する汎用マージパイプライン自体に一度も
        到達しない (同ファイルの doc comment: 「Bypasses ApplyAction's generic action
        pipeline entirely」)。だから `answered` の payload が `task.Payload` へ誤って
        merge される心配はそもそも無く、この集合に含めても安全 —— **ただし
        「daemon の 1 つの map と一致する」という意味での parity ではない**、
        「daemon 側に payload が安全に届く 7 つの経路 (6 つの汎用マージ回避 + `answered`
        専用パスの計 7 つ) と一致する」という意味。過剰に厳しいと「daemon が正しく
        消費するのに khi が送らせない」で詰まる。

        **`child_dropped` を落としていて本番で詰まった** (2026-08-23)。boid #982 で
        daemon 側に足したのに、こちらの集合と このテストが 6 つのまま「daemon と同じ」と
        主張して green だった —— テストがバグの側を固定していた形。
        `drop-child` verb は adapter に弾かれて一度も boid へ届いていなかった。
        """
        self.assertEqual(
            PAYLOAD_CONSUMING,
            frozenset({"park", "attrs_set", "child_added", "child_specced", "child_dropped",
                       "noted", "answered"}),
        )


class NotifyTest(unittest.TestCase):
    def test_the_progress_text_rides_the_progress_flag(self):
        """**旗の名前だけ見ても何も固定できない。** `-m` と `--progress` の値を入れ替えると、
        (a) timeline の progress payload がダミーになって記録がゼロ件と読まれ、
        (b) 記録本文が message 側へ回って**見送り 1 件ごとに人へ通知 hook が飛ぶ**。
        どちらも例外が出ないので、値がどちらの旗に載るかを見るしかない。
        """
        client, run = cli()
        client.notify_progress("t1", "khi-record v1 {}")
        self.assertEqual(run.value_of("--progress"), "khi-record v1 {}")
        self.assertNotEqual(run.value_of("-m"), "khi-record v1 {}")

    def test_the_progress_notify_carries_no_message_flag(self):
        """**回避を外した後の不変条件。** `internal/sandbox/broker.go` の `BoidOpTaskNotify` は
        progress の有無を見ずに `Message == ""` を弾いていた —— shim もサービス層も progress
        単独を許すのに broker だけが厳しい方へ drift していた。novshi-tech/boid#980 で修正し、
        **2026-08-22 にデプロイして sandbox 内から `--progress` 単独が通ることを実機で確認した**
        (progress action が 1 本だけ載る)。ダミーの `-m` 併記はここで不要になった。

        argv 全体を固定するのは、回避が別の形で戻ってきたときに落とすため。
        """
        client, run = cli()
        client.notify_progress("t1", "khi-record v1 {}")
        self.assertEqual(
            run.args, ["boid", "task", "notify", "t1", "--progress", "khi-record v1 {}"]
        )

    def test_notify_carries_the_message(self):
        """伝達 (§5.4) はこちら。progress と違い hook が発火する。"""
        client, run = cli()
        client.notify("t1", "サマリーが変わった")
        self.assertEqual(run.value_of("-m"), "サマリーが変わった")
        self.assertNotIn("--progress", run.args)


class ResolveIdentityTest(unittest.TestCase):
    """`boid task identity resolve <identity>` —— 非 boid シグナルの帰属先を引く (§5.1)。

    **未登録は例外ではなく exit code で来る。** boid 側の設計 doc が明示している
    (「未登録は「見つからない」を exit code で表し、エラーにしない」) —— get-or-create を
    する呼び出し側が、stderr の文字列を pattern match せずに「無かった」と「本当の失敗」を
    見分けられるようにするため。
    """

    def test_it_returns_the_task_id_and_status(self):
        """応答は **JSON** (`{"task_id":..., "status":...}`)。素の id ではない
        (`internal/server/boid_executor.go`)。"""
        client, run = cli(json.dumps({"task_id": "t-1", "status": "parked"}))
        self.assertEqual(client.resolve_identity("jira:X-1"), ("t-1", "parked"))
        self.assertEqual(run.args, ["boid", "task", "identity", "resolve", "jira:X-1"])

    def test_a_miss_is_none_not_an_error(self):
        """**exit code 2 を例外にすると、新規候補が 1 件も立たなくなる** —— 未登録の
        identity こそが起票の候補なので、そこで落ちると経路全体が死ぬ。"""
        client, _run = cli((IDENTITY_NOT_FOUND_EXIT_CODE, "", "not found"))
        self.assertIsNone(client.resolve_identity("slack-thread:1.0"))

    def test_a_real_failure_still_raises(self):
        """exit 1 は握り潰さない。握り潰すと「全部が新規候補」に見え、**既存 task に
        合流すべきシグナルが二重起票される**。"""
        client, _run = cli((1, "", "boom"))
        with self.assertRaises(BoidError):
            client.resolve_identity("jira:X-1")


class LinkIdentityTest(unittest.TestCase):
    def test_the_identity_comes_before_the_task_id(self):
        """`boid task identity link <identity> <task-id>`。**逆にすると identity として
        task id が登録され、以降その task を identity で引けなくなる。**"""
        client, run = cli()
        client.link_identity("jira:X-1", "task-9")
        self.assertEqual(run.args, ["boid", "task", "identity", "link", "jira:X-1", "task-9"])


class ResolveProjectTest(unittest.TestCase):
    """`child_specced` の project は**解決済み UUID でなければならない** ——
    `action send` は broker で名前解決されず、executor の `AllowsProject` は UUID の
    完全一致比較。名前を渡すと毎回落ちる (2026-08-21 に本番で踏んだ形)。
    """

    JSON_OUT = json.dumps([{"id": "p-uuid-1", "name": "rook-tools"}, {"id": "p-uuid-2", "name": "khi"}])
    PLAIN_OUT = "p-uuid-1 active rook-tools (/w/rook-tools) upstream=x\np-uuid-2 active khi (/w/khi)\n"

    def test_resolves_a_name_from_json_output(self):
        client, _ = cli(self.JSON_OUT)
        self.assertEqual(client.resolve_project("rook-tools"), "p-uuid-1")

    def test_resolves_a_name_from_plain_output(self):
        """**出力形式はホストとサンドボックスで違う** —— ホストの実 CLI は plain テキスト、
        sandbox の shim は JSON で、しかも `--output json` を受け付けない。両方読む。"""
        client, _ = cli(self.PLAIN_OUT)
        self.assertEqual(client.resolve_project("khi"), "p-uuid-2")

    def test_an_id_passes_through(self):
        client, _ = cli(self.JSON_OUT)
        self.assertEqual(client.resolve_project("p-uuid-2"), "p-uuid-2")

    def test_an_unknown_name_raises_with_the_known_ones(self):
        client, _ = cli(self.JSON_OUT)
        with self.assertRaises(BoidError) as caught:
            client.resolve_project("nope")
        message = str(caught.exception)
        self.assertIn("nope", message)
        self.assertIn("rook-tools", message)

    def test_the_index_is_loaded_once(self):
        client, run = cli(self.JSON_OUT)
        client.resolve_project("rook-tools")
        client.resolve_project("khi")
        self.assertEqual(len(run.calls), 1)

    def test_a_half_broken_json_does_not_leave_debris(self):
        """JSON を途中まで読んで失敗したとき、中途半端な結果を残さない。
        混ぜると plain 解析が JSON 本文の上を走り、`'"name":' -> '[{"id":'` のような
        ゴミが index に入る。`resolve_project` は values() を「もう UUID」の判定に
        使っているので、汚れた値がそのまま素通し集合になる。
        """
        client, _ = cli(json.dumps([{"id": "p1", "name": "alpha"}, {"id": "p2"}]))
        with self.assertRaises(BoidError) as caught:
            client.resolve_project('"name":')
        self.assertNotIn('[{"id":', str(caught.exception))

    def test_a_json_object_instead_of_a_list_yields_nothing(self):
        """配列でない JSON が返ったら空。ここで plain 解析へ倒すと JSON 本文の行を
        舐めてゴミが index に入る (エラーの「既知」欄にそれが現れる)。"""
        client, _ = cli(json.dumps({"projects": [{"id": "p1", "name": "alpha"}]}))
        with self.assertRaises(BoidError) as caught:
            client.resolve_project("alpha")
        message = str(caught.exception)
        self.assertIn("alpha", message)
        self.assertIn("既知: []", message)


class BehaviorsOfTest(unittest.TestCase):
    """振り先の project が持つ behavior 名。`app/write.py` が送信前の照合に使う。

    **`None` は「無い」ではなく「照合できなかった」。** 呼び出し側はこれを素通しに
    倒すので、ここで両者を混ぜると一時的な不調が「その behavior は無い」に化ける。
    """

    #: 実物の `boid project behaviors` は値に instruction 本文まで積んで返す。
    OUT = json.dumps(
        {
            "implement": {"readonly": False, "default_instruction": {"message": "長い本文…"}},
            "research": {"readonly": True},
        }
    )

    def test_returns_the_behavior_names(self):
        client, _ = cli(self.OUT)
        self.assertEqual(set(client.behaviors_of("p-uuid-1") or ()), {"implement", "research"})

    def test_passes_the_project_id_through(self):
        client, run = cli(self.OUT)
        client.behaviors_of("p-uuid-1")
        self.assertEqual(run.args[-3:], ["project", "behaviors", "p-uuid-1"])

    def test_unparsable_output_is_none_not_empty(self):
        """未知の project は CLI がエラー文を吐く。`()` を返すと呼び出し側が
        「behavior が 1 つも無い」と読んで全部拒否する。"""
        client, _ = cli('boid project behaviors: resolve project "x": no project matches ref "x"')
        self.assertIsNone(client.behaviors_of("x"))

    def test_a_failing_call_is_none(self):
        client, _ = cli((1, "", "boom"))
        self.assertIsNone(client.behaviors_of("p-uuid-1"))

    def test_the_result_is_cached_per_project(self):
        """1 巡で同じ project へ複数の子を立てることがある。"""
        client, run = cli(self.OUT)
        client.behaviors_of("p-uuid-1")
        client.behaviors_of("p-uuid-1")
        self.assertEqual(len([c for c in run.calls if "behaviors" in c[0]]), 1)

    def test_a_failure_is_cached_too(self):
        """読めなかったことも覚える —— 失敗を毎回引き直すと 1 巡で何度も待たされる。"""
        client, run = cli((1, "", "boom"))
        self.assertIsNone(client.behaviors_of("p-uuid-1"))
        self.assertIsNone(client.behaviors_of("p-uuid-1"))
        self.assertEqual(len([c for c in run.calls if "behaviors" in c[0]]), 1)


class GetCardTest(unittest.TestCase):
    """`CardView` (`internal/api/card_read.go`、旧 `TaskTriageView` / `triage_read.go`) は
    task_id / project_id / title / status / description / kind / urgency / wake_at /
    wake_task_id / parked_from / detail。**top-level の `summary` は無い。**

    **2026-08-25、`boid task triage <id>` → `boid card get <id>` へ rename**
    (`docs/plans/card-model-cleanup.md` §4)。互換 alias は無い。
    """

    PROJECTION = json.dumps(
        {
            "task_id": "t1",
            "status": "working",
            "urgency": "week",
            "description": "本文",
            "detail": {"attrs": {"urgency": "week", "khi_record": []}, "children": [{"id": "ch:x:boid:a1"}]},
        }
    )

    def test_reads_one_tasks_projection(self):
        client, run = cli(self.PROJECTION)
        view = client.get_card("t1")
        self.assertEqual(run.args, ["boid", "card", "get", "t1"])
        self.assertEqual(view["status"], "working")

    def test_detail_may_arrive_as_a_json_string(self):
        """現行の boid は `json.RawMessage` で返すので常にオブジェクト。文字列も読むのは
        別経路 (DB ダンプ等) から食わせても壊れないための防御。"""
        payload = json.dumps({"task_id": "t1", "detail": json.dumps({"attrs": {"urgency": "now"}})})
        client, _ = cli(payload)
        self.assertEqual(client.attrs_of("t1"), {"urgency": "now"})

    def test_attrs_of_an_empty_detail(self):
        client, _ = cli(json.dumps({"task_id": "t1"}))
        self.assertEqual(client.attrs_of("t1"), {})

    def test_a_non_object_response_raises(self):
        client, _ = cli("[]")
        with self.assertRaises(BoidError):
            client.get_card("t1")


class ClaimSignalsTest(unittest.TestCase):
    """`boid signal claim <id>...` —— 判断に回す行を名指しして attempts を進める
    (2026-08-29、boid #1033)。"""

    def test_ids_ride_as_positional_arguments(self):
        client, run = cli()
        client.claim_signals(["sig-1", "sig-2"])
        self.assertEqual(run.args, ["boid", "signal", "claim", "sig-1", "sig-2"])

    def test_an_empty_list_does_not_call_the_cli(self):
        """空で叩くと boid が「id を 1 件以上」と言って失敗する。無駄な subprocess も
        避ける (`ack_signals` と同じ規律)。"""
        client, run = cli()
        client.claim_signals([])
        self.assertEqual(run.calls, [])

    def test_a_failure_raises(self):
        client, _ = cli((1, "", "unknown id(s) in workspace \"khi\": typo"))
        with self.assertRaises(BoidError):
            client.claim_signals(["typo"])


class TaskFieldTest(unittest.TestCase):
    def test_reads_one_field(self):
        """S-9 の actor 判別の唯一の経路 —— `card list` (旧 `task triage --list`) の行にも
        `task list` の出力にも behavior は載っていない。"""
        client, run = cli("sweep\n")
        self.assertEqual(client.task_field("t1", "behavior"), "sweep")
        self.assertEqual(run.args, ["boid", "task", "show", "t1", "--field", "behavior"])


class CurrentFieldTest(unittest.TestCase):
    def test_reads_a_field_of_the_running_job_itself(self):
        """`task_field` と違い task id を引数に取らない —— `app/write.py` の
        `_readonly_forces_report` が「今動いている自分は readonly か」を、env
        (`BOID_TASK_ID`) 経由の偽装が効かない形で問い合わせるための経路。"""
        client, run = cli("false\n")
        self.assertEqual(client.current_field("readonly"), "false")
        self.assertEqual(run.args, ["boid", "task", "current", "--field", "readonly"])


class ResolveOrCaptureTest(unittest.TestCase):
    """**返るのは JSON** (`{"task_id": ..., "created": ...}`、`internal/server/boid_executor.go`)。
    素の id として読むと、その文字列が以降の `--task` に載って全部 404 になる。
    """

    OK = json.dumps({"task_id": "task-9", "created": True}) + "\n"

    def test_reads_the_task_id_out_of_the_json(self):
        client, _ = cli(self.OK)
        task_id, created = client.resolve_or_capture("jira:X-1", title="題", description="本文")
        self.assertEqual(task_id, "task-9")
        self.assertIs(created, True)

    def test_reports_when_the_task_already_existed(self):
        """`created` は S-2 の「立てた直後に初期化する / 合流ならしない」の分岐に使う。
        捨てると、既存 task に当たるたびに人が触った値を上書きする。"""
        client, _ = cli(json.dumps({"task_id": "task-9", "created": False}))
        _task_id, created = client.resolve_or_capture("jira:X-1", title="題", description="本文")
        self.assertIs(created, False)

    def test_the_identity_and_title_are_on_the_argv(self):
        client, run = cli(self.OK)
        client.resolve_or_capture("jira:X-1", title="題", description="本文")
        self.assertIn("jira:X-1", run.args)
        self.assertEqual(run.value_of("--title"), "題")

    def test_sends_the_description_through_stdin(self):
        client, run = cli(self.OK)
        client.resolve_or_capture("jira:X-1", title="題", description="本文")
        self.assertIn("--description-file", run.args)
        self.assertEqual(run.stdin, "本文")

    def test_does_not_pass_status(self):
        """着地 status は `parked` のまま (card 機械 v2 以前は `captured` だった、D-15)。
        `resolve-or-capture` は `--status` を受け付けず、渡すと新規起票が**恒久的に
        失敗する** (2026-08-21 の本番 1 巡目で発覚)。"""
        client, run = cli(self.OK)
        client.resolve_or_capture("jira:X-1", title="題", description="本文")
        self.assertNotIn("--status", run.args)

    def test_an_identity_conflict_is_reported(self):
        """衝突は exit=3 (`internal/sandbox/protocol.go`)。stderr 文字列では判定しない。"""
        client, _ = cli((3, "", "identity conflict"))
        with self.assertRaises(BoidError) as caught:
            client.resolve_or_capture("jira:X-1", title="題", description="本文")
        self.assertIn("jira:X-1", str(caught.exception))

    def test_an_empty_response_raises(self):
        """空の task id を返すと、それが以降の `--task` に載って静かに壊れる。"""
        client, _ = cli("\n")
        with self.assertRaises(BoidError):
            client.resolve_or_capture("jira:X-1", title="題", description="本文")

    def test_a_json_without_a_task_id_raises(self):
        client, _ = cli(json.dumps({"created": True}))
        with self.assertRaises(BoidError):
            client.resolve_or_capture("jira:X-1", title="題", description="本文")


class UpdateDescriptionTest(unittest.TestCase):
    """**`--description-file` は `task update` に無い** —— shim が受けるのは
    `--patch-file` / `--title` / `--description` / `--payload-file` だけで、
    `--description-file` は `resolve-or-capture` 専用 (`internal/sandbox/boid_shim.go`)。
    """

    def test_uses_patch_file_with_json_on_stdin(self):
        client, run = cli()
        client.update_description("t1", "## いま何が起きているか\n\n本文\n")
        self.assertEqual(run.args, ["boid", "task", "update", "t1", "--patch-file", "-"])
        self.assertEqual(json.loads(run.stdin)["description"], "## いま何が起きているか\n\n本文\n")

    def test_japanese_is_not_escaped(self):
        client, run = cli()
        client.update_description("t1", "障害の調査")
        self.assertIn("障害の調査", run.stdin or "")


class ListSignalsTest(unittest.TestCase):
    """`boid signal list` —— 2026-08-27 カットオーバー、検知を boid signal inbox 読みへ
    切り替える (`boidmeta.inbox`)。ここでは argv の組み立てと応答の素通しだけを見る
    —— envelope → `Signal` のマッピングは `boidmeta.inbox` のテストが持つ。
    """

    PAGE = json.dumps({"signals": [{"id": "slack:C1:1.0", "occurred_at": "2026-08-26T01:23:45Z"}]})

    def test_defaults_to_pending_state(self):
        client, run = cli(self.PAGE)
        client.list_signals()
        self.assertEqual(run.args, ["boid", "signal", "list", "--state", "pending", "--limit", str(DEFAULT_SIGNAL_LIST_LIMIT)])

    def test_the_read_never_charges_attempts(self):
        """**読みは副作用なし** (2026-08-29、boid #1033)。`--claim` は
        「読み出しが返した行」を一律で課金する形だったので渡さない —— 判断に回す行は
        `claim_signals` が名指しする。"""
        client, run = cli(self.PAGE)
        client.list_signals()
        self.assertNotIn("--claim", run.args)

    def test_a_custom_state_and_limit_are_passed(self):
        client, run = cli(self.PAGE)
        client.list_signals(state="dead", limit=50)
        self.assertEqual(run.value_of("--state"), "dead")
        self.assertEqual(run.value_of("--limit"), "50")

    def test_no_limit_omits_the_flag(self):
        client, run = cli(self.PAGE)
        client.list_signals(limit=None)
        self.assertNotIn("--limit", run.args)

    def test_returns_the_raw_payload(self):
        client, _ = cli(self.PAGE)
        payload = client.list_signals()
        self.assertEqual(payload["signals"][0]["id"], "slack:C1:1.0")

    def test_a_non_object_response_raises(self):
        """`actions_for`/`list_cards` と同じ規律 —— 形が違えば黙って空にしない。"""
        client, _ = cli("[]")
        with self.assertRaises(BoidError):
            client.list_signals()


class AckSignalsTest(unittest.TestCase):
    def test_ids_ride_the_argv_as_positionals(self):
        client, run = cli()
        client.ack_signals(["slack:C1:1.0", "jira:X-1:issue:2026-08-22T00:00"])
        self.assertEqual(run.args, ["boid", "signal", "ack", "slack:C1:1.0", "jira:X-1:issue:2026-08-22T00:00"])

    def test_an_empty_list_does_not_call_boid(self):
        """無駄な subprocess 起動を避ける —— 呼び出し側 (`boidmeta.inbox.ack`) が
        空リストを弾いていても、ここでも独立に守る。"""
        run = FakeRun()
        client = BoidCLI(run=run)
        client.ack_signals([])
        self.assertEqual(run.calls, [])

    def test_a_failure_raises(self):
        client, _ = cli((1, "", "no such signal"))
        with self.assertRaises(BoidError):
            client.ack_signals(["nope"])


if __name__ == "__main__":
    unittest.main()
