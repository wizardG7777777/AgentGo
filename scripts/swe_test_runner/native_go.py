"""Native Go test adapter. Shared runner.py owns provider, Run, Graph and process orchestration."""
from __future__ import annotations

import collections
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import contextlib
import processes as managed_processes
import storage as resource_storage
import uuid

from suite_config import project_path
from test_identity import compare_failures

REPORT_SCHEMA = "agentgo.go-test-report/v1"
COUNT_SEMANTICS = "go-top-level-tests/v1"
PROTECTED_ROOT = {"go.mod", "go.sum", "go.work", "go.work.sum", ".gitignore", ".gitattributes", "Makefile"}
IGNORED_DIRS = {".git", ".agentgo", ".venv", "__pycache__"}


def now():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def write_json(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    temporary.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    temporary.replace(path)


def go_environment(config, *, tests=False):
    environment = os.environ.copy()
    environment.update(GOTOOLCHAIN=config.suite.toolchain, GOWORK="off", GOFLAGS="", CGO_ENABLED="0")
    environment.pop("GOCOVERDIR", None)
    environment.pop("GOEXPERIMENT", None)
    if tests:
        for name in list(environment):
            if name.startswith(("SWE_", "AGENTGO_")):
                del environment[name]
    return environment


def run(config, worktree, argv, *, timeout=None, check=True):
    if resource_storage.active() is not None:
        return managed_processes.run(argv, cwd=worktree, env=go_environment(config, tests=True),
                                     timeout=timeout or 1200, check=check, label="go-command")
    result = subprocess.run(argv, cwd=worktree, env=go_environment(config, tests=True),
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
    if check and result.returncode:
        raise RuntimeError(f"Go environment command failed: {argv[:3]} exit={result.returncode}")
    return result


def input_identity(worktree):
    root = worktree.resolve(strict=True)
    files = {}
    # Upstream tests may create ignored output files (Gin writes ./test).
    # Retain tracked assets and untracked inputs, plus compiler/test inputs even
    # when a .gitignore rule would otherwise hide them. Do not mask source edits.
    visible = None
    if (root / ".git").exists():
        listed = managed_processes.run(["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"],
                                       cwd=root, check=True, timeout=60, label="go-source-inventory")
        visible = set(listed.stdout.decode("utf-8").split("\0"))
    for parent, directories, names in os.walk(root, followlinks=False):
        directories[:] = sorted(name for name in directories if name not in IGNORED_DIRS)
        for name in directories + names:
            path = Path(parent) / name
            if path.is_symlink() or not path.resolve().is_relative_to(root):
                raise ValueError("Go test input cannot escape through a symlink")
        for name in sorted(names):
            if name.endswith((".pyc", ".pyo")):
                continue
            path = Path(parent) / name
            relative = path.relative_to(root).as_posix()
            compiler_input = path.suffix in {".go", ".s", ".S", ".c", ".h"}
            if visible is not None and relative not in visible and not compiler_input and not protected_path(relative):
                continue
            files[relative] = hashlib.sha256(path.read_bytes()).hexdigest()
    digest = hashlib.sha256(json.dumps(files, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
    return {"schema": "agentgo.swe-test-input/v1", "files": files, "digest": digest}


def protected_path(name):
    path = Path(name)
    return (name in PROTECTED_ROOT or name.endswith("_test.go") or "testdata" in path.parts
            or name.startswith((".github/", ".ci/")))


def protected_manifest(worktree):
    files = input_identity(worktree)["files"]
    return {"schema": "agentgo.go-test-baseline/v1", "files": {
        name: files.get(name) for name in sorted(set(PROTECTED_ROOT) | {n for n in files if protected_path(n)})}}


def module_identity(config, worktree):
    module = json.loads(run(config, worktree, ["go", "list", "-mod=readonly", "-m", "-json"]).stdout)
    if (module.get("Path") != config.suite.module or module.get("Main") is not True
            or Path(module.get("Dir", "")).resolve() != worktree.resolve()
            or module.get("Replace")):
        raise ValueError("Go module under test is not the task checkout")
    version = run(config, worktree, ["go", "version"]).stdout.decode().strip()
    if config.suite.toolchain not in version.split():
        raise ValueError("Go toolchain differs from the suite pin")
    # Replacing a dependency's extracted files must not silently change the judge.
    verification = run(config, worktree, ["go", "mod", "verify"]).stdout.decode().strip()
    return {"module": module, "go_version": version, "go_launcher": shutil.which("go"),
            "GOWORK": "off", "CGO_ENABLED": "0", "dependencies_verified": verification}


def prepare_environment(config, worktree):
    run(config, worktree, ["go", "mod", "download"], timeout=900)
    identity = module_identity(config, worktree)
    print(f"[环境准备] {identity['go_version']} module={config.suite.module}")


def decode_events(raw):
    if isinstance(raw, Path):
        with raw.open("rb") as stream:
            return _decode_event_lines(stream)
    return _decode_event_lines(raw.splitlines())


def _decode_event_lines(lines):
    events = []
    for line in lines:
        event = json.loads(line)
        if not isinstance(event, dict) or not isinstance(event.get("Action"), str):
            raise ValueError("invalid Go test JSON event")
        events.append(event)
    return events


def enumeration(events, selectors=()):
    selected = set(selectors)
    names = set()
    for event in events:
        output = event.get("Output", "").strip()
        if event.get("Action") == "output" and re.fullmatch(r"(?:Test|Example|Fuzz)[A-Za-z0-9_]*", output):
            if not selected or output in selected:
                names.add(event["Package"] + "::" + output)
    return sorted(names)


def parse_report(events, expected, exit_code):
    """Count top-level tests once; retain subtest failures as separate evidence."""
    if exit_code not in {0, 1}:
        raise ValueError("Go test process did not finish normally")
    package_starts, package_ends = set(), {}
    states, failures, outputs, build_errors = {}, set(), [], set()
    for event in events:
        action, package, name = event["Action"], event.get("Package", ""), event.get("Test", "")
        if action == "build-fail":
            build_errors.add(event.get("ImportPath") or package or "unknown")
        if action in {"output", "build-output"}:
            outputs.append(event.get("Output", ""))
        if action == "start":
            package_starts.add(package)
        if name:
            identity = package + "::" + name
            if "/" not in name:
                if action == "run":
                    if identity in states:
                        raise ValueError("Go test ran a top-level test more than once")
                    states[identity] = "run"
                elif action in {"pass", "fail", "skip"}:
                    if states.get(identity) not in {None, "run"}:
                        raise ValueError("conflicting Go test terminal events")
                    states[identity] = action
            if action == "fail":
                failures.add((identity, "test"))
        elif action in {"pass", "fail", "skip"}:
            if package in package_ends:
                raise ValueError("duplicate Go package terminal event")
            package_ends[package] = action
    if not events or package_starts - package_ends.keys():
        raise ValueError("Go test JSON ended before package settlement")
    if not package_ends and not build_errors:
        raise ValueError("Go test JSON has no package outcome")
    aborted = {name for name, state in states.items() if state == "run"}
    for name in aborted:
        failures.add((name, "aborted"))
    for package in build_errors:
        failures.add((package + "::<build>", "build"))
    for package, state in package_ends.items():
        if state == "fail" and not any(name.startswith(package + "::") for name, _ in failures):
            failures.add((package + "::<package>", "package"))
    observed = set(states)
    collection = set(expected or observed)
    if expected is not None and observed - collection:
        raise ValueError("Go executed tests outside the collected inventory")
    counts = collections.Counter(states.values())
    errors = collections.Counter(phase for _, phase in failures if phase != "test")
    not_run = collection - {name for name, state in states.items() if state in {"pass", "fail", "skip"}}
    if exit_code == 0 and (failures or not_run or counts["pass"] == 0):
        raise ValueError("Go exit=0 conflicts with test inventory or failure events")
    if exit_code == 1 and not failures:
        raise ValueError("Go exit=1 without recorded failures")
    return {"schema": REPORT_SCHEMA, "test_engine": "go", "count_semantics": COUNT_SEMANTICS,
            "tests": len(collection), "collected": len(collection), "collected_nodeids": sorted(collection),
            "passed": counts["pass"], "failed": counts["fail"], "skipped": counts["skip"],
            "not_run": sorted(not_run), "errors": sum(errors.values()), "phase_errors": dict(errors),
            "test_outcomes": dict(sorted(states.items())),
            "xfailed": 0, "xpassed": 0, "exit_code": exit_code,
            "failure_events": [{"nodeid": name, "phase": phase} for name, phase in sorted(failures)],
            "package_outcomes": package_ends, "summary_tail": "".join(outputs).splitlines()[-5:]}


def run_tests(config, task, worktree, run_dir, stage, *, targeted=False, run_id=""):
    current = resource_storage.active()
    with current.phase(stage) if current else contextlib.nullcontext():
        return _run_tests(config, task, worktree, run_dir, stage, targeted=targeted, run_id=run_id)


def _run_tests(config, task, worktree, run_dir, stage, *, targeted=False, run_id=""):
    run_dir.mkdir(parents=True, exist_ok=True)
    prefix = run_dir / (stage + ".go-test")
    selectors = task.test_selectors if targeted else ()
    before = input_identity(worktree)
    record = {"schema": "agentgo.swe-test-execution/v1", "test_engine": "go",
              "test_execution_id": str(uuid.uuid4()), "task_id": task.task_id, "run_id": run_id,
              "stage": stage, "cwd": str(worktree.resolve()), "started_at": now(),
              "input_before": before, "status": "running"}
    execution_path = prefix.with_name(prefix.name + ".execution.json")
    write_json(execution_path, record)
    try:
        record["baseline_commit"] = run(config, worktree, ["git", "rev-parse", "HEAD"]).stdout.decode().strip()
        record["execution_environment"] = module_identity(config, worktree)
        command = ["go", "test", "-json", "-mod=readonly", "-count=1", "-timeout", f"{config.suite.test_timeout_sec}s"]
        list_command = ["go", "test", "-json", "-mod=readonly", "-list", ".", "./..."]
        collected = run(config, worktree, list_command, timeout=600, check=False)
        managed_processes.save_output_reference(collected, run_dir / (stage + ".collection.jsonl"))
        record["collection_argv"] = list_command
        record["collection_exit_code"] = collected.returncode
        expected = enumeration(decode_events(managed_processes.stdout_source(collected)), selectors) if collected.returncode == 0 else None
        if targeted and expected is not None and {name.split("::", 1)[1] for name in expected} != set(selectors):
            raise ValueError("Go target regression tests were not collected")
        if targeted:
            command += ["-run", "^(" + "|".join(re.escape(name) for name in selectors) + ")$"]
        command.append("./...")
        record["argv"] = command
        result = run(config, worktree, command, timeout=config.suite.test_timeout_sec + 600, check=False)
        managed_processes.save_output_reference(result, prefix.with_name(prefix.name + ".jsonl"))
        managed_processes.save_output_reference(result, prefix.with_name(prefix.name + ".stderr.log"), stderr=True)
        record["exit_code"] = result.returncode
        events = decode_events(managed_processes.stdout_source(result))
        human_log = ("".join(event.get("Output", "") for event in events)[-65536:]
                     + managed_processes.output_summary(result).decode("utf-8", errors="replace")[-4096:])
        prefix.with_name(prefix.name + ".log").write_text(human_log, encoding="utf-8")
        report = parse_report(events, expected, result.returncode)
        record["input_after"] = input_identity(worktree)
        if before != record["input_after"]:
            raise ValueError("Go test input changed while the judge was running")
        module_identity(config, worktree)
        record.update(status="completed", collected_nodeids=report["collected_nodeids"])
        report.update(test_execution_ref=str(execution_path), test_input_digest=before["digest"])
        write_json(prefix.with_name(prefix.name + ".json"), report)
        return report
    except BaseException as error:
        record.update(status="interrupted" if isinstance(error, KeyboardInterrupt) else "invalid", error_type=type(error).__name__)
        raise
    finally:
        record["finished_at"] = now()
        write_json(execution_path, record)


def fix_paths(config, task, api):
    changed = api.run_command(["git", "-C", str(config.repo), "diff", "--name-only", "-z",
                               task.fix_sha + "^", task.fix_sha]).stdout.decode().split("\0")
    paths = [project_path(name) for name in changed if name]
    tests = [name for name in paths if name.endswith("_test.go")]
    sources = [name for name in paths if name.endswith(".go") and not name.endswith("_test.go")]
    if sorted(tests) != sorted(task.test_files) or not sources or set(paths) != set(tests + sources):
        raise ValueError("Go task patch does not match its declared source/test boundary")
    return tests, sources


def apply_paths(config, task, worktree, paths, api):
    patch = api.run_command(["git", "-C", str(config.repo), "diff", "--binary", task.fix_sha + "^", task.fix_sha, "--", *paths]).stdout
    if not patch:
        raise ValueError("empty Go task patch")
    api.run_command(["git", "apply", "-"], cwd=worktree, input_data=patch)


def green(report):
    return report["exit_code"] == 0 and report["passed"] > 0 and not report["failure_events"] and not report["not_run"]


def target_green(report):
    return green(report) and report["passed"] == report["collected"] and report["skipped"] == 0


def preserves_baseline(clean, current, expected_nodes=None):
    comparison = compare_failures(clean, current)
    # Golden tests add functions. Existing tests must remain collected, and a
    # previously passing/skipped test must not become unexecuted after a panic.
    missing = set(clean["collected_nodeids"]) - set(current["collected_nodeids"])
    newly_unexecuted = set(current["not_run"]) - set(clean["not_run"])
    lost_passes = sorted(name for name, state in clean["test_outcomes"].items()
                         if state == "pass" and current["test_outcomes"].get(name) != "pass")
    same_expected = expected_nodes is None or set(current["collected_nodeids"]) == set(expected_nodes)
    comparison.update(missing_baseline_tests=sorted(missing), newly_unexecuted=sorted(newly_unexecuted),
                      previously_passing_tests_not_passed=lost_passes, same_expected_collection=same_expected)
    return not comparison["added_failures"] and not missing and not newly_unexecuted and not lost_passes and same_expected, comparison


def prepare_task(config, task, api):
    worktree = api.setup_task(config, task)
    run_dir = config.run_dir(task.task_id)
    tests, _ = fix_paths(config, task, api)
    clean = run_tests(config, task, worktree, run_dir, "clean-baseline")
    if clean["errors"] or clean["not_run"]:
        raise ValueError("clean Go baseline did not finish all tests")
    apply_paths(config, task, worktree, tests, api)
    api.run_command(["git", "add", "--", *tests], cwd=worktree)
    api.run_command(["git", "-c", "user.name=agentgo-swe", "-c", "user.email=swe-test-runner@agentgo.local",
                     "commit", "-q", "-m", "test-runner: install Gin regression tests", "--", *tests], cwd=worktree)
    red = run_tests(config, task, worktree, run_dir, "targeted-baseline", targeted=True)
    if not red["failure_events"]:
        raise ValueError("Gin target did not reproduce on its base commit")
    baseline = run_tests(config, task, worktree, run_dir, "baseline")
    write_json(run_dir / "baseline.json", baseline)
    write_json(run_dir / "clean_baseline.json", clean)
    write_json(run_dir / "test_baseline_manifest.json", protected_manifest(worktree))
    print(f"[Go 红态确认] target_failures={len(red['failure_events'])} baseline_failures={len(clean['failure_events'])}")
    return baseline


def judge_task(config, task, api):
    worktree, run_dir = config.worktree(task.task_id), config.run_dir(task.task_id)
    run_id = api.read_json(run_dir / "run_contract.json", {}).get("run_id", "")
    saved = api.read_json(run_dir / "test_baseline_manifest.json", {})
    if saved.get("schema") != "agentgo.go-test-baseline/v1":
        raise ValueError("missing Go test protection manifest")
    current = protected_manifest(worktree)
    tampered = sorted(name for name in set(saved["files"]) | set(current["files"])
                      if saved["files"].get(name) != current["files"].get(name))
    targeted = run_tests(config, task, worktree, run_dir, "judge-targeted", targeted=True, run_id=run_id)
    regression = run_tests(config, task, worktree, run_dir, "judge", run_id=run_id)
    clean = api.read_json(run_dir / "clean_baseline.json", {})
    expected = api.read_json(run_dir / "baseline.json", {})["collected_nodeids"]
    preserved, comparison = preserves_baseline(clean, regression, expected)
    verdict = "resolved" if target_green(targeted) and preserved else "failed"
    if tampered:
        verdict = "test_tampered"
    paths = [name for name in input_identity(worktree)["files"] if name.endswith(".go") and not protected_path(name)]
    api.run_command(["git", "reset", "-q"], cwd=worktree)
    # Include deletions too, without staging config, fixtures or unrelated artifacts.
    tracked = api.run_command(["git", "ls-files", "-z"], cwd=worktree).stdout.decode().split("\0")
    paths = sorted(set(paths) | {name for name in tracked if name.endswith(".go") and not protected_path(name)})
    api.run_command(["git", "add", "-A", "--", *paths], cwd=worktree)
    patch = api.run_command(["git", "diff", "--cached", "--", *paths], cwd=worktree).stdout
    (run_dir / "model.patch").write_bytes(patch)
    report = dict(regression, schema="agentgo.swe-judge/v2", task_id=task.task_id, run_id=run_id,
                  verdict=verdict, patch_lines=len(patch.splitlines()), tampered=bool(tampered),
                  tampered_files=tampered, targeted=targeted, baseline_comparison=comparison,
                  preexisting_failures=clean["failure_events"], full_suite_green=green(regression),
                  criterion="target_failures_fixed_and_full_baseline_preserved")
    write_json(run_dir / "judge.json", report)
    print(f"[Go Judge] verdict={verdict} targeted_passed={targeted['passed']} regression_passed={regression['passed']} "
          f"preexisting_failures={len(clean['failure_events'])} added_failures={len(comparison['added_failures'])} tampered={bool(tampered)}")
    return report


def verify_candidates(config, api):
    counts, rows = collections.Counter(), []
    for task in api.load_tasks(config.tasks_file, "go"):
        target = config.testbed / "worktrees" / ("verify-" + task.task_id)
        worktree = api.setup_task(config, task, target=target)
        run_dir = config.testbed / "runs" / ("verify-" + task.task_id)
        tests, sources = fix_paths(config, task, api)
        clean = run_tests(config, task, worktree, run_dir, "clean")
        if clean["errors"] or clean["not_run"]:
            raise ValueError("clean Go baseline has build/package failures")
        apply_paths(config, task, worktree, tests, api)
        red = run_tests(config, task, worktree, run_dir, "red", targeted=True)
        apply_paths(config, task, worktree, sources, api)
        targeted = run_tests(config, task, worktree, run_dir, "green-targeted", targeted=True)
        regression = run_tests(config, task, worktree, run_dir, "green")
        expected = set(clean["collected_nodeids"]) | set(red["collected_nodeids"])
        preserved, comparison = preserves_baseline(clean, regression, expected)
        status = "OK" if red["failure_events"] and target_green(targeted) and preserved else "INVALID"
        counts[status] += 1
        rows.append({"task_id": task.task_id, "status": status, "clean_failures": clean["failure_events"],
                     "targeted_passed": targeted["passed"], "regression_passed": regression["passed"],
                     "baseline_comparison": comparison})
        print(json.dumps(rows[-1], ensure_ascii=False))
    write_json(config.testbed / "test-runner" / "candidates_report.json", {"counts": dict(counts), "tasks": rows})
    return dict(counts)
