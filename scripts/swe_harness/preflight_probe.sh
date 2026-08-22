#!/bin/bash
# 批次级真实 function-call 能力探针；纯文本 HTTP 200 不算通过。
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
AGENTGO_ROOT="${SWE_AGENTGO_ROOT:-$(cd "$SCRIPT_DIR/../.." && pwd)}"
KEY_VAR="SWE_API_KEY"
BASE_URL="${SWE_BASE_URL:-https://openrouter.ai/api/v1}"
MODEL="${SWE_MODEL:-openai/gpt-5.6-luna}"
PROTOCOL="${SWE_PROTOCOL:-responses}"

python3 "$SCRIPT_DIR/harness.py" probe \
  --base-url "$BASE_URL" \
  --model "$MODEL" \
  --protocol "$PROTOCOL" \
  --key-var "$KEY_VAR" \
  --timeout "${SWE_PROBE_TIMEOUT_SEC:-45}"

echo "typed function-call 活探针通过: ${BASE_URL%/} protocol=$PROTOCOL model=$MODEL"
