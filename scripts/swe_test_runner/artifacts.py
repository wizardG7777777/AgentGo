"""Verified evidence archives with explicit exclusions for rebuildable environments."""
from __future__ import annotations
import hashlib
import json
import os
from pathlib import Path
import stat
import sys
import time
import uuid
import zipfile

from storage import StorageError, atomic_json, contained, linked, read_json, relative_path, physical_paths, REBUILDABLE_MARKER

SCHEMA = "agentgo.swe-artifact-manifest/v2"
LEGACY_SCHEMA = "agentgo.swe-artifact-manifest/v1"
PATH_ENCODING = "mapped-filesystem-path/v1"
ENCODED_PATH_PREFIX = "__swe_encoded_paths_v1__/"


def _zip_member_name(path):
    name = str(relative_path(path))
    try:
        name.encode("utf-8")
        encodable = True
    except UnicodeEncodeError:
        encodable = False
    if encodable and not name.startswith(ENCODED_PATH_PREFIX):
        return name
    # Preserve the logical filesystem name in JSON, and use a ZIP-safe name.
    # Reserve this namespace even for ordinary source paths to avoid aliases.
    return ENCODED_PATH_PREFIX + hashlib.sha256(name.encode("utf-8", "surrogatepass")).hexdigest()


def archive_member_name(record, manifest):
    name = str(relative_path(record["path"]))
    encoding = manifest.get("path_encoding")
    if encoding is None:
        if "member" in record:
            raise StorageError("archive_path_encoding_missing")
        return name
    if encoding != PATH_ENCODING:
        raise StorageError("archive_path_encoding_unsupported")
    expected = _zip_member_name(name)
    if record.get("member", name) != expected:
        raise StorageError("archive_member_mapping_mismatch")
    return expected


class BoundedArchiveFile:
    """Limit actual ZIP bytes, including headers, central directory and seeks."""
    def __init__(self, stream, limit):
        self.stream, self.limit = stream, limit

    def write(self, data):
        if self.stream.tell() + len(data) > self.limit:
            raise StorageError("archive_budget_exceeded")
        return self.stream.write(data)

    def __getattr__(self, name):
        return getattr(self.stream, name)


def digest_file(path):
    with Path(path).open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def source_entries(attempt, omitted_copies=None):
    excluded = []
    marked = []
    for resource in attempt.resources:
        if resource.get("owner") != "borrowed" and not resource.get("archive", True):
            excluded.append(attempt.storage.path / resource["path"])
        if (resource.get("owner") == "owned" and resource.get("purpose") == "rebuildable_environment"
                and resource.get("archive") is False and resource.get("archive_marker_sha256")
                and resource.get("archive_relative_path")):
            marked.append((relative_path(resource["archive_relative_path"]).parts, resource))
    # Generated server authentication belongs to this run only. It is not evidence.
    excluded += [attempt.reports / "setting.yaml"]
    stack = [(attempt.path, attempt.path, "")]
    paths = physical_paths(attempt.storage.path, attempt.state)
    for name in ("worktree", "scratch"):
        if not paths[name].is_relative_to(attempt.path) and paths[name].exists():
            stack.append((paths[name], paths[name], name))
    while stack:
        current, base, prefix = stack.pop()
        with os.scandir(current) as entries:
            for entry in sorted(entries, key=lambda e: e.name):
                path = Path(entry.path)
                relative = "/".join(part for part in (prefix, path.relative_to(base).as_posix()) if part)
                if any(path == root or path.is_relative_to(root) for root in excluded):
                    continue
                if path.parent == attempt.reports and (path.name == "setting.yaml.tmp" or
                        path.name.startswith(".setting.yaml.") and path.name.endswith(".tmp")):
                    continue
                if linked(path):
                    yield path, "link", relative
                elif entry.is_dir(follow_symlinks=False):
                    if path.is_relative_to(paths["worktree"]) or path.is_relative_to(paths["scratch"]):
                        candidates = [resource for suffix, resource in marked if path.parts[-len(suffix):] == suffix]
                        marker = path / REBUILDABLE_MARKER
                        if candidates and not linked(marker) and marker.is_file() and marker.stat().st_size <= 4096:
                            fingerprint = digest_file(marker)
                            matching = next((r for r in candidates if r["archive_marker_sha256"] == fingerprint), None)
                            if matching:
                                if omitted_copies is not None:
                                    omitted_copies.append({"path": relative, "resource_id": matching["resource_id"],
                                                           "reason": "registered_rebuildable_copy"})
                                continue
                    stack.append((path, base, prefix))
                elif entry.is_file(follow_symlinks=False):
                    yield path, "file", relative
                else:
                    raise StorageError("unarchivable_resource", relative)


