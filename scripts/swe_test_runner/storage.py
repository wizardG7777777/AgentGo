"""Owned native SWE storage, admission and resource facts (no provider access)."""
from __future__ import annotations

import contextlib
import contextvars
import copy
from dataclasses import asdict, dataclass, fields
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import stat
import threading
import time
import uuid

GIB = 1024 ** 3
SCHEMA = "agentgo.swe-storage/v1"
ACTIVE = contextvars.ContextVar("swe_storage_attempt", default=None)
REPORT_ENVIRONMENT = {"AGENTGO_SWE_PYTEST_REPORT", "AGENTGO_NATIVE_PYTEST_REPORT", "AGENTGO_NATIVE_PYTHON_SUBJECT"}
CACHE_GROUPS = {"go-build", "go-mod", "cargo", "uv", "pip", "npm", "pnpm", "yarn", "tools", "upstream"}
REBUILDABLE_MARKER = ".swe-rebuildable-directory.json"


class StorageError(RuntimeError):
    def __init__(self, code, detail=""):
        self.code = code
        super().__init__(code + (": " + detail if detail else ""))


class InventoryTimeout(StorageError):
    def __init__(self):
        super().__init__("storage_observation_unavailable", "directory scan deadline")


def linked(path):
    path = Path(path)
    return path.is_symlink() or (hasattr(path, "is_junction") and path.is_junction())


def identifier(value):
    if not isinstance(value, str) or not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9_.-]{0,159}", value):
        raise StorageError("invalid_resource_id")
    return value


def relative_path(value):
    p = PurePosixPath(value)
    if not value or p.is_absolute() or "\\" in value or ":" in value or any(x in {"", ".", ".."} for x in value.split("/")):
        raise StorageError("invalid_resource_path")
    return p


def contained(root, path, *, exists=False):
    """Validate every existing ancestor; never follow an owned target link."""
    root, path = Path(root), Path(path)
    if linked(root):
        raise StorageError("storage_root_is_link")
    root = root.resolve(strict=True)
    path = Path(os.path.abspath(path))
    try:
        relative = path.relative_to(root)
    except ValueError:
        raise StorageError("resource_outside_root") from None
    if not relative.parts:
        raise StorageError("refusing_storage_root_operation")
    current = root
    for part in relative.parts:
        current /= part
        if linked(current):
            raise StorageError("resource_path_is_link", str(relative))
    resolved = path.resolve(strict=exists)
    if not resolved.is_relative_to(root) or resolved == root:
        raise StorageError("resource_outside_root")
    return path


def file_identity(path):
    s = Path(path).stat(follow_symlinks=False)
    return {"device": s.st_dev, "inode": s.st_ino}


def atomic_json(path, value):
    path = Path(path)
    if linked(path) or linked(path.parent):
        raise StorageError("metadata_path_is_link")
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name("." + path.name + "." + uuid.uuid4().hex + ".tmp")
    try:
        with temporary.open("x", encoding="utf-8", newline="\n") as stream:
            json.dump(value, stream, ensure_ascii=True, indent=2)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        deadline = time.monotonic() + 0.5
        while True:
            try:
                temporary.replace(path)
                break
            except PermissionError as error:
                # NTFS readers (including file scanners) may briefly hold a file
                # without FILE_SHARE_DELETE. Retry the same sealed bytes only;
                # do not chmod/truncate the destination or swallow a lasting error.
                if os.name != "nt" or getattr(error, "winerror", None) not in {5, 32, 33} or time.monotonic() >= deadline:
                    raise
                if linked(path):
                    raise StorageError("metadata_path_is_link") from error
                time.sleep(0.025)
    finally:
        temporary.unlink(missing_ok=True)


def read_json(path):
    return json.loads(Path(path).read_text(encoding="utf-8"))


