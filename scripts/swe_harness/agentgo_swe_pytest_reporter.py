"""AgentGo SWE pytest 阶段计数插件。

pytest 的 call failure 与 teardown error 可以属于同一个逻辑测试，因此 JUnit
suite 顶层计数不能用减法恢复 passed。本插件直接消费 pytest 的阶段报告，生成
机器可读、允许重叠的权威计数。
"""

from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any


REPORT_SCHEMA = "agentgo.pytest-phase-report/v1"
COUNT_SEMANTICS = "pytest-phase-overlap/v1"
REPORT_PATH_ENV = "AGENTGO_SWE_PYTEST_REPORT"


class PhaseCounter:
    """按逻辑 nodeid 统计 call 结果，并按事件统计阶段错误。"""

    def __init__(self) -> None:
        self.passed: set[str] = set()
        self.failed: set[str] = set()
        self.skipped: set[str] = set()
        self.xfailed: set[str] = set()
        self.xpassed: set[str] = set()
        self.phase_errors = {"collection": 0, "setup": 0, "teardown": 0}

    @staticmethod
    def _nodeid(report: Any) -> str:
        return str(getattr(report, "nodeid", "") or "")

    def record_runtest(self, report: Any) -> None:
        nodeid = self._nodeid(report)
        when = str(getattr(report, "when", "") or "")
        wasxfail = bool(getattr(report, "wasxfail", False))

        if bool(getattr(report, "skipped", False)):
            (self.xfailed if wasxfail else self.skipped).add(nodeid)
            return

        if when == "call":
            if bool(getattr(report, "passed", False)):
                (self.xpassed if wasxfail else self.passed).add(nodeid)
            elif bool(getattr(report, "failed", False)):
                # strict xpass 由 pytest 作为 call failure 处理，保持 failed 权威。
                self.failed.add(nodeid)
            return

        if when in {"setup", "teardown"} and bool(getattr(report, "failed", False)):
            self.phase_errors[when] += 1

    def record_collect(self, report: Any) -> None:
        if bool(getattr(report, "failed", False)):
            self.phase_errors["collection"] += 1
        elif bool(getattr(report, "skipped", False)):
            self.skipped.add(self._nodeid(report))

    def result(self, collected: int) -> dict[str, Any]:
        errors = sum(self.phase_errors.values())
        return {
            "schema": REPORT_SCHEMA,
            "count_semantics": COUNT_SEMANTICS,
            "collected": int(collected),
            "passed": len(self.passed),
            "failed": len(self.failed),
            "errors": errors,
            "skipped": len(self.skipped),
            "xfailed": len(self.xfailed),
            "xpassed": len(self.xpassed),
            "phase_errors": dict(self.phase_errors),
        }


_COUNTER = PhaseCounter()


def pytest_runtest_logreport(report: Any) -> None:
    _COUNTER.record_runtest(report)


def pytest_collectreport(report: Any) -> None:
    _COUNTER.record_collect(report)


def _atomic_json(path: Path, value: object) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    temporary.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    os.replace(temporary, path)


def pytest_sessionfinish(session: Any, exitstatus: Any) -> None:
    del exitstatus
    raw_path = os.environ.get(REPORT_PATH_ENV, "").strip()
    if not raw_path:
        return
    _atomic_json(Path(raw_path), _COUNTER.result(int(getattr(session, "testscollected", 0))))