def seal(attempt):
    from storage_inventory import ensure_stopped
    ensure_stopped(attempt.state)
    import scoped_exec
    scoped_exec.ensure_stopped(attempt, recover=True)
    if any(not p.record.get("quiescent") for p in attempt.processes):
        raise StorageError("process_quiescence_unknown")
    destination = contained(attempt.storage.path, attempt.storage.path / "archives" / attempt.batch_id / attempt.attempt_id / uuid.uuid4().hex)
    manifest_path = destination / "manifest.json"
    archive_path = destination / "evidence.zip"
    partial = destination / "evidence.zip.partial"
    manifest = {"schema": SCHEMA, "root_id": attempt.storage.root_id, "batch_id": attempt.batch_id,
                "attempt_id": attempt.attempt_id, "source_root": str(attempt.path),
                "source_roots": {name: str(path) for name, path in physical_paths(attempt.storage.path, attempt.state).items()},
                "archive": archive_path.name, "complete": False, "files": [], "started_at": time.time()}
    manifest.update(path_encoding=PATH_ENCODING, filesystem_encoding=sys.getfilesystemencoding(),
                    filesystem_errors=sys.getfilesystemencodeerrors())
    manifest["omitted_copies"] = []
    selected = list(source_entries(attempt, manifest["omitted_copies"]))
    # Reserve initial/final/atomic-temporary manifests separately. Paths are
    # measured as escaped JSON, so long or non-ASCII names cannot evade this.
    manifest_budget = len(json.dumps(manifest, ensure_ascii=True, indent=2).encode("utf-8")) + 4096
    manifest_budget += sum(len(json.dumps(relative, ensure_ascii=True).encode("utf-8")) + 512
                           for _, _, relative in selected)
    capacity = attempt.storage.archive_capacity(additional=3 * manifest_budget + 1)
    zip_limit = capacity - 3 * manifest_budget
    # Do not rely on a guessed compression ratio to reserve archival space.
    source_bytes = sum(p.lstat().st_size for p, _, _ in selected)
    attempt.storage.check_free(additional=source_bytes + 3 * manifest_budget)
    destination.mkdir(parents=True)
    atomic_json(manifest_path, manifest)
    last_check = time.monotonic()
    stored_names = set()
    try:
        with partial.open("xb") as sink, zipfile.ZipFile(BoundedArchiveFile(sink, zip_limit), "w", compression=zipfile.ZIP_DEFLATED, compresslevel=1, allowZip64=True) as archive:
            for source, kind, relative in selected:
                member_name = _zip_member_name(relative)
                if member_name in stored_names:
                    raise StorageError("archive_member_mapping_collision")
                stored_names.add(member_name)
                before = source.lstat()
                digest, size = hashlib.sha256(), 0
                info = zipfile.ZipInfo(member_name)
                info.compress_type = zipfile.ZIP_DEFLATED
                info._compresslevel = 1
                info.external_attr = (stat.S_IMODE(before.st_mode) | stat.S_IFREG) << 16
                with archive.open(info, "w", force_zip64=True) as target:
                    if kind == "link":
                        # A link is archived as data, never followed or recreated by verification.
                        data = os.fsencode(os.readlink(source))
                        target.write(data)
                        digest.update(data)
                        size = len(data)
                    else:
                        with source.open("rb") as stream:
                            while data := stream.read(1024 * 1024):
                                target.write(data)
                                digest.update(data)
                                size += len(data)
                                if time.monotonic() - last_check > 2:
                                    attempt.storage.check_free()
                                    last_check = time.monotonic()
                after = source.lstat()
                if (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns) != (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns):
                    raise StorageError("archive_source_changed", relative)
                record = {"path": relative, "kind": kind, "bytes": size,
                            "sha256": digest.hexdigest(), "mode": stat.S_IMODE(before.st_mode),
                            "mtime_ns": before.st_mtime_ns}
                if member_name != relative:
                    record["member"] = member_name
                manifest["files"].append(record)
        partial.replace(archive_path)
        manifest.update(archive_bytes=archive_path.stat().st_size, source_bytes=source_bytes,
                        archive_sha256=digest_file(archive_path), complete=True, finished_at=time.time())
        if len(json.dumps(manifest, ensure_ascii=True, indent=2).encode("utf-8")) + 1 > manifest_budget:
            raise StorageError("archive_metadata_budget_exceeded")
        atomic_json(manifest_path, manifest)
        verify(manifest_path)
        return manifest_path
    except BaseException as error:
        manifest.update(complete=False, error_type=type(error).__name__, finished_at=time.time())
        atomic_json(manifest_path, manifest)
        raise


