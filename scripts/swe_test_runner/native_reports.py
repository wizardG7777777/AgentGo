"""Common native test facts. Counts describe the driver's named test units."""
from __future__ import annotations

import collections
import json
from pathlib import Path
import re
import xml.etree.ElementTree as ET

from suite_config import project_path

SCHEMA = "agentgo.native-test-report/v1"


def report(outcomes, *, engine, exit_code, errors=(), expected=None, baseline_added_files=()):
    # Selecting a node in a module that cannot import makes pytest report both
    # a collection failure and "found no collectors" (exit 4). Require the
    # reporter's collection event; a bad selector alone remains a usage error.
    collection_failure = engine == "pytest" and exit_code in {2, 4} and any(item.get("phase") == "collection" for item in errors)
    if exit_code not in {0, 1} and not collection_failure:
        raise ValueError("test process did not finish normally")
    states = dict(outcomes)
    if any(value not in {"pass", "fail", "skip", "xfail", "xpass", "error", "run"} for value in states.values()):
        raise ValueError("unknown test outcome")
    inventory = sorted(set(expected) if expected is not None else states)
    if set(states) - set(inventory):
        raise ValueError("test outcomes outside the collected inventory")
    missing = sorted(set(inventory) - {name for name, state in states.items() if state != "run"})
    explicit = {item["nodeid"] for item in errors}
    failures = [{"nodeid": name, "phase": "test"} for name, value in states.items()
                if value == "fail" or (value == "error" and name not in explicit)]
    failures.extend(dict(item) for item in errors)
    counts = collections.Counter(states.values())
    empty_baseline = bool(baseline_added_files) and not states and not inventory and not failures and exit_code == 0
    if exit_code == 0 and (failures or missing or counts["pass"] == 0) and not empty_baseline:
        raise ValueError("successful exit conflicts with test evidence")
    if exit_code != 0 and not failures:
        raise ValueError("failed exit without test/build evidence")
    value = {"schema": SCHEMA, "test_engine": engine, "count_semantics": engine + "/v1",
            "tests": len(inventory), "collected": len(inventory), "collected_nodeids": inventory,
            "test_outcomes": states, "case_outcomes": dict(states), "passed": counts["pass"], "failed": counts["fail"],
            "errors": len(errors) + sum(value == "error" and name not in explicit for name, value in states.items()), "skipped": counts["skip"],
            "xfailed": counts["xfail"], "xpassed": counts["xpass"], "not_run": missing,
            "failure_events": failures, "phase_errors": {}, "exit_code": exit_code}
    if empty_baseline:
        value.update(empty_before_test_patch=True, absent_new_test_files=list(baseline_added_files), tests_executed=False)
    return value


def merge(reports, engine):
    states, cases, inventory, failures = {}, {}, set(), []
    for item in reports:
        if set(states) & set(item["test_outcomes"]):
            raise ValueError("duplicate test identity across native commands")
        states.update(item["test_outcomes"])
        cases.update(item.get("case_outcomes", item["test_outcomes"]))
        inventory.update(item["collected_nodeids"])
        failures.extend(item["failure_events"])
    errors = [item for item in failures if item["phase"] != "test"]
    result = report(states, engine=engine, exit_code=int(any(r["exit_code"] for r in reports)),
                    errors=errors, expected=inventory)
    result["failure_events"] = failures
    result["case_outcomes"] = cases
    return result


def vitest_paths(worktree, selectors, *, baseline_added_files=()):
    """Resolve declared files/directories before Vitest's substring filtering."""
    root = Path(worktree).resolve(strict=True)
    selected = []
    for value in selectors:
        original = root / project_path(value)
        candidate = original.resolve()
        if not candidate.is_relative_to(root) or candidate.is_relative_to(root / ".agentgo"):
            raise ValueError("Vitest selection is outside the subject source")
        if not candidate.exists() and not original.is_symlink() and value in baseline_added_files:
            continue
        candidate = candidate.resolve(strict=True)
        if not candidate.is_file() and not candidate.is_dir():
            raise ValueError("Vitest selection is not a file or directory")
        selected.append((candidate, candidate.is_dir()))
    if not selected and not (selectors and set(selectors) <= set(baseline_added_files)):
        raise ValueError("Vitest selection is empty")
    return selected


