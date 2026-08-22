#!/bin/bash
# 为一道 Flask SWE 题准备物理隔离的独立克隆与时代锁定 venv。
set -euo pipefail

TASK_ID="$1"
BASE_COMMIT="$2"
FLASK_REPO="${3:-${SWE_FLASK_REPO:-/Users/yanchenyu/Documents/PythonProjects/flask}}"
TESTBED="${SWE_TESTBED:-/tmp/agentgo-swe}"
WT="$TESTBED/worktrees/$TASK_ID"

if [ -d "$WT" ]; then
  git -C "$FLASK_REPO" worktree remove --force "$WT" 2>/dev/null || rm -rf "$WT"
  git -C "$FLASK_REPO" worktree prune 2>/dev/null || true
fi

git clone --no-local --quiet "$FLASK_REPO" "$WT"
cd "$WT"
git checkout --quiet --detach "$BASE_COMMIT"

git for-each-ref --format='%(refname)' | while read -r ref; do
  git update-ref -d "$ref"
done
git reflog expire --expire=now --all
git gc --prune=now --quiet
git remote remove origin 2>/dev/null || true

uv sync --frozen --no-default-groups --group tests --python 3.13 >/dev/null
.venv/bin/python - <<'EOF'
import importlib.metadata as metadata
import sys
print(f"env-ok: flask={metadata.version('flask')} pytest={metadata.version('pytest')} py={sys.version.split()[0]}")
EOF
echo "worktree 就绪: $WT"
