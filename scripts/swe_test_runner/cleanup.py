"""Plan-bound, retryable cleanup of managed attempts, never legacy worktrees."""
from __future__ import annotations
import hashlib
from pathlib import Path
import time
import uuid

import artifacts
import process_identity
from storage import SCHEMA, FileLock, StorageError, atomic_json, contained, file_identity, read_json, remove_owned, physical_paths


def _manifest_identity(path):
    return artifacts.digest_file(path)


def plan(attempt, manifest_path, *, retained=False, reason=""):
    manifest_path = contained(attempt.storage.path, manifest_path, exists=True)
    manifest = artifacts.verify(manifest_path)
    if manifest["attempt_id"] != attempt.attempt_id or manifest["root_id"] != attempt.storage.root_id:
        raise StorageError("archive_owner_mismatch")
    if any(not p.record.get("quiescent") for p in attempt.processes):
        raise StorageError("process_quiescence_unknown")
    resources = []
    paths = physical_paths(attempt.storage.path, attempt.state)
    targets = [(paths[name], retained) for name in ("worktree", "scratch", "commands")]
    private_configs = {paths["reports"] / name for name in ("setting.yaml", "setting.yaml.tmp")}
    targets.extend((path, False) for path in sorted(private_configs))
    for path, retain_resource in targets:
        path = contained(attempt.storage.path, path)
        if path.exists():
            if path in private_configs and not path.is_file():
                raise StorageError("private_configuration_not_regular")
            resources.append({"path": path.relative_to(attempt.storage.path).as_posix(),
                              "identity": file_identity(path), "stable_ctime_ns": path.stat().st_ctime_ns,
                              "action": "retain" if retain_resource else "delete"})
    plan_id = uuid.uuid4().hex
    destination = attempt.storage.path / "cleanup" / plan_id
    destination.mkdir()
    value = {"schema": "agentgo.swe-cleanup-plan/v1", "plan_id": plan_id,
             "root": str(attempt.storage.path), "root_id": attempt.storage.root_id,
             "attempt_id": attempt.attempt_id, "attempt": str(attempt.path),
             "attempt_identity": file_identity(attempt.path), "created_at": time.time(),
             "manifest": str(manifest_path), "manifest_sha256": _manifest_identity(manifest_path),
             "resources": resources, "retain_reason": reason if retained else ""}
    path = destination / "plan.json"
    atomic_json(path, value)
    return path


def _processes_stopped(attempt_path):
    """Offline cleanup also refuses unfinished/unknown command receipts."""
    from types import SimpleNamespace
    import scoped_exec
    scoped_exec.ensure_stopped(SimpleNamespace(path=Path(attempt_path)))
    from storage_inventory import ensure_stopped
    ensure_stopped(read_json(attempt_path / "attempt.json"))
    for path in (attempt_path / "commands").glob("*/command.json"):
        data = read_json(path)
        if data.get("schema") != "agentgo.swe-command/v1" or data.get("quiescent") is not True:
            raise StorageError("process_quiescence_unknown", path.parent.name)
        from processes import process_library
        psutil = process_library()
        try:
            identities = list(data.get("observed_processes", []))
            if data.get("process_identity"):
                identities.append(data["process_identity"])
            elif data.get("pid") is not None:
                identities.append({"pid": data["pid"], "birth": data.get("birth")})
            if data.get("guardian_identity"):
                identities.append(data["guardian_identity"])
            if any(process_identity.alive(identity, psutil) for identity in identities):
                raise StorageError("cleanup_process_still_alive")
        except psutil.AccessDenied:
            raise StorageError("process_quiescence_unknown") from None


def reconcile_commands(attempt_path):
    """Recover resource facts only; never restart subject/model work."""
    from processes import process_library
    psutil = process_library()
    recovered = []
    for path in (attempt_path / "commands").glob("*/command.json"):
        data = read_json(path)
        if data.get("quiescent") is True:
            continue
        supervisor = data.get("supervisor_identity")
        if supervisor is None or process_identity.alive(supervisor, psutil):
            raise StorageError("process_quiescence_unknown", "supervisor still alive or unidentified")
        identities = list(data.get("observed_processes", []))
        if data.get("process_identity"):
            identities.append(data["process_identity"])
        if data.get("guardian_identity"):
            identities.append(data["guardian_identity"])
        if any(process_identity.alive(identity, psutil) for identity in identities):
            raise StorageError("cleanup_process_still_alive")
        if data.get("coverage") == "windows_job" and data.get("kill_on_supervisor_exit"):
            reason = "job_owner_exited"
        elif (path.parent / "guardian.json").is_file() and read_json(path.parent / "guardian.json").get("quiescent") is True:
            reason = "guardian_confirmed"
        elif data.get("status") == "starting" and data.get("pid") is None:
            reason = "startup_barrier_not_released"
        else:
            raise StorageError("process_quiescence_unknown", "no durable exit evidence")
        data.update(status="interrupted", output_complete=False, quiescent=True,
                    reconciled_at=time.time(), reconciliation_reason=reason)
        atomic_json(path, data)
        recovered.append(str(path))
    _processes_stopped(attempt_path)
    return recovered


