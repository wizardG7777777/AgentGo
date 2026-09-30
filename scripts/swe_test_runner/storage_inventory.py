"""Read-only inventory in an isolated interpreter, outside the Runner's GIL."""
from __future__ import annotations
import json
import os
from pathlib import Path
import subprocess
import sys
import time

# -I deliberately excludes the script directory; import only our own modules.
if __name__ == "__main__":
    sys.path.insert(0, str(Path(__file__).resolve().parent))
import storage

SCHEMA = "agentgo.swe-inventory/v1"


def collect(root, attempt_path, max_seconds, *, exact=True, snapshot=None):
    started = time.monotonic()
    sections, observing, seen = {}, "initialization", set()
    result = {"schema": SCHEMA, "pid": os.getpid(), "ok": False,
              "accounting": 'exact' if exact else 'logical-with-cache-refinement'}
    reader = storage.InventoryReader(root, max_seconds=max_seconds, retry_transient=True)
    try:
        root, attempt_path = Path(root), Path(attempt_path)
        storage.contained(root, attempt_path, exists=True)
        if snapshot is None:
            # Offline direct callers can still inspect stopped attempts. Live
            # workers must use the detached snapshot sent by their supervisor.
            try:
                state = storage.read_json(attempt_path / 'attempt.json')
            except Exception as error:
                error.inventory_operation = 'read_attempt_metadata'
                raise
            result['state_source'] = 'disk'
        else:
            if not isinstance(snapshot, dict) or snapshot.get('schema') != 'agentgo.swe-observation-snapshot/v1' or not isinstance(snapshot.get('state'), dict):
                raise storage.StorageError('inventory_snapshot_invalid')
            state = snapshot['state']
            storage.StoragePolicy.from_record(state['policy'])
            result['state_source'] = 'supervisor_snapshot'
        marker = storage.read_json(root / "storage-root.json")
        if (state.get('schema') != storage.SCHEMA
                or state.get('attempt_id') != attempt_path.name
                or Path(os.path.abspath(state['path'])) != Path(os.path.abspath(attempt_path))
                or state.get("identity") != storage.file_identity(attempt_path)
                or marker.get("identity") != storage.file_identity(root)
                or marker.get("root_id") != state.get("root_id")):
            raise storage.StorageError("inventory_owner_mismatch")
        paths = storage.physical_paths(root, state)
        current_paths = (attempt_path, paths["worktree"], paths["scratch"])
        def measure(path, label, *, live=False, excluded=(), deduplicate=None):
            nonlocal observing
            observing = label
            reader.section = label
            remaining = max_seconds - (time.monotonic() - started)
            if remaining <= 0:
                raise storage.InventoryTimeout()
            section_start = time.monotonic()
            value = storage.measure_tree(path, max_seconds=remaining, excluded=excluded,
                _seen=seen if live else None, deduplicate=exact if deduplicate is None else deduplicate,
                _reader=reader)
            sections[label] = {"wall_sec": time.monotonic() - section_start, "files": value["files"]}
            return value
        current = storage.combine_usage(*(measure(path, label, live=True) for path, label in
            zip(current_paths, ("attempt_metadata", "worktree", "scratch"))))
        cache = measure(root / "cache", "cache")
        # Logical bytes are an upper bound on unique bytes. Below the cap, no
        # per-file identity lookup is needed; above it, refine before deciding
        # rather than falsely rejecting a hardlink-heavy cache.
        if not exact and cache['logical_bytes'] > state['policy']['cache_max_bytes']:
            cache = measure(root / 'cache', 'cache_exact', deduplicate=True)
        history = measure(root / "archives", "archives")
        live = storage.combine_usage(current, *(measure(root / name, "other_" + name, live=True,
            excluded=current_paths) for name in ("batches", "w", "t")))
        result.update(ok=True, current=current, cache=cache, history=history, live=live)
    except Exception as error:
        result.update(error_type=type(error).__name__, error_code=getattr(error, "code", "storage_observation_unavailable"),
                      blocked_section=observing)
        result.update(error_operation=getattr(error, 'inventory_operation', None),
                      error_errno=getattr(error, 'errno', None), error_winerror=getattr(error, 'winerror', None))
        detail = getattr(error, 'inventory_read_detail', None)
        if detail is not None:
            result['error_read_detail'] = detail
        filename = getattr(error, 'inventory_path', None) or getattr(error, "filename", None)
        if filename:
            result['error_path'] = reader.relative(filename)
    result.update(sections=sections, wall_sec=time.monotonic() - started, read_diagnostics=reader.diagnostics)
    return result


