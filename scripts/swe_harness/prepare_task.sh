#!/bin/bash
# 准备提交态 golden test patch 与全量红态基线。
set -euo pipefail

TASK_ID="$1"
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
TESTBED="${SWE_TESTBED:-/tmp/agentgo-swe}"
TASKS_FILE="${SWE_TASKS_FILE:-$TESTBED/harness/tasks.csv}"
FLASK_REPO="${SWE_FLASK_REPO:-/Users/yanchenyu/Documents/PythonProjects/flask}"

row=$(grep "^$TASK_ID," "$TASKS_FILE") || { echo "未知考题: $TASK_ID" >&2; exit 1; }
sha=$(echo "$row" | cut -d, -f2)
tfiles=$(echo "$row" | cut -d, -f3)

bash "$SCRIPT_DIR/setup_task.sh" "$TASK_ID" "$sha^" "$FLASK_REPO"
WT="$TESTBED/worktrees/$TASK_ID"
RUN="$TESTBED/runs/$TASK_ID"
mkdir -p "$RUN"
cd "$WT"

git -C "$FLASK_REPO" show "$sha" -- tests/ | git apply -
git add -A -- tests/
git -c user.name=agentgo-swe -c user.email=swe@harness.local \
  commit -q -m "harness: 预置期望行为测试（考题 ${TASK_ID}）" -- tests/

red_out=$(.venv/bin/python -m pytest $tfiles -q 2>&1 | tail -1) || true
echo "$red_out"
echo "$red_out" | grep -qE 'failed|error' || { echo "未确认红状态，考题准备失败" >&2; exit 1; }

base_out=$(.venv/bin/python -m pytest -q 2>&1 | tail -3) || true
bfailed=$(echo "$base_out" | grep -oE '[0-9]+ failed' | awk '{s+=$1} END {print s+0}' || true)
berrors=$(echo "$base_out" | grep -oE '[0-9]+ error' | awk '{s+=$1} END {print s+0}' || true)
bpassed=$(echo "$base_out" | grep -oE '[0-9]+ passed' | awk '{s+=$1} END {print s+0}' || true)
python3 - "$bpassed" "$bfailed" "$berrors" "$RUN/baseline.json" <<'EOF'
import json
import sys
with open(sys.argv[4], "w", encoding="utf-8") as handle:
    json.dump({"passed": int(sys.argv[1]), "failed": int(sys.argv[2]),
               "errors": int(sys.argv[3]), "note": "base 红态基线（agent 运行前全量 pytest）"},
              handle, ensure_ascii=False, indent=2)
EOF
echo "$sha" > "$RUN/fix_sha"
echo "考题 $TASK_ID 就绪：passed=$bpassed failed=$bfailed errors=$berrors"
