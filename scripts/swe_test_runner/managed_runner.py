"""Managed native entrypoints, shared across all SWE suite engines."""
from __future__ import annotations
import collections
import json
import os
from pathlib import Path
import platform
import sys
from types import SimpleNamespace

import artifacts
import cleanup
import lifecycle
import storage


def configurations(config, api):
    if config.suite.engine == "group":
        import native_harness
        return list(native_harness.group_configs(config, api))
    return [config]


def qualify(config, task, api):
    current = storage.active()
    if config.suite.engine == "native":
        import native_harness
        return native_harness.qualify(config, task, api)
    root = api.setup_task(config, task, target=current.worktree)
    out = current.reports
    if config.suite.engine == "go":
        import native_go
        tests, sources = native_go.fix_paths(config, task, api)
        clean = native_go.run_tests(config, task, root, out, "clean")
        if clean["errors"] or clean["not_run"]:
            raise ValueError("clean Go baseline has build/package failures")
        native_go.apply_paths(config, task, root, tests, api)
        red = native_go.run_tests(config, task, root, out, "red", targeted=True)
        native_go.apply_paths(config, task, root, sources, api)
        targeted = native_go.run_tests(config, task, root, out, "green-targeted", targeted=True)
        green = native_go.run_tests(config, task, root, out, "green")
        okay, comparison = native_go.preserves_baseline(clean, green, set(clean["collected_nodeids"]) | set(red["collected_nodeids"]))
        if not red["failure_events"] or not native_go.target_green(targeted) or not okay:
            raise ValueError("Go candidate does not reproduce and repair the required defect")
        result = {"status": "qualified", "task_id": task.task_id, "comparison": comparison}
    else:
        def test(stage, files=()):
            return api.run_pytest(root, out / (stage + ".junit.xml"), out / (stage + ".pytest.log"), files, task_id=task.task_id)
        clean = test("clean")
        if clean["failed"] or clean["errors"]:
            raise ValueError("clean Flask baseline is not green")
        api.apply_fix_slice(config, task, root, "tests/")
        red = test("red", task.test_files)
        if not red["failed"] and not red["errors"]:
            raise ValueError("Flask candidate did not reproduce its defect")
        api.apply_fix_slice(config, task, root, "src/")
        green = test("green")
        if green["failed"] or green["errors"]:
            raise ValueError("Flask source fix did not restore green")
        result = {"status": "qualified", "task_id": task.task_id, "clean": clean, "red": red, "green": green}
    storage.atomic_json(out / "reference.json", result)
    return result


def ensure_qualified(session, config, task, api):
    if config.suite.engine != "native":
        return
    import native_harness
    path = native_harness.qualification_dir(config, task) / "reference.json"
    reference = storage.read_json(path) if path.is_file() else {}
    if (reference.get("status") == "qualified"
            and reference.get("definition_digest") == native_harness.definition_digest(config, task)
            and reference.get("harness_digest") == native_harness.harness_digest()
            and reference.get("platform") == sys.platform):
        return
    session.execute(config, task.task_id, "qualification", lambda attempt: qualify(config, task, api))


def command(config, args, api, mode):
    owners = []
    try:
        return _command(config, args, api, mode, owners)
    finally:
        for owner in owners:
            owner.close()


