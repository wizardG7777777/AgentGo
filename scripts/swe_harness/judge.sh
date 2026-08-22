#!/bin/bash
# Flask 全量 pytest 是任务正确性 oracle；架构正确性由 result.json 独立记录。
set -euo pipefail

TASK_ID="$1"
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
TESTBED="${SWE_TESTBED:-/tmp/agentgo-swe}"
TASKS_FILE="${SWE_TASKS_FILE:-$TESTBED/harness/tasks.csv}"
FLASK_REPO="${SWE_FLASK_REPO:-/Users/yanchenyu/Documents/PythonProjects/flask}"
WT="$TESTBED/worktrees/$TASK_ID"
RUN="$TESTBED/runs/$TASK_ID"
mkdir -p "$RUN"
cd "$WT"

out=$(.venv/bin/python -m pytest -q 2>&1 | tail -3) || true
echo "$out"
failed=$(echo "$out" | grep -oE '[0-9]+ failed' | awk '{s+=$1} END {print s+0}' || true)
errors=$(echo "$out" | grep -oE '[0-9]+ error' | awk '{s+=$1} END {print s+0}' || true)
passed=$(echo "$out" | grep -oE '[0-9]+ passed' | awk '{s+=$1} END {print s+0}' || true)
verdict="failed"
if [ "$failed" -eq 0 ] && [ "$errors" -eq 0 ] && [ "$passed" -gt 0 ]; then verdict="resolved"; fi

row=$(grep "^$TASK_ID," "$TASKS_FILE")
sha=$(echo "$row" | cut -d, -f2)
test_files=$(git -C "$FLASK_REPO" show "$sha" --name-only --format= -- tests/)
tampered=0
tampered_files=""
for f in $test_files; do
  if git -C "$FLASK_REPO" cat-file -e "$sha:$f" 2>/dev/null; then
    expect=$(git -C "$FLASK_REPO" show "$sha:$f" | shasum -a 256 | awk '{print $1}')
    actual="MISSING"
    [ ! -f "$f" ] || actual=$(shasum -a 256 "$f" | awk '{print $1}')
    [ "$expect" = "$actual" ] || { tampered=1; tampered_files="$tampered_files $f"; }
  else
    [ ! -e "$f" ] || { tampered=1; tampered_files="$tampered_files $f(应已删除)"; }
  fi
done
if [ "$tampered" = 1 ]; then
  verdict="test_tampered"
  echo "测试篡改检测:$tampered_files"
fi

excl=( ':!.venv' ':!.agentgo' )
for f in $test_files; do excl+=( ":!$f" ); done
git reset -q 2>/dev/null || true
git add -A -- . "${excl[@]}" 2>/dev/null
git diff --cached -- . "${excl[@]}" > "$RUN/model.patch"
patch_lines=$(wc -l < "$RUN/model.patch" | tr -d ' ')

python3 - "$verdict" "$passed" "$failed" "$errors" "$patch_lines" "$tampered" \
  "$RUN/judge.json" "$RUN/baseline.json" <<'EOF'
import json
import os
import sys
report = {"verdict": sys.argv[1], "passed": int(sys.argv[2]), "failed": int(sys.argv[3]),
          "errors": int(sys.argv[4]), "patch_lines": int(sys.argv[5]), "tampered": sys.argv[6] == "1"}
if os.path.isfile(sys.argv[8]):
    try:
        base = json.load(open(sys.argv[8], encoding="utf-8"))
        report["baseline"] = {key: base[key] for key in ("passed", "failed", "errors")}
        if report["verdict"] == "failed":
            if report["failed"] == base["failed"] and report["errors"] == base["errors"]:
                report["red_note"] = "红态与基线完全一致：补丁未造成新增破坏（但也未修复）"
            elif report["failed"] + report["errors"] > base["failed"] + base["errors"]:
                report["red_note"] = "红态重于基线：补丁引入了新增破坏"
            else:
                report["red_note"] = "红态轻于基线：部分修复但未全绿"
    except (OSError, KeyError, json.JSONDecodeError):
        pass
with open(sys.argv[7], "w", encoding="utf-8") as handle:
    json.dump(report, handle, ensure_ascii=False, indent=2)
print(json.dumps(report, ensure_ascii=False))
EOF

python3 "$SCRIPT_DIR/harness.py" finalize --result "$RUN/result.json" --judge "$RUN/judge.json"