def verify(manifest_path):
    manifest_path = Path(manifest_path)
    if linked(manifest_path) or linked(manifest_path.parent):
        raise StorageError("archive_path_is_link")
    data = read_json(manifest_path)
    if data.get("schema") not in (SCHEMA, LEGACY_SCHEMA):
        raise StorageError("archive_schema_rejected")
    if data.get("complete") is not True:
        raise StorageError("archive_incomplete")
    if ((data["schema"] == SCHEMA and data.get("path_encoding") != PATH_ENCODING)
            or (data["schema"] == LEGACY_SCHEMA and data.get("path_encoding") is not None)):
        raise StorageError("archive_path_encoding_unsupported")
    archive_name = relative_path(data["archive"])
    if len(archive_name.parts) != 1:
        raise StorageError("invalid_archive_path")
    archive_path = contained(manifest_path.parent, manifest_path.parent / data["archive"], exists=True)
    if archive_path.stat().st_size != data["archive_bytes"] or digest_file(archive_path) != data["archive_sha256"]:
        raise StorageError("archive_digest_mismatch")
    records = data.get("files")
    if not isinstance(records, list) or not records:
        raise StorageError("archive_manifest_empty")
    if data.get("path_encoding") not in (None, PATH_ENCODING):
        raise StorageError("archive_path_encoding_unsupported")
    seen, seen_paths = set(), set()
    with zipfile.ZipFile(archive_path) as archive:
        if len(archive.infolist()) != len(records):
            raise StorageError("archive_member_count_mismatch")
        for record in records:
            logical_name = str(relative_path(record["path"]))
            name = archive_member_name(record, data)
            if name in seen or logical_name in seen_paths or record["kind"] not in {"file", "link"}:
                raise StorageError("invalid_archive_member")
            seen.add(name)
            seen_paths.add(logical_name)
            member = archive.getinfo(name)
            if member.file_size != record["bytes"]:
                raise StorageError("archive_member_size_mismatch")
            digest = hashlib.sha256()
            with archive.open(member) as stream:
                while chunk := stream.read(1024 * 1024):
                    digest.update(chunk)
            if digest.hexdigest() != record["sha256"]:
                raise StorageError("archive_member_digest_mismatch")
        if set(archive.namelist()) != seen:
            raise StorageError("archive_member_set_mismatch")
    return data


def read_member(manifest_path, name):
    """Read evidence after cleanup without extracting or following any links."""
    data = verify(manifest_path)
    name = str(relative_path(name))
    record = next((item for item in data["files"] if item["path"] == name), None)
    if record is None or record["kind"] != "file":
        raise StorageError("archive_file_unavailable")
    with zipfile.ZipFile(Path(manifest_path).parent / data["archive"]) as archive:
        return archive.read(archive_member_name(record, data))
