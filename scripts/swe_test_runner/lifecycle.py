"""Attempt resource lifecycle. Subject outcome and storage outcome stay separate."""
from __future__ import annotations
import contextlib
import hashlib
import os
from pathlib import Path
import platform
import time

import artifacts
import cleanup
import storage


def source_identity():
    directory = Path(__file__).parent
    sources = [p for p in directory.glob("*.py") if not p.name.endswith("_test.py")]
    sources.append(directory / "firewall_broker.ps1")
    return {p.name: artifacts.digest_file(p) for p in sorted(sources)}


class Session:
    def __init__(self, root, policy, identity):
        self.storage = storage.StorageRoot(root, policy)
        self.frozen_sources, self.identity = source_identity(), dict(identity)
        self.batch_id = self.storage.batch({"runner_sources": self.frozen_sources, "python": platform.python_version(),
                         "platform": platform.platform(), "architecture": platform.machine(), **identity})
        self.batch_dir = self.storage.path / "batches" / self.batch_id
        self.rows = []
        self.expected = []
        self.expected_kind = None

    def execute(self, config, task_id, kind, callback):
        """Run a callback inside a fresh attempt; retain evidence on every failure."""
        with self.storage.lock, self.storage.volume_lease() as volume:
            self.storage.evict_idle_cache(pinned=[getattr(config, "repo", self.storage.path / "cache" / "upstream")])
            if source_identity() != self.frozen_sources:
                raise storage.StorageError("runner_sources_changed")
            expected_binary = self.identity.get("subject_binary_sha256")
            if expected_binary and artifacts.digest_file(config.agentgo_bin) != expected_binary:
                raise storage.StorageError("subject_binary_changed")
            self.storage.check_free(additional=self.storage.policy.max_attempt_bytes + self.storage.policy.archive_reserve_bytes)
            self.storage.archive_capacity(additional=self.storage.policy.archive_reserve_bytes)
            attempt = storage.Attempt(self.storage, self.batch_id, config.suite.suite_id, task_id, kind)
            attempt.state["volume_lease"] = volume.metadata
            for name in ("repo", "agentgo_bin"):
                path = getattr(config, name, None)
                if path is not None and Path(path).exists() and not Path(path).resolve().is_relative_to(self.storage.path):
                    attempt.register(path, "borrowed", name)
            attempt.state["cache_before"] = storage.measure_tree(self.storage.path / "cache")
            attempt.persist()
            # Each config keeps only an index to immutable attempts, never a reused path.
            config._managed_reports = getattr(config, "_managed_reports", {})
            config._managed_worktrees = getattr(config, "_managed_worktrees", {})
            config._managed_reports[task_id] = attempt.reports
            config._managed_worktrees[task_id] = attempt.worktree
            result = None
            error = finalization_error = None
            row = {"suite": config.suite.suite_id, "task": task_id, "kind": kind,
                   "attempt_id": attempt.attempt_id, "attempt": str(attempt.path), "status": "running",
                   "reports": str(attempt.reports)}
            self.rows.append(row)
            self.checkpoint()
            with attempt.activate():
                try:
                    result = callback(attempt)
                    if source_identity() != self.frozen_sources:
                        raise storage.StorageError("runner_sources_changed")
                except BaseException as failure:
                    error = failure
                    storage.atomic_json(attempt.reports / "failure.json", {
                        "schema": "agentgo.swe-resource-failure/v1", "error_type": type(failure).__name__,
                        "reason_code": getattr(failure, "code", getattr(failure, "reason_code", "attempt_execution_failed")),
                        "interrupted": isinstance(failure, KeyboardInterrupt)})
                finally:
                    try:
                        self.finish(attempt, result, error)
                    except BaseException as failure:
                        finalization_error = failure
                        if attempt.state["status"] != "cleaned":
                            attempt.state.update(status="retained", retention_reason=getattr(failure, "code", type(failure).__name__))
                        attempt.state["finalization_error"] = getattr(failure, "code", type(failure).__name__)
                        attempt.persist()
                        storage.atomic_json(attempt.reports / "storage.json", attempt.state)
                    row.update(status="failed" if error or finalization_error else "completed",
                               storage_status=attempt.state["status"], storage_ref=str(attempt.reports / "storage.json"),
                               reason_code=getattr(error or finalization_error, "code", getattr(error or finalization_error, "reason_code", None)),
                               task_resolved=result.get("task_resolved") if isinstance(result, dict) else None)
                    self.checkpoint()
            if error is not None:
                raise error
            if finalization_error is not None:
                raise finalization_error
            return result

    def finish(self, attempt, result, error):
        for process in attempt.processes:
            if not process._finished:
                process.stop()
        attempt.finish_observation()
        quiescent = all(p.record.get("quiescent") is True for p in attempt.processes)
        attempt.state.update(status="quiesced" if quiescent else "retained", quiescent=quiescent,
                             execution_error=getattr(error, "code", getattr(error, "reason_code", type(error).__name__ if error else None)),
                             process_coverage=sorted({p.record.get("coverage", "unknown") for p in attempt.processes}),
                             storage_enforcement="monitored", command_count=len(attempt.processes),
                             descendants_cancelled=sum(p.record.get("descendants_cancelled", 0) for p in attempt.processes))
        try:
            attempt.check(force=True)
        except storage.StorageError as failure:
            attempt.state["resource_error"] = failure.code
            if error is None:
                error = failure
        attempt.persist()
        storage.atomic_json(attempt.reports / "storage.json", dict(attempt.state, policy=attempt.policy.record()))
        if not quiescent:
            raise storage.StorageError("process_quiescence_unknown")
        firewall = getattr(attempt.storage, "firewall", None)
        if firewall:
            receipt = firewall.release_run(attempt.attempt_id)
            attempt.state["firewall"] = {"session_id": firewall.data["session_id"], "cleanup_complete": receipt.get("remaining") == 0,
                "remote_addresses": ["127.0.0.1"], "profile": "all", "leases": receipt.get("stats")}
            storage.atomic_json(attempt.reports / "firewall.json", attempt.state["firewall"])
            if receipt.get("remaining") != 0:
                raise storage.StorageError("firewall_rules_not_released")
        incomplete = isinstance(result, dict) and result.get("execution_complete") is False
        retain = incomplete or (error is not None and attempt.policy.preserve_failed_worktree)
        reason = "execution_incomplete" if incomplete else getattr(error, "code", type(error).__name__ if error else "")
        attempt.state.update(status="archiving")
        attempt.persist()
        manifest = artifacts.seal(attempt)
        plan = cleanup.plan(attempt, manifest, retained=retain, reason=reason)
        receipt = cleanup.apply(plan, lock_held=True)
        attempt.state.update(status="retained" if receipt["retained"] else "cleaned", archive_ref=str(manifest),
                             cleanup_ref=str(plan.with_name("receipt.json")), finished_at=time.time(),
                             retention_reason=reason if retain else "", output_bytes=attempt.output_bytes,
                             peak_bytes=attempt.peak_bytes)
        attempt.persist()
        storage.atomic_json(attempt.reports / "storage.json", attempt.state)
        if attempt.state.get("resource_error"):
            raise storage.StorageError(attempt.state["resource_error"])

    def checkpoint(self):
        started = {(row["suite"], row["task"]) for row in self.rows if row["kind"] == self.expected_kind}
        storage.atomic_json(self.batch_dir / "summary.json", {"schema": "agentgo.swe-managed-batch/v1",
                            "batch_id": self.batch_id, "runs": self.rows, "expected": self.expected,
                            "not_run": [item for item in self.expected if (item["suite"], item["task"]) not in started],
                            "updated_at": time.time()})


def current():
    return storage.active()
