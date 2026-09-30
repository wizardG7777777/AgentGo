"""Root-session-only reporter, including when pytest itself is the subject."""
import json
import importlib.util
import os
from pathlib import Path

_session = None
_states = {}
_phases = []
_inventory = []
_collecting = False


def pytest_sessionstart(session):
    global _session, _collecting
    if _session is None:
        _session = session
        _collecting = True


def pytest_collection_finish(session):
    global _collecting
    if session is _session:
        _inventory[:] = [item.nodeid for item in session.items]
        _collecting = False


def pytest_runtest_logreport(report):
    # Nested pytester sessions do not load this explicitly requested plugin.
    if report.nodeid not in _inventory:
        return
    if report.when == "call":
        value = "xfail" if report.skipped and hasattr(report, "wasxfail") else "xpass" if report.passed and hasattr(report, "wasxfail") else "pass" if report.passed else "fail" if report.failed else "skip"
        _states[report.nodeid] = value
    elif report.skipped:
        _states[report.nodeid] = "skip"
    elif report.failed:
        _states[report.nodeid] = "error"
        _phases.append({"nodeid": report.nodeid, "phase": report.when})


def pytest_collectreport(report):
    if _collecting and report.failed:
        _phases.append({"nodeid": report.nodeid or "<collection>", "phase": "collection"})


def pytest_sessionfinish(session, exitstatus):
    if session is not _session:
        return
    path = Path(os.environ["AGENTGO_NATIVE_PYTEST_REPORT"])
    subject = os.environ.get("AGENTGO_NATIVE_PYTHON_SUBJECT", "")
    spec = importlib.util.find_spec(subject) if subject else None
    value = {"schema": "agentgo.native-pytest-report/v1", "pid": os.getpid(),
             "outcomes": _states, "inventory": _inventory, "errors": _phases,
             "exit_code": int(exitstatus), "subject_file": getattr(spec, "origin", None)}
    path.write_text(json.dumps(value, ensure_ascii=False) + "\n", encoding="utf-8")
