# HarnessAdapter.Usage() 返却型 + jobs テーブル schema 方針

> Status: 設計確定 (2026-06-17)  
> Phase: Phase 2 準備 (agent-aware-boid.md 設計判断 6 の詳細化)  
> この文書は Phase 3 の jobs テーブル拡張マイグレーションと claude adapter 実装の直接の入力になる。

## 目的

`HarnessAdapter.Usage()` の返却型 (Go の型) と jobs テーブルへの usage 保存 schema 方針を確定する。
Phase 3 着手前に確定させることで「最大公約数 schema で書いた後に harness 固有データの
保存方針で揉める」事態を防ぐ。

---

## 1. 各ハーネスの usage 粒度調査

### 1.1 Claude Code (claude adapter) — 実機調査済み

#### 調査方法

`~/.claude/projects/` 配下のセッション jsonl を直接読んで確認した。
以下のパス群を対象にした:

- `~/.claude/projects/-home-nosen-src-github-com-novshi-tech-boid/*.jsonl` (boid 本体プロジェクト)
- `~/.claude/projects/-home-nosen--local-share-boid-worktrees-*/*.jsonl` (boid タスクのサンドボックス)
- サブエージェントの jsonl: `<session_id>/subagents/agent-*.jsonl`

複数セッション (モデル: claude-opus-4-6 / 4-7 / 4-8 / claude-sonnet-4-6) を横断して確認。

#### jsonl ファイルの構造

```
~/.claude/projects/<project-dir-encoded>/<session-id>.jsonl
```

- `<project-dir-encoded>`: プロジェクト作業ディレクトリの `/` を `-` で置換したもの
- `<session-id>`: UUID v4。ファイル名のステムであり、各エントリの `sessionId` フィールドと一致
- `type: "assistant"` のエントリが API レスポンスを保持し、その中に usage データがある

#### usage フィールドの実測値

```json
{
  "type": "assistant",
  "message": {
    "model": "claude-opus-4-8",
    "usage": {
      "input_tokens": 5215,
      "cache_creation_input_tokens": 18882,
      "cache_read_input_tokens": 15850,
      "output_tokens": 336,
      "server_tool_use": {
        "web_search_requests": 0,
        "web_fetch_requests": 0
      },
      "service_tier": "standard",
      "cache_creation": {
        "ephemeral_1h_input_tokens": 18882,
        "ephemeral_5m_input_tokens": 0
      },
      "inference_geo": "not_available",
      "iterations": [
        {
          "input_tokens": 5215,
          "output_tokens": 336,
          "cache_read_input_tokens": 15850,
          "cache_creation_input_tokens": 18882,
          "cache_creation": {
            "ephemeral_5m_input_tokens": 0,
            "ephemeral_1h_input_tokens": 18882
          },
          "type": "message"
        }
      ],
      "speed": "standard"
    }
  },
  "sessionId": "1b9e9210-6ed6-4ddc-a3ac-887138e1d0ce",
  ...
}
```

#### フィールド意味と粒度

| フィールド | 意味 | 粒度 |
|---|---|---|
| `input_tokens` | キャッシュ非使用の入力トークン数 | メッセージ単位 |
| `output_tokens` | 出力トークン数 | メッセージ単位 |
| `cache_creation_input_tokens` | キャッシュ書き込みトークン数 | メッセージ単位 |
| `cache_read_input_tokens` | キャッシュ読み取りトークン数 | メッセージ単位 |
| `cache_creation.ephemeral_1h_input_tokens` | 1h キャッシュへの書き込みトークン | メッセージ単位 |
| `cache_creation.ephemeral_5m_input_tokens` | 5m キャッシュへの書き込みトークン | メッセージ単位 |
| `server_tool_use.web_search_requests` | サーバー側 web 検索リクエスト数 | メッセージ単位 |
| `server_tool_use.web_fetch_requests` | サーバー側 web フェッチリクエスト数 | メッセージ単位 |
| `service_tier` | サービス帯 (e.g. `"standard"`) | メッセージ単位 |
| `inference_geo` | 推論実行ロケーション | メッセージ単位 |
| `iterations` | API コール単位のトークン内訳配列 (通常 1 要素) | メッセージ単位 |
| `speed` | 速度帯 (e.g. `"standard"`) | メッセージ単位 |

実測した全セッションで `iterations` は常に 1 要素だった。構造上は複数要素を許容するが、
通常のメッセージ呼び出しでは 1 要素が前提と見てよい。

`model` は `usage` の外側 (`message.model`) にある。`<synthetic>` という値は
ツール結果合成などの内部ターンで使われ、トークン数はすべて 0。

#### HarnessAdapter.Usage() で読む場所

Claude adapter は以下の経路で jsonl にアクセスする:

1. `jobID` → DB 参照 → `jobs.runtime_id`、`jobs.task_id`
2. `task_id` → DB 参照 → `task.payload` の `artifact.claude_code.sessions[].id` → `session_id`
3. `task_id` → DB 参照 → `task.project_id` → `projects.work_dir` → jsonl ディレクトリ特定
4. `~/.claude/projects/<encoded(work_dir)>/<session_id>.jsonl` を読んで usage を集計