def apply(plan_path, *, lock_held=False):
    plan_path = Path(plan_path)
    value = read_json(plan_path)
    if value.get("schema") != "agentgo.swe-cleanup-plan/v1":
        raise StorageError("cleanup_plan_schema_rejected")
    root = Path(value["root"])
    contained(root, plan_path, exists=True)
    marker = read_json(root / "storage-root.json")
    if (marker.get("schema") != SCHEMA or marker.get("root_id") != value["root_id"]
            or marker.get("identity") != file_identity(root)):
        raise StorageError("storage_root_identity_mismatch")
    if not lock_held:
        with FileLock(root / "locks" / "storage.lock"):
            return apply(plan_path, lock_held=True)
    attempt_path = contained(root, value["attempt"], exists=True)
    if file_identity(attempt_path) != value["attempt_identity"]:
        raise StorageError("attempt_identity_changed")
    manifest = contained(root, value["manifest"], exists=True)
    if _manifest_identity(manifest) != value["manifest_sha256"]:
        raise StorageError("cleanup_manifest_changed")
    archive = artifacts.verify(manifest)
    if archive["root_id"] != value["root_id"] or archive["attempt_id"] != value["attempt_id"]:
        raise StorageError("archive_owner_mismatch")
    receipt_path = plan_path.with_name("receipt.json")
    plan_digest = _manifest_identity(plan_path)
    previous = read_json(receipt_path) if receipt_path.exists() else None
    if previous and previous.get("plan_sha256") != plan_digest:
        raise StorageError("cleanup_plan_changed")
    import shutil
    receipt = previous or {"schema": "agentgo.swe-cleanup-receipt/v1", "plan_id": value["plan_id"],
                           "plan_sha256": plan_digest, "started_at": time.time(),
                           "free_before": shutil.disk_usage(root).free, "resources": [], "complete": False}
    _processes_stopped(attempt_path)
    # A valid archive alone does not authorize deleting arbitrary owned-root paths.
    paths = physical_paths(root, read_json(attempt_path / "attempt.json"))
    allowed = {paths[name] for name in ("worktree", "scratch", "commands")}
    private_configs = {paths["reports"] / name for name in ("setting.yaml", "setting.yaml.tmp")}
    allowed.update(private_configs)
    seen = set()
    for resource in value["resources"]:
        target = contained(root, root / resource["path"])
        if target not in allowed or target in seen or resource["action"] not in {"retain", "delete"}:
            raise StorageError("cleanup_target_not_allowed")
        if target in private_configs and target.exists() and not target.is_file():
            raise StorageError("private_configuration_not_regular")
        seen.add(target)
        if resource["action"] == "delete" and target.exists():
            if (file_identity(target) != resource["identity"]
                    or target.stat().st_ctime_ns != resource["stable_ctime_ns"]
                    or previous and previous.get("complete") and not previous.get("retained")):
                raise StorageError("resource_identity_changed")
    try:
        for resource in value["resources"]:
            target = contained(root, root / resource["path"])
            if target not in allowed or resource["action"] not in {"retain", "delete"}:
                raise StorageError("cleanup_target_not_allowed")
            item = {"path": resource["path"], "action": resource["action"], "status": "planned"}
            receipt["resources"].append(item)
            atomic_json(receipt_path, receipt)
            if resource["action"] == "retain":
                item.update(status="retained", reason=value["retain_reason"])
            else:
                item["status"] = remove_owned(root, target, resource["identity"])
            atomic_json(receipt_path, receipt)
        receipt.update(complete=True, retained=any(x["action"] == "retain" for x in value["resources"]))
        state = read_json(attempt_path / "attempt.json")
        state.update(status="retained" if receipt["retained"] else "cleaned",
                     archive_ref=str(manifest), cleanup_ref=str(receipt_path))
        atomic_json(attempt_path / "attempt.json", state)
        atomic_json(attempt_path / "reports" / "storage.json", state)
        summary_path = root / "batches" / state["batch_id"] / "summary.json"
        if summary_path.is_file():
            summary = read_json(summary_path)
            for row in summary.get("runs", []):
                if row.get("attempt_id") == state["attempt_id"]:
                    row["storage_status"] = state["status"]
                    row["storage_ref"] = str(attempt_path / "reports" / "storage.json")
            atomic_json(summary_path, summary)
    except BaseException as error:
        receipt.update(complete=False, error_code=getattr(error, "code", type(error).__name__))
        raise
    finally:
        receipt.update(finished_at=time.time(), free_after=shutil.disk_usage(root).free)
        atomic_json(receipt_path, receipt)
    return receipt