def vitest(path, worktree, exit_code, *, selected=None):
    data = json.loads(Path(path).read_text(encoding="utf-8"))
    states, errors = {}, []
    occurrences = collections.Counter()
    root = Path(worktree).resolve()
    for file in data.get("testResults", []):
        filename = Path(file["name"]).resolve()
        if not filename.is_relative_to(root):
            raise ValueError("Vitest report names tests outside the checkout")
        if filename.is_relative_to(root / ".agentgo"):
            raise ValueError("Vitest collected framework-internal test copies")
        if selected is not None and not any(filename == target or directory and filename.is_relative_to(target)
                                            for target, directory in selected):
            raise ValueError("Vitest report names tests outside the declared selection")
        relative = filename.relative_to(root).as_posix()
        for assertion in file.get("assertionResults", []):
            identity = relative + "::" + assertion["fullName"]
            # Vitest permits repeated titles and emits assertionResults in
            # declaration order. Preserve each occurrence instead of collapsing it.
            occurrences[identity] += 1
            identity += "::occurrence=" + str(occurrences[identity])
            status = assertion["status"]
            mapping = {"passed": "pass", "failed": "fail", "pending": "skip", "skipped": "skip", "todo": "skip", "disabled": "skip"}
            if status not in mapping:
                raise ValueError("unknown Vitest assertion status")
            states[identity] = mapping[status]
        if file.get("status") == "failed" and not any(name.startswith(relative + "::") and state == "fail" for name, state in states.items()):
            errors.append({"nodeid": relative + "::<collection>", "phase": "collection"})
    if data.get("numTotalTests") != len(states):
        raise ValueError("Vitest aggregate count differs from named test results")
    counts = collections.Counter(states.values())
    for key, state in (("numPassedTests", "pass"), ("numFailedTests", "fail")):
        if data.get(key) != counts[state]:
            raise ValueError("Vitest aggregate outcomes disagree with test results")
    return report(states, engine="vitest", exit_code=exit_code, errors=errors)


def libtest_inventory(text):
    names = []
    for line in text.splitlines():
        if line.endswith(": test"):
            names.append(line[:-6])
    if len(names) != len(set(names)):
        raise ValueError("duplicate Rust test names")
    return names


def libtest(text, names, prefix, exit_code):
    states = {}
    for line in text.splitlines():
        match = re.fullmatch(r"test (.+?)(?: - should panic)? \.\.\. (ok|FAILED|ignored(?:,.*)?)", line.strip())
        if not match:
            continue
        name, value = match.groups()
        if name not in names or name in states:
            raise ValueError("Rust test output conflicts with its binary inventory")
        states[name] = "pass" if value == "ok" else "fail" if value == "FAILED" else "skip"
    ended = re.search(r"test result: (?:ok|FAILED)\. (\d+) passed; (\d+) failed; (\d+) ignored;", text)
    if not ended:
        raise ValueError("Rust test output has no terminal summary")
    counts = collections.Counter(states.values())
    if tuple(map(int, ended.groups())) != (counts["pass"], counts["fail"], counts["skip"]):
        raise ValueError("Rust test summary disagrees with individual outcomes")
    return report({prefix + "::" + name: state for name, state in states.items()}, engine="libtest",
                  exit_code=int(exit_code != 0), expected=[prefix + "::" + name for name in names])


def junit(path, *, engine, exit_code):
    root = ET.parse(path).getroot()
    states = {}
    for case in root.iter("testcase"):
        name = (case.get("classname", "") + "::" + case.get("name", "")).strip(":")
        if not name or name in states:
            raise ValueError("invalid or duplicate JUnit test identity")
        states[name] = ("fail" if case.find("failure") is not None else "error" if case.find("error") is not None
                        else "skip" if case.find("skipped") is not None else "pass")
    suites = [root] if root.tag == "testsuite" else list(root.findall("testsuite"))
    if suites and all(suite.get("tests") is not None for suite in suites):
        if sum(int(suite.get("tests")) for suite in suites) != len(states):
            raise ValueError("JUnit aggregate count differs from named test results")
    return report(states, engine=engine, exit_code=exit_code)


def compare(clean, expected, current, required, expected_cases=None):
    old_failures = {(row["nodeid"], row["phase"]) for row in clean["failure_events"]}
    failures = {(row["nodeid"], row["phase"]) for row in current["failure_events"]}
    old_cases = clean.get("case_outcomes", clean["test_outcomes"])
    cases = current.get("case_outcomes", current["test_outcomes"])
    lost = sorted(name for name, value in old_cases.items() if value == "pass" and cases.get(name) != "pass")
    targets = sorted(name for name in required if cases.get(name) != "pass")
    added = [{"nodeid": name, "phase": phase} for name, phase in sorted(failures - old_failures)]
    same = sorted(expected) == sorted(current["collected_nodeids"])
    checks = {"same_expected_collection": same, "required_targets_passed": not targets,
              "original_passes_preserved": not lost, "no_new_failures": not added,
              "no_missing_results": not current["not_run"],
              "same_expected_cases": expected_cases is None or set(cases) == set(expected_cases)}
    return all(checks.values()), {"checks": checks, "added_failures": added,
                                 "lost_passes": lost, "unresolved_targets": targets}


def project_baseline(clean, reference_cases):
    """Official test patches may rename/parametrize tests before source repair."""
    expected = set(reference_cases)
    states = clean.get("case_outcomes", clean["test_outcomes"])
    projected = dict(clean)
    projected["case_outcomes"] = {name: value for name, value in states.items() if name in expected}
    projected["test_outcomes"] = {name: value for name, value in clean["test_outcomes"].items() if name in expected}
    projected["failure_events"] = [row for row in clean["failure_events"] if row["nodeid"] in expected]
    return projected, sorted(set(states) - expected)