def _command(config, args, api, mode, owners):
    if sys.version_info < (3, 12):
        raise storage.StorageError("runner_python_version_unsupported", "native resource management requires Python 3.12 or later")
    policy_file = getattr(args, "storage_policy", None) or os.environ.get("SWE_STORAGE_POLICY")
    if config.suite.engine == "group" and not policy_file:
        raise storage.StorageError("storage_policy_required", "Core-96 requires an explicit policy file")
    if mode in {"task", "batch"} and not config.agentgo_bin.is_file():
        raise storage.StorageError("subject_binary_missing")
    policy = storage.StoragePolicy.load(policy_file)
    binary = artifacts.digest_file(config.agentgo_bin) if config.agentgo_bin.is_file() else None
    session = lifecycle.Session(config.testbed, policy, {"subject_binary_sha256": binary,
                "suite": config.suite.suite_id, "protocol": config.protocol,
                "models": config.model_capabilities(), "mode": mode,
                "runner_shell_declared": os.environ.get("SWE_RUNNER_SHELL", "unspecified")})
    firewall = None
    if os.name == "nt":
        import firewall_windows
        firewall = firewall_windows.from_environment()
        if firewall is None and getattr(args, "windows_firewall", False):
            import shutil
            firewall = firewall_windows.start(config.testbed, [config.agentgo_bin, sys.executable,
                getattr(sys, "_base_executable", sys.executable), shutil.which("node")],
                control_dir=session.batch_dir / "firewall")
        if firewall:
            owners.append(firewall)
            firewall.check()
            if not any(config.testbed.resolve().is_relative_to(Path(root)) for root in firewall.data["roots"]):
                raise storage.StorageError("firewall_testbed_outside_scope")
    session.storage.firewall = firewall
    if firewall:
        import firewall_smoke
        validation = firewall_smoke.run(config.testbed, firewall)
        storage.atomic_json(session.batch_dir / "firewall-validation.json", validation)
    print(json.dumps({"managed_batch": session.batch_id, "path": str(session.batch_dir), "mode": mode}), flush=True)
    members = configurations(config, api)
    for member in members:
        mirror = os.environ.get("SWE_UPSTREAM_ROOT")
        if mirror:
            member.repo = Path(mirror).resolve(strict=True) / member.suite.project
        # Reuse explicitly supplied existing upstreams; newly downloaded sources
        # live in the owned cache and are counted by the same budget.
        if not member.repo.exists() and not mirror and member.repo.is_relative_to(config.testbed):
            with session.storage.lock:
                member.repo = session.storage.cache_dir("upstream") / member.suite.project
        elif not member.repo.exists():
            raise storage.StorageError("borrowed_upstream_missing")
    counts = collections.Counter()
    expected = []
    for member in members if mode != "probe" else []:
        tasks = api.load_tasks(member.tasks_file, member.suite.engine)
        selected = getattr(args, "task_id", None)
        if selected:
            tasks = [task for task in tasks if task.task_id == selected]
        expected.extend((member, task) for task in tasks)
    if not expected and mode != "probe":
        raise ValueError("unknown SWE task selector")
    session.expected = [{"suite": member.suite.suite_id, "task": task.task_id} for member, task in expected]
    session.expected_kind = "qualification" if mode == "verify-candidates" else "model"
    storage.atomic_json(session.batch_dir / "expected.json", {
        "expected_count": len(expected), "mode": mode,
        "tasks": [{"suite": member.suite.suite_id, "task": task.task_id} for member, task in expected]})
    storage.atomic_json(session.batch_dir / "completion.json", {"complete": False, "status": "in_progress", "expected_count": len(expected)})
    session.checkpoint()
    if mode in {"probe", "task", "batch"}:
        try:
            session.execute(config, "provider-probe", "probe", lambda attempt: api.preflight_probe(config, getattr(args, "probe_timeout", getattr(args, "timeout", 45))))
        except BaseException as error:
            storage.atomic_json(session.batch_dir / "completion.json", {"complete": False, "status": "probe_failed",
                "reason": getattr(error, "code", getattr(error, "reason_code", type(error).__name__)), "expected_count": len(expected)})
            raise
        if mode == "probe":
            storage.atomic_json(session.batch_dir / "completion.json", {"complete": True, "status": "probe_completed", "expected_count": 0})
            return 0
    exit_code = 0
    for member, task in expected:
        print(json.dumps({"suite": member.suite.suite_id, "task": task.task_id, "stage": mode}), flush=True)
        try:
            if mode == "verify-candidates":
                session.execute(member, task.task_id, "qualification", lambda attempt: qualify(member, task, api))
                counts["OK"] += 1
            else:
                ensure_qualified(session, member, task, api)
                result = session.execute(member, task.task_id, "model", lambda attempt: api.execute_task_locked(member, task, args.timeout))
                code = api.final_exit_code(result)
                if code:
                    exit_code = max(exit_code, code)
                counts["resolved" if result.get("task_resolved") else "unresolved"] += 1
                # Independent attempts may expose subject failures. Provider or
                # environment failures stop the batch; they are not SWE scores.
                if result.get("infrastructure_ok") is False or result.get("model_contract_compatible") is False:
                    raise api.SWETestRunnerInfrastructureError("subject_infrastructure_failed", "execution", "See the retained attempt result")
        except KeyboardInterrupt:
            storage.atomic_json(session.batch_dir / "completion.json", {"complete": False, "counts": counts,
                                "reason": "batch_interrupted", "expected_count": len(expected)})
            return api.EXIT_SWE_TEST_RUNNER_FAILURE
        except Exception as error:
            counts["INVALID" if mode == "verify-candidates" else "infrastructure_error"] += 1
            print(json.dumps({"task": task.task_id, "error": api.safe_diagnostic(error)}), flush=True)
            exit_code = api.EXIT_SWE_TEST_RUNNER_FAILURE
            if isinstance(error, (storage.StorageError, api.SWETestRunnerInfrastructureError, api.SWETestRunnerModelContractError)) or mode != "verify-candidates":
                storage.atomic_json(session.batch_dir / "completion.json", {"complete": False, "counts": counts,
                                    "reason": getattr(error, "code", type(error).__name__), "expected_count": len(expected)})
                raise
        finally:
            session.checkpoint()
    storage.atomic_json(session.batch_dir / "completion.json", {"complete": True, "counts": counts,
                        "expected_count": len(expected), "exit_code": exit_code})
    print(json.dumps({"batch": session.batch_id, "counts": counts, "exit_code": exit_code}), flush=True)
    return exit_code