ただし、この方法は `~/.claude/projects/` というクロードコードが管理するパスに adapter が
直接触れる設計になる。Phase 3 実装時の代替案として、`run-agent.py` の終了処理 (cleanup 時) に
jsonl を読んで `<runtimes_dir>/<runtime_id>/usage.json` に書き出す方式も検討する。
後者なら adapter は boid 管理下のパスのみ参照すればよく、より独立性が高い。
どちらが優るかは Phase 3 着手時に決定する。

---

### 1.2 Codex (OpenAI Codex CLI) — doc レベル調査

実機未検証。OpenAI API ドキュメントと公開情報をもとにした best-effort 調査。
**実装時に adapter 作者が実機確認すること。**

OpenAI の chat completions API は `usage` を以下の形式で返す:

```json
{
  "usage": {
    "prompt_tokens": 1234,
    "completion_tokens": 567,
    "total_tokens": 1801,
    "prompt_tokens_details": {
      "cached_tokens": 1000
    },
    "completion_tokens_details": {
      "reasoning_tokens": 0
    }
  }
}
```

Claude と比較した主な違い:
- キャッシュは `cached_tokens` のみ (書き込みトークン相当なし)
- モデル名は API リクエストの `model` フィールドで指定され、レスポンスの `model` フィールドに返る
- `cache_creation_input_tokens` 相当のフィールドは存在しない

Codex CLI (OpenAI が提供するターミナルエージェント) が `usage` データをどこに記録するかは
未調査。CLI のセッション jsonl が存在するか不明。

---

### 1.3 opencode — 未調査

opencode (sst.dev が開発するオープンソースのターミナル AI コードエディタ) は
Anthropic / OpenAI / Google など複数のプロバイダーをバックエンドにサポートする。
Usage の形式はバックエンドプロバイダーに依存する。

opencode 固有の usage 記録形式 (独自ログファイルの有無・形式等) は未調査。
**opencode adapter 実装者が実機で確認すること。**

---

## 2. HarnessAdapter.Usage() 返却型 (確定)

### 設計方針

「共通 fixed フィールド + harness 固有 JSON blob ハイブリッド」を採用する。

**採用理由**:
- fixed フィールドで共通ユースケース (コスト概算・ダッシュボード集計) をカバーできる
- `Extra` で前方互換を確保し、Claude 固有フィールド (cache tier 別内訳・inference_geo 等) や
  将来の新 harness 固有データを schema migration なしで保存できる
- 純粋 JSON 1 列方式は SQL 集計が煩雑になる (SQLite の `json_extract` は使えるが遅く、
  また集計クエリのポータビリティを下げる)

### 確定 Go 型

```go
package adapters

import "encoding/json"

// Usage holds token consumption metrics for a completed job.
// Fixed fields cover the common denominator across harnesses.
// Extra holds harness-specific data that does not fit the fixed model.
type Usage struct {
    // Model is the model identifier used for this job (e.g. "claude-opus-4-8").
    Model string `json:"model,omitempty"`

    // InputTokens is the number of uncached input tokens consumed.
    // For Claude: message.usage.input_tokens (excludes cache reads and cache creation).
    // For OpenAI: usage.prompt_tokens minus cached portion.
    InputTokens int64 `json:"input_tokens"`

    // OutputTokens is the number of generated output tokens.
    OutputTokens int64 `json:"output_tokens"`

    // CacheCreationTokens is the number of tokens written to the prompt cache.
    // Zero for harnesses that do not support prompt caching.
    // For Claude: sum of cache_creation_input_tokens across all messages.
    CacheCreationTokens int64 `json:"cache_creation_tokens,omitempty"`

    // CacheReadTokens is the number of tokens served from the prompt cache.
    // Zero for harnesses that do not support prompt caching.
    // For Claude: sum of cache_read_input_tokens across all messages.
    CacheReadTokens int64 `json:"cache_read_tokens,omitempty"`

    // Extra holds harness-specific data not captured by the fixed fields above.
    // The claude adapter uses this to store a ClaudeUsageExtra struct (JSON-encoded).
    // Nil when no additional data is available.
    Extra json.RawMessage `json:"extra,omitempty"`
}
```

### Claude adapter が Extra に格納する型 (参考)

Phase 3 実装時の claude adapter が Extra に格納する予定の構造体:

```go
// ClaudeUsageExtra holds Claude-specific usage fields stored in Usage.Extra.
type ClaudeUsageExtra struct {
    // Cache1hCreationTokens / Cache5mCreationTokens break down CacheCreationTokens
    // by cache tier (ephemeral_1h vs ephemeral_5m).
    Cache1hCreationTokens int64 `json:"cache_1h_creation_tokens,omitempty"`
    Cache5mCreationTokens int64 `json:"cache_5m_creation_tokens,omitempty"`

    // ServerWebSearchRequests / ServerWebFetchRequests are server-side tool use counts.
    ServerWebSearchRequests int `json:"server_web_search_requests,omitempty"`
    ServerWebFetchRequests  int `json:"server_web_fetch_requests,omitempty"`

    // ServiceTier is the billing tier (e.g. "standard").
    ServiceTier string `json:"service_tier,omitempty"`

    // InferenceGeo is the inference execution geography (e.g. "not_available").
    InferenceGeo string `json:"inference_geo,omitempty"`
}
```