class FileLock:
    def __init__(self, path):
        self.path, self.stream = Path(path), None

    def __enter__(self):
        if linked(self.path) or linked(self.path.parent):
            raise StorageError("lock_path_is_link")
        self.path.parent.mkdir(parents=True, exist_ok=True)
        stream = self.path.open("a+b")
        try:
            if stream.seek(0, os.SEEK_END) == 0:
                stream.write(b"0")
                stream.flush()
            stream.seek(0)
            if os.name == "nt":
                import msvcrt
                msvcrt.locking(stream.fileno(), msvcrt.LK_NBLCK, 1)
            else:
                import fcntl
                fcntl.flock(stream.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BaseException as error:
            stream.close()
            if isinstance(error, OSError):
                raise StorageError("storage_busy", self.path.name) from None
            raise
        self.stream = stream
        return self

    def __exit__(self, *args):
        if self.stream:
            self.stream.close()
            self.stream = None


def _inventory_link(info):
    return (stat.S_ISLNK(info.st_mode)
            or getattr(info, 'st_reparse_tag', 0) == getattr(stat, 'IO_REPARSE_TAG_MOUNT_POINT', -1))


class InventoryReader:
    """Bounded, read-only recovery; never changes permissions or follows links."""
    retry_delays = (0.025, 0.05, 0.1, 0.2)
    retry_budget_seconds = 1.0

    def __init__(self, root, *, max_seconds=None, retry_transient=False):
        self.root = Path(os.path.abspath(root))
        self.deadline = None if max_seconds is None else time.monotonic() + max_seconds
        self.retry_transient = retry_transient
        self.section = None
        self.diagnostics = {'failures': 0, 'retries': 0, 'retry_wait_seconds': 0.0,
                            'events': [], 'omitted_events': 0}

    def check_deadline(self):
        if self.deadline is not None and time.monotonic() >= self.deadline:
            raise InventoryTimeout()

    @staticmethod
    def transient_error(error):
        return os.name == 'nt' and getattr(error, 'winerror', None) in {5, 32, 33}

    def relative(self, path):
        try:
            value = Path(os.path.abspath(path)).relative_to(self.root).as_posix()
            return value if len(value) <= 512 else value[:512] + '<truncated>'
        except ValueError:
            return '<outside-owned-root>'

    def path_state(self, path):
        # This extra work happens only on errors. Check ancestors without resolve()
        # so a replaced parent cannot redirect a retry into an external tree.
        path = Path(os.path.abspath(path))
        try:
            parts = path.relative_to(self.root).parts
        except ValueError:
            return {'kind': 'outside_owned_root'}
        current = self.root
        for index in range(len(parts) + 1):
            try:
                info = current.lstat()
            except FileNotFoundError:
                return {'kind': 'missing', 'path': self.relative(current)}
            except OSError as error:
                return {'kind': 'unavailable', 'path': self.relative(current),
                        'at_leaf': index == len(parts),
                        'errno': error.errno, 'winerror': getattr(error, 'winerror', None)}
            if _inventory_link(info):
                return {'kind': 'leaf_link' if index == len(parts) and parts else 'ancestor_link',
                        'path': self.relative(current)}
            if index < len(parts):
                if not stat.S_ISDIR(info.st_mode):
                    return {'kind': 'ancestor_not_directory', 'path': self.relative(current)}
                current /= parts[index]
        return {'kind': 'directory' if stat.S_ISDIR(info.st_mode) else 'file',
                'device': info.st_dev, 'inode': info.st_ino}

    def read(self, path, operation, action, *, retry_action=None):
        event = None
        attempt = 0
        while True:
            self.check_deadline()
            try:
                value = (retry_action if attempt and retry_action is not None else action)()
                if event is not None:
                    event['outcome'] = 'recovered'
                return value
            except FileNotFoundError:
                if event is not None:
                    event['outcome'] = 'disappeared'
                return None
            except OSError as error:
                # Keep the original exception class for the existing scratch
                # freshness rule, but preserve the actual failing syscall too.
                operation_name = getattr(error, 'inventory_operation',
                                         'retry_lstat' if attempt and retry_action is not None else operation)
                error.inventory_operation = operation_name
                error.inventory_path = str(path)
                if event is None:
                    event = {'section': self.section, 'operation': operation_name,
                             'path': self.relative(path), 'errno': error.errno,
                             'winerror': getattr(error, 'winerror', None), 'retries': 0}
                    self.diagnostics['failures'] += 1
                    if len(self.diagnostics['events']) < 4:
                        self.diagnostics['events'].append(event)
                    else:
                        self.diagnostics['omitted_events'] += 1
                event.update(outcome='failed', last_operation=operation_name, last_errno=error.errno,
                             last_winerror=getattr(error, 'winerror', None))
                error.inventory_read_detail = event
                eligible = self.retry_transient and self.transient_error(error)
                if not eligible:
                    raise
                self.check_deadline()
                state = self.path_state(path)
                event.setdefault('initial_path_state', state)
                event['path_state'] = state
                if state['kind'] == 'missing':
                    event['outcome'] = 'disappeared'
                    return None
                if state['kind'] in {'outside_owned_root', 'ancestor_link', 'ancestor_not_directory'}:
                    raise
                if state['kind'] == 'unavailable' and not state['at_leaf']:
                    raise  # An unreadable ancestor cannot establish a safe retry boundary.
                if attempt >= len(self.retry_delays):
                    raise
                delay = self.retry_delays[attempt]
                if self.diagnostics['retry_wait_seconds'] + delay > self.retry_budget_seconds:
                    event['outcome'] = 'retry_budget_exhausted'
                    raise
                if self.deadline is not None and time.monotonic() + delay >= self.deadline:
                    raise InventoryTimeout() from error
                # Every retry repeats only a metadata read / directory snapshot.
                # Directory iterators have already closed, including on failure.
                time.sleep(delay)
                attempt += 1
                event['retries'] = attempt
                self.diagnostics['retries'] += 1
                self.diagnostics['retry_wait_seconds'] += delay
                self.check_deadline()
                # Revalidate after the wait as well; a parent may have changed
                # while this observer deliberately held no handles.
                state = self.path_state(path)
                event['path_state'] = state
                if state['kind'] == 'missing':
                    event['outcome'] = 'disappeared'
                    return None
                if (state['kind'] in {'outside_owned_root', 'ancestor_link', 'ancestor_not_directory'}
                        or (state['kind'] == 'unavailable' and not state['at_leaf'])):
                    raise


def measure_tree(root, *, max_seconds=None, excluded=(), _seen=None, deduplicate=True, _reader=None):
    """No link traversal. Logical and unique-file sizes are separate metrics."""
    root = Path(root)
    result = {"logical_bytes": 0, "unique_file_bytes": 0 if deduplicate else None,
              "allocated_bytes": 0 if deduplicate else None,
              "files": 0, "links": 0, "complete": True}
    excluded = set(map(Path, excluded))
    if root in excluded:
        return result
    reader = _reader or InventoryReader(root, max_seconds=max_seconds)
    seen, stack, started = _seen if _seen is not None else set(), [root], time.monotonic()

    def snapshot_directory(path):
        info = path.lstat()
        if _inventory_link(info):
            return info, []
        # A queued directory may have become a regular file; account for its
        # current no-follow metadata rather than discarding that replacement.
        if not stat.S_ISDIR(info.st_mode):
            return info, []
        try:
            entries = os.scandir(path)
        except OSError as error:
            error.inventory_operation = 'scandir_open'
            raise
        snapshot = []
        with entries:
            try:
                for index, entry in enumerate(entries):
                    if index % 256 == 0:
                        reader.check_deadline()
                    snapshot.append(entry)
            except OSError as error:
                error.inventory_operation = 'scandir_iterate'
                raise
        return info, snapshot

    def account(info):
        if _inventory_link(info):
            result['links'] += 1
        elif stat.S_ISREG(info.st_mode):
            result['files'] += 1
            result['logical_bytes'] += info.st_size
            if deduplicate:
                identity = (info.st_dev, info.st_ino)
                if identity not in seen:
                    seen.add(identity)
                    result['unique_file_bytes'] += info.st_size
                    if hasattr(info, 'st_blocks'):
                        result['allocated_bytes'] += info.st_blocks * 512
        else:
            result['links'] += 1

    while stack:
        if max_seconds is not None and time.monotonic() - started > max_seconds:
            raise InventoryTimeout()
        current = stack.pop()
        snapshot = reader.read(current, 'directory_lstat', lambda: snapshot_directory(current))
        if snapshot is None:
            continue
        current_info, snapshot = snapshot
        if _inventory_link(current_info) and current == root:
            raise StorageError('measurement_root_is_link')
        if _inventory_link(current_info) or not stat.S_ISDIR(current_info.st_mode):
            account(current_info)
            continue
        # Enumeration handles are closed before file-ID / per-file work.
        for entry in snapshot:
            if result["files"] % 256 == 0 and max_seconds is not None and time.monotonic() - started > max_seconds:
                raise InventoryTimeout()
            p = Path(entry.path)
            if p in excluded:
                continue
            info = reader.read(p, 'entry_stat', lambda: entry.stat(follow_symlinks=False), retry_action=p.lstat)
            if info is None:
                continue
            if deduplicate and stat.S_ISREG(info.st_mode) and not _inventory_link(info) and (not info.st_ino or not info.st_dev):
                info = reader.read(p, 'file_identity_lstat', p.lstat)
                if info is None:
                    continue
            if stat.S_ISDIR(info.st_mode) and not _inventory_link(info):
                stack.append(p)
            else:
                account(info)
    if os.name == "nt":
        result["allocated_bytes"] = None
    return result


@dataclass(frozen=True)
class StoragePolicy:
    min_free_bytes: int = 50 * GIB
    max_attempt_bytes: int = 24 * GIB
    max_live_bytes: int = 80 * GIB
    cache_max_bytes: int = 32 * GIB
    archive_max_bytes: int = 32 * GIB
    archive_reserve_bytes: int = 8 * GIB
    shutdown_reserve_bytes: int = GIB
    max_command_output_bytes: int = 256 * 1024 ** 2
    max_attempt_log_bytes: int = 4 * GIB
    max_active_attempts: int = 1
    max_build_jobs: int = 2
    sample_interval_ms: int = 2000
    inventory_interval_ms: int = 15000
    max_scan_seconds: int = 450
    max_inventory_age_seconds: int = 600
    max_monitor_stop_seconds: int = 30
    max_scratch_prefix_bytes: int = 80
    preserve_failed_worktree: bool = True
    require_wsl_host_space: bool = True

    @classmethod
    def load(cls, path=None):
        if path is None:
            return cls()
        return cls.from_record(read_json(path))

    @classmethod
    def from_record(cls, value):
        data = dict(value)
        if data.pop("schema", None) != "agentgo.swe-storage-policy/v1":
            raise StorageError("storage_policy_schema_rejected")
        known = {f.name for f in fields(cls)}
        if set(data) - known:
            raise StorageError("unknown_storage_policy_fields")
        result = cls(**data)
        for field in fields(cls):
            value = getattr(result, field.name)
            if isinstance(field.default, bool):
                if not isinstance(value, bool):
                    raise StorageError("invalid_storage_policy", field.name)
            elif type(value) is not int or value <= 0:
                raise StorageError("invalid_storage_policy", field.name)
        if result.max_active_attempts != 1:
            raise StorageError("unsupported_storage_concurrency", "one managed attempt per root")
        if result.max_inventory_age_seconds * 1000 <= result.max_scan_seconds * 1000 + result.inventory_interval_ms:
            raise StorageError("invalid_storage_policy", "inventory age must exceed scan budget plus interval")
        return result

    def record(self):
        return {"schema": "agentgo.swe-storage-policy/v1", **asdict(self)}


def host_free_bytes():
    """WSL adapter: read host volume free space, never run a provider/tool task."""
    if os.name == "nt" or not Path("/proc/sys/kernel/osrelease").exists():
        return None
    if "microsoft" not in Path("/proc/sys/kernel/osrelease").read_text().lower():
        return None
    import subprocess
    distro = os.environ.get("WSL_DISTRO_NAME", "")
    if not re.fullmatch(r"[A-Za-z0-9_.-]+", distro):
        raise StorageError("storage_observation_unavailable", "WSL distribution identity")
    # Query registry metadata only. DistributionName is validated, not arbitrary code.
    script = ("$d=Get-ChildItem 'HKCU:\\Software\\Microsoft\\Windows\\CurrentVersion\\Lxss' | "
              "ForEach-Object { Get-ItemProperty $_.PSPath } | Where-Object { $_.DistributionName -eq '" + distro + "' }; "
              r"if(@($d).Count -ne 1){exit 2}; $p=[string]$d.BasePath; if($p.StartsWith('\\?\')){$p=$p.Substring(4)}; "
              "$v=[System.IO.DriveInfo]::new([System.IO.Path]::GetPathRoot($p)); "
              "[Console]::WriteLine($v.AvailableFreeSpace)")
    try:
        proc = subprocess.Popen(["powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script],
                                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        try:
            out, _ = proc.communicate(timeout=10)
        except BaseException:
            proc.kill()
            proc.wait()
            raise
        if proc.returncode or not out.strip().isdigit():
            raise ValueError()
        return int(out)
    except (OSError, ValueError, subprocess.SubprocessError):
        raise StorageError("storage_observation_unavailable", "WSL host volume") from None


class StorageRoot:
    def __init__(self, path, policy=None):
        path = Path(path).absolute()
        if linked(path):
            raise StorageError("storage_root_is_link")
        path.mkdir(parents=True, exist_ok=True)
        self.path = path.resolve(strict=True)
        self.policy = policy or StoragePolicy()
        self.lock = FileLock(self.path / "locks" / "storage.lock")
        self._mutex = threading.RLock()
        self._volume = None
        with self.lock:
            marker = self.path / "storage-root.json"
            if marker.exists():
                value = read_json(marker)
                if (value.get("schema") != SCHEMA or value.get("root") != str(self.path)
                        or value.get("identity") != file_identity(self.path)):
                    raise StorageError("storage_root_identity_mismatch")
                self.root_id = value["root_id"]
            else:
                if any((self.path / name).exists() for name in ("batches", "archives", "cache", "w", "t")):
                    raise StorageError("unowned_managed_directories")
                self.root_id = uuid.uuid4().hex
                atomic_json(marker, {"schema": SCHEMA, "root_id": self.root_id, "root": str(self.path),
                                     "identity": file_identity(self.path), "created_at": time.time()})
            for name in ("batches", "archives", "cache", "cleanup", "w", "t"):
                contained(self.path, self.path / name).mkdir(exist_ok=True)

    def free(self):
        if self._volume is not None:
            self._volume.ensure_alive()
        local = shutil.disk_usage(self.path).free
        if self._volume is not None and self._volume.wsl:
            host = self._volume.free_bytes()
        else:
            host = host_free_bytes() if self.policy.require_wsl_host_space else None
        return {"local_free_bytes": local, "host_free_bytes": host}

    @contextlib.contextmanager
    def volume_lease(self):
        from volume import VolumeLease
        with VolumeLease(self.path, host_required=self.policy.require_wsl_host_space) as lease:
            self._volume = lease
            try:
                yield lease
            finally:
                self._volume = None

    def check_free(self, *, additional=0):
        values = self.free()
        threshold = self.policy.min_free_bytes + self.policy.shutdown_reserve_bytes + additional
        if any(value is not None and value < threshold for value in values.values()):
            raise StorageError("disk_free_below_floor")
        return values

    def archive_capacity(self, *, additional=0):
        # Include incomplete archives: failed compression must not disappear
        # from accounting. History is retained; a full budget stops admission.
        usage = measure_tree(self.path / "archives", max_seconds=self.policy.max_scan_seconds)
        remaining = self.policy.archive_max_bytes - usage["logical_bytes"]
        if remaining < additional:
            raise StorageError("archive_budget_exceeded")
        return remaining

    def cache_dir(self, name):
        if name not in CACHE_GROUPS:
            raise StorageError("unknown_cache_group")
        with self._mutex:
            index_path = self.path / "cache-index.json"
            index = read_json(index_path) if index_path.exists() else {"schema": SCHEMA, "root_id": self.root_id, "groups": {}}
            if index.get("root_id") != self.root_id or index.get("schema") != SCHEMA:
                raise StorageError("cache_owner_mismatch")
            path = contained(self.path, self.path / "cache" / name)
            record = index["groups"].get(name)
            if path.exists():
                if record is None or record.get("identity") != file_identity(path):
                    raise StorageError("unowned_cache_directory", name)
            else:
                path.mkdir()
                record = {"identity": file_identity(path), "created_at": time.time(), "last_used": 0}
                index["groups"][name] = record
            if time.time() - record["last_used"] > 60:
                record["last_used"] = time.time()
                atomic_json(index_path, index)
            return path

    def evict_idle_cache(self, *, pinned=()):
        """Caller holds root and volume leases; never evict during a command."""
        index_path = self.path / "cache-index.json"
        if not index_path.exists():
            return []
        index = read_json(index_path)
        if index.get("root_id") != self.root_id:
            raise StorageError("cache_owner_mismatch")
        usage = measure_tree(self.path / "cache")["unique_file_bytes"]
        if usage <= self.policy.cache_max_bytes:
            return []
        pinned = [Path(path).resolve() for path in pinned]
        removed = []
        for name, record in sorted(index["groups"].items(), key=lambda item: item[1]["last_used"]):
            if name not in CACHE_GROUPS:
                raise StorageError("unknown_cache_group")
            path = contained(self.path, self.path / "cache" / name)
            if any(item == path or item.is_relative_to(path) for item in pinned):
                continue
            if path.exists():
                size = measure_tree(path)["unique_file_bytes"]
                remove_owned(self.path, path, record["identity"])
                removed.append({"group": name, "unique_file_bytes": size})
            index["groups"].pop(name)
            atomic_json(index_path, index)
            usage = measure_tree(self.path / "cache")["unique_file_bytes"]
            if usage <= self.policy.cache_max_bytes * 0.8:
                break
        atomic_json(self.path / "cleanup" / ("cache-" + uuid.uuid4().hex + ".json"),
                    {"schema": "agentgo.swe-cache-cleanup/v1", "removed": removed,
                     "remaining_bytes": usage, "within_budget": usage <= self.policy.cache_max_bytes})
        if usage > self.policy.cache_max_bytes:
            raise StorageError("cache_budget_exceeded", "pinned or unowned data retained")
        return removed

    def batch(self, identity):
        self.check_free(additional=self.policy.archive_reserve_bytes)
        self.archive_capacity(additional=self.policy.archive_reserve_bytes)
        batch_id = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime()) + "-" + uuid.uuid4().hex[:12]
        directory = contained(self.path, self.path / "batches" / batch_id)
        directory.mkdir()
        atomic_json(directory / "batch.json", {"schema": SCHEMA, "root_id": self.root_id,
                    "batch_id": batch_id, "policy": self.policy.record(), "identity": identity,
                    "created_at": time.time()})
        return batch_id


def physical_paths(root, state):
    root = Path(root)
    base = contained(root, state["path"])
    layout = state.get("layout", "nested-v1")
    if layout == "compact-v1":
        identity = state["attempt_id"]
        if not re.fullmatch(r"[0-9a-f]{32}", identity):
            raise StorageError("invalid_resource_id")
        paths = {"worktree": root / "w" / identity[:12], "scratch": root / "t" / identity[:12]}
    elif layout == "nested-v1":
        paths = {"worktree": base / "worktree", "scratch": base / "scratch"}
    else:
        raise StorageError("storage_layout_rejected")
    paths.update(commands=base / "commands", reports=base / "reports")
    for name, path in paths.items():
        contained(root, path)
        if name in state and state[name] != str(path):
            raise StorageError("resource_path_identity_mismatch")
    return paths


def combine_usage(*items):
    return {key: None if any(item[key] is None for item in items) else sum(item[key] for item in items)
            for key in ("logical_bytes", "unique_file_bytes", "allocated_bytes", "files", "links")} | {"complete": True}


class Attempt:
    def __init__(self, storage, batch_id, suite, task, kind="model"):
        self.storage, self.policy = storage, storage.policy
        self.batch_id, self.suite, self.task = map(identifier, (batch_id, suite, task))
        self.attempt_id = uuid.uuid4().hex
        self.path = contained(storage.path, storage.path / "batches" / batch_id / "attempts" / suite / task / self.attempt_id)
        self.worktree = storage.path / "w" / self.attempt_id[:12]
        self.scratch = storage.path / "t" / self.attempt_id[:12]
        if len(os.fsencode(str(self.scratch / "00"))) > self.policy.max_scratch_prefix_bytes:
            raise StorageError("testbed_path_too_long", "use a shorter native SWE_TESTBED for portable scratch/socket paths")
        if self.worktree.exists() or self.scratch.exists():
            raise StorageError("resource_id_collision")
        self.phase_name = "prepare"
        self._mutex = threading.RLock()
        self.resources, self.processes = [], []
        self.peak_bytes = self.output_bytes = 0
        self._last_inventory = self._last_free = 0.0
        self._last_firewall = 0.0
        self._last_complete_inventory = time.monotonic()
        self._violation = None
        self._inventory_worker = None
        self._inventory_closed = False
        self.path.mkdir(parents=True)
        self.state = {"schema": SCHEMA, "root_id": storage.root_id, "batch_id": batch_id,
                      "attempt_id": self.attempt_id, "suite": suite, "task": task, "kind": kind,
                      "path": str(self.path), "identity": file_identity(self.path), "status": "allocated",
                      "layout": "compact-v1", "worktree": str(self.worktree), "scratch": str(self.scratch), "phases": {},
                      "created_at": time.time(), "policy": self.policy.record()}
        self.persist()
        for name, path in physical_paths(storage.path, self.state).items():
            self.register(path, "owned", name, create=True)
        self.reports = self.path / "reports"

    def persist(self):
        with self._mutex:
            atomic_json(self.path / "attempt.json", self.state)

    def observation_snapshot(self):
        """Only the ownership/layout/policy fields needed by a read-only worker."""
        with self._mutex:
            keys = ('schema', 'root_id', 'attempt_id', 'path', 'identity', 'layout', 'worktree', 'scratch')
            state = {key: copy.deepcopy(self.state[key]) for key in keys}
            state['policy'] = self.policy.record()
            return {'schema': 'agentgo.swe-observation-snapshot/v1', 'state': state}

    def register(self, path, owner, purpose, *, create=False, archive=True):
        if owner not in {"owned", "shared", "borrowed"}:
            raise StorageError("invalid_resource_owner")
        if owner == "borrowed":
            if create:
                raise StorageError("cannot_create_borrowed_resource")
            external = Path(path).resolve(strict=True)
            item = {"resource_id": uuid.uuid4().hex, "external_path": str(external),
                    "owner": owner, "purpose": purpose, "archive": False,
                    "status": "borrowed", "identity": file_identity(external)}
            self.resources.append(item)
            self._resource_event(item)
            return external
        path = contained(self.storage.path, path)
        item = {"resource_id": uuid.uuid4().hex, "path": path.relative_to(self.storage.path).as_posix(),
                "owner": owner, "purpose": purpose, "archive": archive, "status": "planned"}
        self.resources.append(item)
        self._resource_event(item)
        if create:
            path.mkdir(parents=True, exist_ok=True)
        if path.exists():
            item.update(status="created", identity=file_identity(path))
            self._resource_event(item)
        return path

    def _resource_event(self, item):
        with self._mutex:
            with (self.path / "resources.jsonl").open("a", encoding="utf-8", newline="\n") as stream:
                stream.write(json.dumps(item, ensure_ascii=True) + "\n")
                stream.flush()
                os.fsync(stream.fileno())

    def mark_rebuildable_resources(self):
        """Let copies inherit a declared environment's archival policy."""
        for item in self.resources:
            if (item.get("owner") != "owned" or item.get("purpose") != "rebuildable_environment"
                    or item.get("archive", True)):
                continue
            path = contained(self.storage.path, self.storage.path / item["path"])
            if not path.is_relative_to(self.worktree) or not path.is_dir() or linked(path):
                continue
            marker = path / REBUILDABLE_MARKER
            value = {"schema": "agentgo.swe-rebuildable-directory/v1", "root_id": self.storage.root_id,
                     "attempt_id": self.attempt_id, "resource_id": item["resource_id"]}
            if marker.exists() or marker.is_symlink():
                if linked(marker) or not marker.is_file() or marker.stat().st_size > 4096:
                    raise StorageError("rebuildable_marker_collision")
                try:
                    previous = read_json(marker)
                except (json.JSONDecodeError, UnicodeError):
                    raise StorageError("rebuildable_marker_collision") from None
                if previous != value:
                    raise StorageError("rebuildable_marker_collision")
            else:
                atomic_json(marker, value)
            fingerprint = hashlib.sha256(marker.read_bytes()).hexdigest()
            relative = path.relative_to(self.worktree).as_posix()
            if item.get("archive_marker_sha256") == fingerprint and item.get("archive_relative_path") == relative:
                continue
            item["archive_marker_sha256"] = fingerprint
            item["archive_relative_path"] = relative
            self._resource_event(item)

    @contextlib.contextmanager
    def activate(self):
        token = ACTIVE.set(self)
        try:
            yield self
        finally:
            try:
                self.finish_observation()
            finally:
                ACTIVE.reset(token)

    @contextlib.contextmanager
    def phase(self, name):
        previous = self.phase_name
        self.phase_name = identifier(name)
        with self._mutex:
            self.state.update(status="running", phase=name)
            self.persist()
        try:
            self.mark_rebuildable_resources()
            yield
        finally:
            self.phase_name = previous

    def environment(self, source=None, *, credentials=False):
        env = dict(os.environ if source is None else source)
        if not credentials:
            env = {k: v for k, v in env.items() if k in REPORT_ENVIRONMENT or not k.startswith(("SWE_", "AGENTGO_", "OPENAI_", "ANTHROPIC_"))}
        for name in ("AGENTGO_SWE_PYTEST_REPORT", "AGENTGO_NATIVE_PYTEST_REPORT"):
            if name in env and not Path(env[name]).resolve().is_relative_to(self.path):
                raise StorageError("test_report_outside_attempt")
        with self._mutex:
            phases = self.state["phases"]
            if self.phase_name not in phases:
                phases[self.phase_name] = format(len(phases), "02x")
                self.persist()
            scratch = self.scratch / phases[self.phase_name]
        scratch.mkdir(parents=True, exist_ok=True)
        cache = self.storage.path / "cache"
        for name in ("go-build", "go-mod", "cargo", "uv", "pip", "npm", "pnpm", "yarn"):
            self.storage.cache_dir(name)
        paths = {"TEMP": scratch, "TMP": scratch, "TMPDIR": scratch, "GOTMPDIR": scratch / "go",
                 "GOCACHE": cache / "go-build", "GOMODCACHE": cache / "go-mod", "CARGO_HOME": cache / "cargo",
                 "UV_CACHE_DIR": cache / "uv", "PIP_CACHE_DIR": cache / "pip",
                 "npm_config_cache": cache / "npm", "npm_config_store_dir": cache / "pnpm",
                 "YARN_CACHE_FOLDER": cache / "yarn"}
        for name, path in paths.items():
            contained(self.storage.path, path).mkdir(parents=True, exist_ok=True)
            for existing in list(env):
                if existing.lower() == name.lower():
                    env.pop(existing)
            env[name] = str(path)
        for existing in list(env):
            if existing.lower() == "goflags":
                env.pop(existing)
        env.update(PYTHONUTF8="1", PYTHONDONTWRITEBYTECODE="1", UV_LINK_MODE="copy",
                   GOFLAGS=f"-p={self.policy.max_build_jobs}",
                   CARGO_BUILD_JOBS=str(self.policy.max_build_jobs), CMAKE_BUILD_PARALLEL_LEVEL=str(self.policy.max_build_jobs))
        if os.name == 'posix':
            import scoped_exec
            for target in ('X86_64_UNKNOWN_LINUX_GNU', 'AARCH64_UNKNOWN_LINUX_GNU', 'X86_64_APPLE_DARWIN', 'AARCH64_APPLE_DARWIN'):
                env['CARGO_TARGET_' + target + '_RUNNER'] = scoped_exec.hook_command()
        firewall = getattr(self.storage, "firewall", None)
        if os.name == "nt" and firewall:
            import firewall_windows
            env[firewall_windows.ENV_SESSION] = str(firewall.path)
            env[firewall_windows.ENV_RUN] = self.attempt_id
            env["GOFLAGS"] = firewall_windows.go_flags(self.policy.max_build_jobs)
            for target in ("X86_64_PC_WINDOWS_GNU", "X86_64_PC_WINDOWS_MSVC", "AARCH64_PC_WINDOWS_MSVC"):
                env["CARGO_TARGET_" + target + "_RUNNER"] = firewall_windows.hook_command()
        if credentials:
            env["AGENTGO_TRACE_KEEP_ALL"] = "1"
        return env

    def check(self, *, force=False):
        if force:
            self.finish_observation()
            self._inventory_scan(final=True)
        with self._mutex:
            if self._violation:
                raise self._violation
            now = time.monotonic()
            try:
                firewall = getattr(self.storage, "firewall", None)
                if os.name == "nt" and firewall:
                    firewall.check_dialogs()
                if os.name == "nt" and firewall and (force or now - self._last_firewall >= 5):
                    firewall.check(self.attempt_id)
                    self._last_firewall = now
                if not force and now - self._last_complete_inventory > self.policy.max_inventory_age_seconds:
                    raise StorageError("storage_observation_unavailable", "inventory sample expired")
                if force or self._last_free == 0 or now - self._last_free >= self.policy.sample_interval_ms / 1000:
                    self.state["space"] = self.storage.check_free()
                    self._last_free = now
                if (not self._inventory_closed and (self._last_inventory == 0 or now - self._last_inventory >= self.policy.inventory_interval_ms / 1000)
                        and (self._inventory_worker is None or not self._inventory_worker.is_alive())):
                    self._inventory_worker = threading.Thread(target=self._inventory_scan, daemon=True)
                    self._inventory_worker.start()
            except (OSError, StorageError) as error:
                self._violation = error if isinstance(error, StorageError) else StorageError("storage_observation_unavailable")
                raise self._violation

    def _inventory_scan(self, *, final=False):
        # A separate interpreter prevents CPU-heavy Runner parsing from starving
        # metadata syscalls as they reacquire the GIL. The thread only waits.
        from storage_inventory import run_inventory
        started = time.monotonic()
        sections, observing = {}, "worker_start"
        failure_detail = {}
        try:
            result = run_inventory(self, exact=final)
            # Keep completed/failed scans separately: a later successful final
            # scan must not erase the cause of an earlier freshness failure.
            with (self.reports / 'inventory-samples.jsonl').open('a', encoding='utf-8') as stream:
                stream.write(json.dumps({'finished_at': time.time(), 'final': final, 'sample': result}, ensure_ascii=True) + '\n')
                stream.flush()
                os.fsync(stream.fileno())
            sections, observing = result.get("sections", {}), result.get("blocked_section", "")
            if result.get("ok") is not True:
                failure_detail = {"inventory_error_type": result.get("error_type", "UnknownError"),
                    "inventory_error_code": result.get("error_code", "storage_observation_unavailable"),
                    "inventory_blocked_section": observing, "inventory_error_path": result.get("error_path"),
                    "inventory_error_operation": result.get('error_operation'),
                    "inventory_error_errno": result.get('error_errno'),
                    "inventory_error_winerror": result.get('error_winerror'),
                    "inventory_error_read_detail": result.get('error_read_detail'),
                    "inventory_read_diagnostics": result.get('read_diagnostics'),
                    "inventory_incomplete": True,
                    "inventory_sections": sections, "inventory_wall_sec": time.monotonic() - started}
                scratch_denied = (os.name != 'nt' and result.get("error_type") == "PermissionError" and observing == "scratch"
                    and isinstance(result.get("error_path"), str)
                    and result['error_path'].startswith(self.scratch.relative_to(self.storage.path).as_posix() + '/'))
                if result.get("error_type") == "InventoryTimeout" or scratch_denied:
                    raise InventoryTimeout()
                raise StorageError(result.get("error_code", "storage_observation_unavailable"))
            current, cache, history, live = (result[key] for key in ("current", "cache", "history", "live"))
            with self._mutex:
                self.peak_bytes = max(self.peak_bytes, current["logical_bytes"])
                self.state.update(usage=current, peak_bytes=self.peak_bytes, cache_usage=cache,
                                  archive_usage=history,
                                  live_usage=live, output_bytes=self.output_bytes, sampled_at=time.time(),
                                  inventory_wall_sec=time.monotonic() - started, inventory_sections=sections,
                                  inventory_accounting=result.get('accounting', 'exact'),
                                  inventory_read_diagnostics=result.get('read_diagnostics'),
                                  peak_is_sampled=True)
                self._last_inventory = time.monotonic()
                self._last_complete_inventory = self._last_inventory
                self.state["inventory_incomplete"] = False
                self.persist()
                if current["logical_bytes"] > self.policy.max_attempt_bytes or live["logical_bytes"] > self.policy.max_live_bytes:
                    self._violation = self._violation or StorageError("storage_budget_exceeded")
                elif (cache['unique_file_bytes'] if cache['unique_file_bytes'] is not None else cache['logical_bytes']) > self.policy.cache_max_bytes:
                    self._violation = self._violation or StorageError("cache_budget_exceeded")
                elif history["logical_bytes"] > self.policy.archive_max_bytes:
                    self._violation = self._violation or StorageError("archive_budget_exceeded")
        except InventoryTimeout as error:
            with self._mutex:
                self._last_inventory = time.monotonic()
                self.state.update(inventory_incomplete=True, inventory_error_type=type(error).__name__,
                                  inventory_blocked_section=observing, inventory_sections=sections,
                                  inventory_wall_sec=time.monotonic() - started,
                                  inventory_timeouts=self.state.get("inventory_timeouts", 0) + 1)
                if failure_detail:
                    self.state.update(failure_detail)
                if final or self._last_inventory - self._last_complete_inventory > self.policy.max_inventory_age_seconds:
                    self._violation = self._violation or error
                self.persist()
        except Exception as error:
            with self._mutex:
                self._violation = self._violation or (error if isinstance(error, StorageError) else StorageError("storage_observation_unavailable"))
                self.state.update(failure_detail or {"inventory_error_type": type(error).__name__,
                    "inventory_blocked_section": observing, "inventory_sections": sections})
                self.persist()

    def finish_observation(self):
        with self._mutex:
            self._inventory_closed = True
            worker = self._inventory_worker
        if worker and worker is not threading.current_thread():
            worker.join(self.policy.max_scan_seconds + 15)
            if worker.is_alive():
                raise StorageError("storage_observation_unavailable", "inventory worker did not stop")

    def charge_output(self, size):
        with self._mutex:
            remaining = max(0, self.policy.max_attempt_log_bytes - self.output_bytes)
            accepted = min(size, remaining)
            self.output_bytes += accepted
            if accepted < size:
                self._violation = StorageError("output_limit_exceeded", "attempt")
            return accepted


def active():
    return ACTIVE.get()


def retry_readonly(function, path, error):
    if os.name != "nt" or not isinstance(error, PermissionError):
        raise error
    os.chmod(path, stat.S_IREAD | stat.S_IWRITE)
    function(path)


def remove_owned(root, path, expected):
    path = contained(root, path)
    if not path.exists():
        return "already_absent"
    if file_identity(path) != expected:
        raise StorageError("resource_identity_changed")
    if path.is_dir():
        shutil.rmtree(path, onexc=retry_readonly)
    else:
        path.unlink()
    return "deleted"
