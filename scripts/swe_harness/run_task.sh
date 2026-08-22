#!/bin/bash
# 启动 AgentGo、注入冻结 RunContract、按 typed outcome 收敛并采集脱敏结果。
set -euo pipefail

TASK_ID="$1"
TIMEOUT="${2:-1200}"
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
AGENTGO_ROOT="${AGENTGO_ROOT:-$(cd "$SCRIPT_DIR/../.." && pwd)}"
TESTBED="${SWE_TESTBED:-/tmp/agentgo-swe}"
WT="$TESTBED/worktrees/$TASK_ID"
RUN="$TESTBED/runs/$TASK_ID"
BIN="${AGENTGO_BIN:-$AGENTGO_ROOT/agentgo}"
PROMPT="${SWE_PROMPT_DIR:-$TESTBED/harness/prompts}/$TASK_ID.md"
KEY_VAR="${SWE_KEY_VAR:-AGENTGO_SWE_API_KEY}"
BASE_URL="${SWE_BASE_URL:-https://openrouter.ai/api/v1}"
MODEL="${SWE_MODEL:-deepseek/deepseek-v4-flash-0731}"

[ -d "$WT" ] || { echo "worktree 不存在: $WT（先跑 prepare_task.sh）" >&2; exit 1; }
[ -f "$PROMPT" ] || { echo "prompt 不存在: $PROMPT" >&2; exit 1; }
[ -n "${!KEY_VAR:-}" ] || { echo "错误: $KEY_VAR 未设置" >&2; exit 1; }
[ "$TIMEOUT" -ge 240 ] || { echo "错误: timeout 必须至少 240 秒" >&2; exit 1; }
mkdir -p "$RUN"
rm -f "$RUN/result.json" "$RUN/judge.json" "$RUN/snapshot.final.json" \
  "$RUN/monitor.json" "$RUN/run_contract.json" "$RUN/model.patch"

PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
TOKEN=$(python3 -c 'import secrets; print(secrets.token_hex(16))')

sed -e "s|__PROJECT_ROOT__|$WT|" -e "s|__PORT__|$PORT|" -e "s|__TOKEN__|$TOKEN|" \
    -e "s|__AGENTGO_ROOT__|$AGENTGO_ROOT|" -e "s|__BASE_URL__|$BASE_URL|" \
    -e "s|__MODEL__|$MODEL|" "$AGENTGO_ROOT/setting.swe-flask.yaml" > "$RUN/setting.yaml"

BASE="http://127.0.0.1:$PORT"
START=$(date +%s)
cd "$WT"
nohup "$BIN" -config "$RUN/setting.yaml" > "$RUN/agentgo.log" 2>&1 &
PID=$!
echo "agentgo pid=$PID port=$PORT"

ready=0
for _ in $(seq 1 90); do
  curl -sf "$BASE/healthz" >/dev/null 2>&1 && { ready=1; break; }
  kill -0 "$PID" 2>/dev/null || break
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "SPAWN_ERROR: healthz 未就绪，见 $RUN/agentgo.log" >&2
  kill -9 "$PID" 2>/dev/null || true
  exit 1
fi
grep -q '\[OK\].*function-call schema/arguments' "$RUN/agentgo.log" || {
  echo "SPAWN_ERROR: 产品 function-call probe 没有成功证据" >&2
  kill "$PID" 2>/dev/null || true
  exit 1
}

python3 "$SCRIPT_DIR/harness.py" inject \
  --base-url "$BASE" --token "$TOKEN" --prompt "$PROMPT" --task-id "$TASK_ID" \
  --timeout "$TIMEOUT" --contract "$RUN/run_contract.json"
RUN_ID=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["run_id"])' "$RUN/run_contract.json")

python3 "$SCRIPT_DIR/harness.py" monitor \
  --base-url "$BASE" --token "$TOKEN" --pid "$PID" --run-id "$RUN_ID" \
  --started-at "$START" --timeout "$TIMEOUT" --snapshot "$RUN/snapshot.final.json" \
  --output "$RUN/monitor.json" --poll 3 --grace 30

curl -sf -H "Authorization: Bearer $TOKEN" "$BASE/api/snapshot" \
  -o "$RUN/snapshot.final.json" 2>/dev/null || true
kill "$PID" 2>/dev/null || true
for _ in $(seq 1 20); do
  kill -0 "$PID" 2>/dev/null || break
  sleep 0.1
done
kill -9 "$PID" 2>/dev/null || true
wait "$PID" 2>/dev/null || true

python3 "$SCRIPT_DIR/harness.py" collect \
  --snapshot "$RUN/snapshot.final.json" --monitor "$RUN/monitor.json" \
  --project-root "$WT" --run-id "$RUN_ID" --startup-probe-passed \
  --output "$RUN/result.json"