---

## 3. jobs テーブル schema 方針 (確定)

### 選択肢と評価

| 方針 | 利点 | 欠点 |
|---|---|---|
| A: 最大公約数 fixed columns のみ | SQL 集計が単純。クエリが速い | 新 harness/フィールドごとに migration が必要。harness 固有データを捨てる |
| B: 単一 JSON 列 | 追加フィールドに migration 不要 | `json_extract` 多用。集計クエリが遅く書きにくい |
| **C: Hybrid (fixed + JSON)** | 集計用 fixed column + 固有データ保存の両立。前方互換 | 若干設計が複雑 |

### 採用: Hybrid (C)

Phase 3 で追加する jobs テーブルの新カラム (方針のみ、**migration はこの文書に含まない**):

```sql
-- Phase 3 で追加するカラム (この SQL は方針記述のみ。実際の migration は Phase 3 で作成)
ALTER TABLE jobs ADD COLUMN input_tokens         INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN output_tokens        INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN cache_creation_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN cache_read_tokens    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN model               TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN usage_extra         TEXT NOT NULL DEFAULT '';
```

**カラム意味対応**:

| jobs カラム | Usage フィールド |
|---|---|
| `input_tokens` | `Usage.InputTokens` |
| `output_tokens` | `Usage.OutputTokens` |
| `cache_creation_tokens` | `Usage.CacheCreationTokens` |
| `cache_read_tokens` | `Usage.CacheReadTokens` |
| `model` | `Usage.Model` |
| `usage_extra` | `Usage.Extra` (JSON 文字列) |

### 選択理由

1. **集計クエリの利便性**: `SUM(input_tokens)`, `SUM(output_tokens)` など、コストダッシュボードで
   頻出するクエリを固定カラムで直接書ける。`json_extract` を使わなくてよい。

2. **前方互換**: `usage_extra` TEXT に JSON を格納するので、新 harness / 新フィールドが
   生じても既存カラムを変えずに対応できる。

3. **harness 固有データの損失を防ぐ**: Claude の cache tier 別内訳や inference_geo など、
   billing や将来の分析に使う可能性があるデータを `usage_extra` で保持できる。

4. **DEFAULT 0 / ''**: 既存レコード (usage 未記録の旧ジョブ) が自然に 0 になる。
   `SUM` の際に NULL 処理が不要。

---

## 4. Phase 3 着手のための前提条件

本ドキュメントが確定することで、Phase 3 は以下の作業に直接入れる状態になる:

1. **jobs テーブル migration 作成**: `0028_add_jobs_usage.sql` (上記方針に基づく)
2. **claude adapter の Usage() 実装**:
   - jsonl 読み取り経路の確定 (直接アクセス vs `run-agent.py` で `usage.json` 書き出し)
   - `Usage` 集計ロジックの実装
3. **HarnessAdapter インタフェースへの組み込み**: `agent-aware-boid.md` 設計判断 5 の
   インタフェーススケッチに `Usage()` の返却型として `adapters.Usage` を使う
4. **Web UI 集計表示**: `SUM(input_tokens)`, `SUM(output_tokens)`, `SUM(cache_creation_tokens)`,
   `SUM(cache_read_tokens)` をタスク / プロジェクト単位で集計して表示

---

## 付録: 調査に使ったコマンド

```bash
# usage フィールドを持つ jsonl の特定
grep -l '"usage"' ~/.claude/projects/-home-nosen-src-github-com-novshi-tech-boid/*.jsonl

# usage フィールドの内容確認
grep '"usage"' <session>.jsonl | head -1 | python3 -c "
import sys,json; data=json.loads(sys.stdin.read())
print(json.dumps(data.get('message',{}).get('usage',{}), indent=2))
"

# 全セッションで出現した usage キーの列挙
grep '"usage"' ~/.claude/projects/-home-nosen-src-github-com-novshi-tech-boid/*.jsonl | python3 -c "
import sys, json
all_keys = set()
for line in sys.stdin:
    idx = line.find('{')
    if idx < 0: continue
    try:
        data = json.loads(line[idx:])
        usage = data.get('message', {}).get('usage', {})
        if usage:
            all_keys.update(usage.keys())
    except: pass
print('All usage keys seen:', sorted(all_keys))
"
```

証跡として確認したセッション ID の例:
- `1b9e9210-6ed6-4ddc-a3ac-887138e1d0ce` (boid プロジェクト、claude-opus-4-8)
- `96e60d3a-1390-45a4-93cc-cb9d5990406f` (boid worktree タスク、claude-sonnet-4-6)
- boid worktree 配下のサブエージェント jsonl 複数
