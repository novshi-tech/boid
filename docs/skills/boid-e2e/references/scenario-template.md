# シナリオ作成テンプレート

## ディレクトリ構成

```
e2e/scenarios/<scenario-name>/
├── scenario.sh                    # シナリオスクリプト（必須）
├── requires-sandbox               # サンドボックス必要な場合のみ（空ファイル）
└── workspace/
    └── app/
        └── .boid/
            └── project.yaml       # テスト用プロジェクト定義（必須）
```

fixture kit（カスタム hooks/gates）が必要な場合:

```
e2e/fixtures/kits/github.com/novshi-tech/boid-kits/<kit-name>/
├── kit.yaml
├── hooks/
│   └── <hook-id>.sh
└── gates/
    └── <gate-id>.sh
```

## project.yaml テンプレート

### 最小構成（hook/gate なし）

参照: `e2e/scenarios/project-smoke/workspace/app/.boid/project.yaml`

```yaml
id: my-scenario
name: My Scenario
task_behaviors:
  smoke:
    name: Smoke
hooks: []
gates: []
```

### kit を使う構成（verify gate 付き）

参照: `e2e/scenarios/readonly-hook-gate/workspace/app/.boid/project.yaml`

```yaml
id: my-scenario
name: My Scenario
kits:
  - github.com/novshi-tech/boid-kits/<kit-name>
task_behaviors:
  dev:
    name: Dev
hooks: []
gates: []
```

### kit.yaml テンプレート

```yaml
env:
  E2E_STATE_DIR: ${E2E_STATE_DIR}   # 環境変数を注入する場合
hooks:
  - id: my-hook
    on: executing                    # 起動するステータス
gates:
  - id: my-gate
    on: verifying
    traits:
      consumes: [artifact]           # アクセスする trait を宣言
```

**`on` に指定できるステータス**: `executing`, `verifying`, `reworking`

## scenario.sh テンプレート

### 基本パターン（プロジェクト登録 → タスク作成 → 検証）

参照: `e2e/scenarios/project-smoke/scenario.sh`

```bash
#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"

# 1. プロジェクト登録
e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"

# 2. タスク作成
task_create_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: my-scenario
title: My Test Task
behavior: smoke
YAML
)"
printf '%s\n' "$task_create_output"
task_id="$(printf '%s\n' "$task_create_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$task_id" ]] || e2e_fail "failed to parse task id"

# 3. タスク開始
e2e_run "$E2E_BIN_DIR/boid" action send --task "$task_id" --type start

# 4. 完了を待機して検証
task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 15s --interval 100ms "$task_id" done)"
printf '%s\n' "$task_json"
e2e_assert_contains "$task_json" '"status":"done"'
```

### hook/gate 同期パターン（ファイルで release 制御）

参照: `e2e/scenarios/readonly-hook-gate/scenario.sh`

```bash
# hook スクリプト側: ファイルが現れるまでブロック
while [[ ! -f ".boid/release-my-hook" ]]; do sleep 0.05; done

# シナリオ側: hook が起動したことを確認してから release
"$E2E_BIN_DIR/boid-e2e" wait-job-count "$task_id" 1
"$E2E_BIN_DIR/boid-e2e" assert-job-role-count "$task_id" hook 1
touch "$PROJECT_DIR/.boid/release-my-hook"
```

### payload override でタスク作成するパターン

参照: `e2e/scenarios/instructions-routing/scenario.sh`

```bash
task_create_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: my-scenario
title: My Task
behavior: dev
payload:
  instructions:
    executor:
      type: execution
      consumer: claude-code
      message: "implement the feature"
YAML
)"
```

## hook/gate スクリプトのテンプレート

### hook スクリプト（artifact を出力）

```bash
#!/usr/bin/env bash
set -euo pipefail

# ブロッキング（任意）
while [[ ! -f ".boid/release-my-hook" ]]; do sleep 0.05; done

# payload_patch を $HOME/.boid/output/ に出力
mkdir -p "$HOME/.boid/output"
cat > "$HOME/.boid/output/payload_patch.yaml" <<'EOF'
{"payload_patch":{"artifact":{"result":"done"}}}
EOF
```

### gate スクリプト（verification を stdout に出力）

```bash
#!/usr/bin/env bash
set -euo pipefail

# stdout に JSON を出力する（payload_patch ではなく直接 stdout）
cat <<'EOF'
{"payload_patch":{"verification":{"findings":[{"message":"all checks passed","status":"resolved"}]}}}
EOF
```