def ensure_stopped(state):
    record = state.get("inventory_scanner")
    if not record:
        return
    from processes import process_library
    import process_identity
    psutil = process_library()
    if not record.get("identity"):
        raise storage.StorageError("process_quiescence_unknown", "inventory identity missing")
    try:
        if process_identity.alive(record["identity"], psutil):
            raise storage.StorageError("inventory_process_still_alive")
    except psutil.AccessDenied:
        raise storage.StorageError("process_quiescence_unknown", "inventory process") from None


def run_inventory(attempt, *, exact=True):
    ensure_stopped(attempt.state)
    from processes import process_library
    import process_identity
    # A Windows venv redirector may spawn another interpreter. This worker uses
    # only stdlib and our source, so launch the real base executable directly.
    executable = str(Path(getattr(sys, "_base_executable", sys.executable)).resolve(strict=True))
    environment = {key: value for key, value in os.environ.items()
                   if key.upper() in {"PATH", "SYSTEMROOT", "WINDIR", "LANG", "LC_ALL"}}
    options = {"creationflags": subprocess.CREATE_NO_WINDOW} if os.name == "nt" else {"start_new_session": True}
    process = subprocess.Popen([executable, "-I", "-S", "-B", str(Path(__file__).resolve())],
        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=environment, **options)
    identity = None
    try:
        identity = process_identity.capture(process_library().Process(process.pid))
        with attempt._mutex:
            attempt.state["inventory_scanner"] = {"identity": identity, "quiescent": False,
                "role": "read_only_inventory", "started_at": time.time()}
            attempt.persist()
            snapshot = attempt.observation_snapshot()
        request = {"root": str(attempt.storage.path), "attempt": str(attempt.path),
                   "max_seconds": attempt.policy.max_scan_seconds, "exact": exact, "snapshot": snapshot}
        try:
            out, err = process.communicate(json.dumps(request).encode("utf-8"), timeout=attempt.policy.max_scan_seconds + 5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.communicate(timeout=5)
            return {"schema": SCHEMA, "ok": False, "error_type": "InventoryTimeout",
                "error_code": "storage_observation_unavailable", "blocked_section": "worker_deadline", "sections": {}}
        if process.returncode != 0 or len(out) > 65536:
            raise storage.StorageError("inventory_worker_failed", f"exit={process.returncode}")
        value = json.loads(out)
        if value.get("schema") != SCHEMA or value.get("pid") != process.pid:
            raise storage.StorageError("inventory_worker_identity_mismatch")
        return value
    finally:
        if process.poll() is None:
            process.kill()
            process.wait(timeout=5)
        for stream in (process.stdin, process.stdout, process.stderr):
            stream.close()
        if identity is not None:
            with attempt._mutex:
                attempt.state["inventory_scanner"].update(quiescent=process.poll() is not None,
                    exit_code=process.returncode, finished_at=time.time())
                attempt.persist()


if __name__ == "__main__":
    try:
        raw = sys.stdin.buffer.read(65537)
        if len(raw) > 65536:
            raise ValueError("inventory request size")
        request = json.loads(raw)
        maximum = request["max_seconds"]
        if type(maximum) is not int or maximum <= 0:
            raise ValueError("inventory deadline")
        exact = request.get('exact', True)
        if type(exact) is not bool:
            raise ValueError('inventory accounting mode')
        if not isinstance(request.get('snapshot'), dict):
            raise ValueError('live inventory requires a supervisor snapshot')
        value = collect(request["root"], request["attempt"], maximum, exact=exact, snapshot=request['snapshot'])
    except Exception as error:
        value = {"schema": SCHEMA, "pid": os.getpid(), "ok": False,
            "error_code": "storage_observation_unavailable", "error_type": type(error).__name__,
            "blocked_section": "request", "sections": {}}
    print(json.dumps(value, ensure_ascii=True), flush=True)
