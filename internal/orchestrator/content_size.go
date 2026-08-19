package orchestrator

import "fmt"

// docs/plans/ingestion-identity.md PR-2 (B-2), 決定 J-10 / A-5: `description`
// と action payload に上限を置き、超えたら「切り詰めずにエラー」にする。
//
// 根拠 (決定 3 改訂の帰結): 原文が daemon 側の description として載るように
// なった以上、daemon が機械的に効かせられる開示の枠はサイズとフィールド粒度
// だけになった (逆輸入 3 — 中身は解釈しない)。黙って切ると head/agent 発の
// source (外部に正本が無い) は復元できなくなるため、エラーにして workspace
// 側が要約に落とすか分割するかを選べるようにする。
//
// 上限の値 (2026-08-19、この PR で実測): khi の note (取り込み済み 32 枚:
// jira 23 枚 / slack 8 枚 / frontmatter 無しの手動メモ 1 枚。実データ上
// source.type は jira と slack のみで、bitbucket/mail/head/agent は 0 件) の
// 本文 (frontmatter を除いた markdown 部分) バイト数分布は次のとおり:
//
//	jira  (n=23): max 15,584B / median 1,259B / min   213B
//	slack (n= 8): max 13,283B / median 6,302B / min 1,298B
//
// 観測された最大は jira の 15,584B (ES image registry 起票の課題本文)。
// MaxContentBytes は 64 KiB (65,536B) — 実測最大の約 4.2 倍のマージンを取り、
// frontmatter を含めた同ノートのファイル全体サイズの最大 (31,761B) の 2 倍
// 以上をカバーする。実測 32 件は一度も 16KB を超えていないため、64KiB は
// 「日常的な Jira 課題本文 / Slack スレッド全文が絶対に切られない」ための
// 十分な余裕でありながら、1 record が無制限に肥大化するのは防ぐ値として選んだ。
//
// 未確定のまま残るリスク (doc 12 節・このコメントに明記): khi 側 doc は
// 「mail を足したときに本文 + 添付テキストが載ることは未確定」と書いている。
// 添付テキストの抽出結果は jira/slack の会話ログよりずっと大きくなり得るため、
// mail 対応時にこの実測前提が破れる可能性がある。そのときは新たに mail の
// 本文分布を実測し、この定数を見直すこと — 黙って追認しない。
//
// 適用箇所は 1 箇所ではない (J-10): description の入口である task_create /
// task_update / BoidOpTaskResolveOrCapture の新規作成パスと、action payload
// の入口である action_send (= TaskWorkflowService.ApplyAction) の 4 箇所
// すべてがこの ValidateContentSize を通る。どれか 1 つでも抜けると迂回路に
// なるため、新しい書き込み経路を足すときは必ずここを通すこと。
const MaxContentBytes = 64 * 1024 // 65,536 bytes

// ContentSizeError is ValidateContentSize's error type. Exported so a caller
// that wants the raw byte counts (rather than parsing Error()'s text) can use
// errors.As — the message text itself is not a public contract (PR-1 review
// note: don't pin stderr verbatim).
type ContentSizeError struct {
	Field       string
	ActualBytes int
	LimitBytes  int
}

func (e *ContentSizeError) Error() string {
	return fmt.Sprintf("%s exceeds the size limit: %d bytes (limit %d bytes)", e.Field, e.ActualBytes, e.LimitBytes)
}

// ValidateContentSize rejects content whose byte length exceeds
// MaxContentBytes. field names the content in the returned error (e.g.
// "description", "action payload") so a caller can tell which limit was hit.
func ValidateContentSize(field string, content []byte) error {
	if len(content) <= MaxContentBytes {
		return nil
	}
	return &ContentSizeError{Field: field, ActualBytes: len(content), LimitBytes: MaxContentBytes}
}
