"""Export only SWE reports and behavior records; verify before discarding work."""
from __future__ import annotations

import hashlib
import json
from pathlib import Path, PurePosixPath
import os

SCHEMA = "agentgo.swe-linux-export/v2"
PROFILE = "reports-and-behavior"
IMAGE_LABEL = "io.agentgo.swe-linux.lifecycle"
IMAGE_CONTRACT = "reports-and-behavior-v2"
ROOT_REPORTS = ("run.json", "process-exit.json", "runtime-error.json", "recovery.json", "image.json", "agentgo-binary.json")
HOST_REPORTS = {"host.json", "container-state.json", "cleanup.json"}


def atomic_json(path, value):
    temporary = path.with_name(path.name + ".tmp")
    temporary.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    temporary.replace(path)


def digest_file(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def log_files(root):
    if root.is_symlink():
        raise RuntimeError("行为日志目录不能是符号链接")
    if not root.is_dir():
        return
    def walk_error(error):
        raise error

    for current, directories, files in os.walk(root, followlinks=False, onerror=walk_error):
        parent = Path(current)
        for name in sorted(directories):
            if (parent / name).is_symlink():
                raise RuntimeError("行为日志目录包含符号链接")
        for name in sorted(files):
            path = parent / name
            if path.is_symlink():
                raise RuntimeError("行为日志文件不能是符号链接")
            yield path


def selected_files(batch):
    for name in ROOT_REPORTS:
        if (batch / name).is_file():
            yield batch / name, Path(name)
    runs = batch / "testbed" / "runs"
    for path in log_files(runs):
        # setting.yaml and model.patch are configuration/source artifacts.
        if path.suffix in {".json", ".jsonl", ".xml", ".log", ".txt"} or path.name == "fix_sha":
            yield path, Path("runs") / path.relative_to(runs)
    candidates_report = batch / "testbed" / "test-runner" / "candidates_report.txt"
    if candidates_report.is_file():
        yield candidates_report, Path("runs") / "candidates_report.txt"
    probes = batch / "provider-probes"
    for path in log_files(probes):
        yield path, Path("provider-probes") / path.relative_to(probes)
    worktrees = batch / "testbed" / "worktrees"
    if worktrees.is_dir():
        for task in sorted(worktrees.iterdir()):
            if task.is_symlink():
                raise RuntimeError("题目工作目录不能是符号链接")
            state = task / ".agentgo"
            if state.is_symlink():
                raise RuntimeError("行为记录根目录不能是符号链接")
            # These roots contain journals, model history and referenced log
            # bodies. Deliberately exclude candidates, workspaces and .venv.
            for name in ("state", "sessions", "traces"):
                for path in log_files(state / name):
                    yield path, Path("behavior") / task.name / path.relative_to(state)
            if (state / "system.log").is_file():
                yield state / "system.log", Path("behavior") / task.name / "system.log"


def copy_verified(source, destination, relative):
    if source.is_symlink():
        raise RuntimeError("导出文件不能是符号链接")
    target = destination / relative
    target.parent.mkdir(parents=True, exist_ok=True)
    if target.is_symlink() or not target.resolve().is_relative_to(destination.resolve()):
        raise RuntimeError("导出目标超出本批目录")
    temporary = target.with_name(target.name + ".partial")
    if temporary.is_symlink():
        raise RuntimeError("导出临时文件不能是符号链接")
    before = source.stat()
    digest = hashlib.sha256()
    with source.open("rb") as input_stream, temporary.open("wb") as output_stream:
        while chunk := input_stream.read(1024 * 1024):
            output_stream.write(chunk)
            digest.update(chunk)
    after = source.stat()
    if (before.st_size, before.st_mtime_ns) != (after.st_size, after.st_mtime_ns):
        raise RuntimeError("导出期间原始记录仍在变化")
    if digest_file(temporary) != digest.hexdigest():
        raise RuntimeError("导出副本校验失败")
    temporary.replace(target)
    return {"path": relative.as_posix(), "bytes": target.stat().st_size, "sha256": digest.hexdigest()}


def verify_export(destination, batch_id):
    status = json.loads((destination / "archive-status.json").read_text(encoding="utf-8"))
    if (status.get("schema"), status.get("profile"), status.get("batch_id"), status.get("complete")) != (
            SCHEMA, PROFILE, batch_id, True):
        raise RuntimeError("报告/日志导出未完成或镜像导出契约不匹配")
    records = status.get("files")
    if not isinstance(records, list) or not records:
        raise RuntimeError("导出文件清单缺失")
    seen = set()
    for record in records:
        relative = PurePosixPath(record["path"])
        if relative.is_absolute() or ".." in relative.parts or "\\" in record["path"] or not relative.parts:
            raise RuntimeError("导出清单路径非法")
        if relative.parts[0] not in {*ROOT_REPORTS, "batch.log", "README.txt", "runs", "behavior", "provider-probes"}:
            raise RuntimeError("导出清单包含报告和行为日志以外的文件")
        path = destination.joinpath(*relative.parts)
        if path.is_symlink() or not path.resolve().is_relative_to(destination.resolve()):
            raise RuntimeError("导出清单指向批次外部")
        if record["path"] in seen or path.stat().st_size != record["bytes"] or digest_file(path) != record["sha256"]:
            raise RuntimeError("导出文件缺失、重复或内容校验失败")
        seen.add(record["path"])
    actual = {p.relative_to(destination).as_posix() for p in log_files(destination)}
    if actual - seen - HOST_REPORTS - {"archive-status.json"}:
        raise RuntimeError("导出目录包含未登记文件")
    return status


def export_batch(batch, destination, *, recovered=False):
    destination.mkdir(parents=True, exist_ok=True)
    status = {"schema": SCHEMA, "profile": PROFILE, "batch_id": batch.name,
              "complete": False, "recovered_after_container_exit": recovered}
    atomic_json(destination / "archive-status.json", status)
    if not batch.is_dir():
        raise FileNotFoundError("本批工作目录不存在，无法恢复")
    if recovered:
        atomic_json(batch / "recovery.json", {"execution_complete": None, "reason": "container_exited_before_export"})
    records = []
    for source, relative in selected_files(batch):
        if not source.resolve().is_relative_to(batch.resolve()):
            raise RuntimeError("原始行为记录超出本批工作目录")
        records.append(copy_verified(source, destination, relative))
    # The supervisor writes batch.log directly to the host while running. On
    # forced exit that file may be newer than the copy in the working volume.
    live_log = destination / "batch.log"
    if live_log.is_file():
        records.append({"path": "batch.log", "bytes": live_log.stat().st_size, "sha256": digest_file(live_log)})
    elif (batch / "batch.log").is_file():
        records.append(copy_verified(batch / "batch.log", destination, Path("batch.log")))
    readme = batch / "export-README.txt"
    readme.write_text(
        f"SWE_LinuxContainers batch: {batch.name}\nRecovered export: {recovered}\n"
        "Reports: runs/summary.json, runs/<task>/result.json and judge.json\n"
        "Behavior: batch.log, provider-probes/, behavior/<task>/\n"
        "Only reports and behavior records are exported. Lifecycle: cleanup.json\n"
        "Raw model/tool records may contain source text or sensitive content.\n", encoding="utf-8")
    records.append(copy_verified(readme, destination, Path("README.txt")))
    status.update(complete=True, files=records, bytes=sum(item["bytes"] for item in records))
    atomic_json(destination / "archive-status.json", status)
    try:
        verify_export(destination, batch.name)
    except Exception:
        status["complete"] = False
        atomic_json(destination / "archive-status.json", status)
        raise
    return status