def existing_attempt(path):
    attempt_path = Path(path).resolve(strict=True)
    marker_path = next((parent / "storage-root.json" for parent in attempt_path.parents if (parent / "storage-root.json").is_file()), None)
    if marker_path is None:
        raise storage.StorageError("unowned_attempt")
    root = marker_path.parent
    storage.contained(root, attempt_path, exists=True)
    marker, state = storage.read_json(marker_path), storage.read_json(attempt_path / "attempt.json")
    if (marker.get("root_id") != state.get("root_id") or state.get("identity") != storage.file_identity(attempt_path)
            or state.get("path") != str(attempt_path)):
        raise storage.StorageError("attempt_identity_changed")
    resources = {}
    with (attempt_path / "resources.jsonl").open(encoding="utf-8") as stream:
        for line in stream:
            record = json.loads(line)
            resources[record["resource_id"]] = record
    store = storage.StorageRoot(root, storage.StoragePolicy.from_record(state["policy"]))
    paths = storage.physical_paths(root, state)
    return SimpleNamespace(storage=store, path=attempt_path, reports=attempt_path / "reports", state=state,
                           worktree=paths["worktree"], scratch=paths["scratch"],
                           resources=list(resources.values()), attempt_id=state["attempt_id"], batch_id=state["batch_id"], processes=[])


def maintenance(args):
    """Offline operations never construct provider configuration."""
    if sys.version_info < (3, 12):
        raise storage.StorageError("runner_python_version_unsupported", "Python 3.12 or later required")
    if args.operation == "archive-verify":
        data = artifacts.verify(args.manifest)
        print(json.dumps({"verified": True, "files": len(data["files"]), "archive_bytes": data["archive_bytes"]}))
        return 0
    if args.operation == "cleanup-apply":
        receipt = cleanup.apply(args.plan)
        print(json.dumps(receipt, ensure_ascii=True))
        return 0
    if args.operation == "cleanup-plan":
        attempt = existing_attempt(args.attempt)
        state = attempt.state
        cleanup._processes_stopped(attempt.path)
        manifest = state.get("archive_ref")
        if not manifest:
            raise storage.StorageError("archive_incomplete", "attempt has no verified archive")
        retained = state.get("status") == "retained" and not args.release_retained
        with attempt.storage.lock:
            plan_path = cleanup.plan(attempt, Path(manifest), retained=retained, reason=state.get("retention_reason", ""))
        print(json.dumps({"plan": str(plan_path), "retained": retained}))
        return 0
    if args.operation == "archive-seal":
        attempt = existing_attempt(args.attempt)
        with attempt.storage.lock, attempt.storage.volume_lease():
            if not attempt.worktree.is_dir():
                raise storage.StorageError("worktree_already_cleaned")
            recovered = cleanup.reconcile_commands(attempt.path)
            manifest = artifacts.seal(attempt)
            reason = "reconciled_interrupted_attempt" if recovered else attempt.state.get("retention_reason") or "manually_sealed"
            attempt.state.update(status="retained", quiescent=True, archive_ref=str(manifest),
                                 retention_reason=reason, reconciled_commands=len(recovered))
            storage.atomic_json(attempt.path / "attempt.json", attempt.state)
            storage.atomic_json(attempt.reports / "storage.json", attempt.state)
            batch = attempt.storage.path / "batches" / attempt.batch_id
            if (batch / "summary.json").is_file():
                summary = storage.read_json(batch / "summary.json")
                for row in summary.get("runs", []):
                    if row["attempt_id"] == attempt.attempt_id:
                        if row.get("status") == "running":
                            row.update(status="interrupted", reason_code="supervisor_interrupted")
                        row.update(storage_status="retained")
                storage.atomic_json(batch / "summary.json", summary)
        print(json.dumps({"manifest": str(manifest), "reconciled_commands": len(recovered), "retained": True}))
        return 0
    root = Path(args.testbed).resolve(strict=True)
    if args.operation == "storage-report":
        marker = root / "storage-root.json"
        print(json.dumps({"root": str(root), "managed": marker.is_file(), "usage": storage.measure_tree(root)}, ensure_ascii=True))
        return 0
    raise ValueError("unknown storage maintenance operation")
