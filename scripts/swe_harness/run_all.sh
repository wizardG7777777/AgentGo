#!/bin/bash
# 串行 prepare → run → judge；任务正确性与架构正确性分别汇总。
set -euo pipefail

TIMEOUT="${1:-1200}"
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
TESTBED="${SWE_TESTBED:-/tmp/agentgo-swe}"
TASKS_FILE="${SWE_TASKS_FILE:-$TESTBED/harness/tasks.csv}"
mkdir -p "$TESTBED/runs"

bash "$SCRIPT_DIR/preflight_probe.sh"
BATCH_START=$(date +%s)
echo "$BATCH_START" > "$TESTBED/runs/.batch_start"

tail -n +2 "$TASKS_FILE" | while IFS=, read -r tid sha tfiles title; do
  echo "===== $(date +%H:%M:%S) $tid: $title"
  bash "$SCRIPT_DIR/prepare_task.sh" "$tid"
  bash "$SCRIPT_DIR/run_task.sh" "$tid" "$TIMEOUT"
  bash "$SCRIPT_DIR/judge.sh" "$tid"
done

python3 "$SCRIPT_DIR/harness.py" summarize \
  --runs "$TESTBED/runs" --batch-start "$BATCH_START" --output "$TESTBED/runs/summary.json"
