"""Python 正式判题的输入身份；不注入 AgentGo，不解释 Agent 工具命令。"""

from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path


IDENTITY_SCHEMA = "agentgo.swe-test-input/v1"
ROOT_INPUTS = {
    "pyproject.toml", "pytest.ini", "tox.ini", "setup.cfg", "setup.py",
    "conftest.py", "uv.lock", "requirements.txt", "requirements-dev.txt",
}


def input_identity(worktree: Path) -> dict:
    """覆盖被测源码、测试、资源与根配置的实际字节，包括未提交增删。"""
    root = worktree.resolve(strict=True)
    paths = [root / name for name in ROOT_INPUTS if (root / name).exists()]
    for directory in ("src", "tests", "requirements"):
        base = root / directory
        if not base.exists():
            continue
        if base.is_symlink():
            raise ValueError("被测输入根目录不能是符号链接")
        for current, dirs, files in os.walk(base, followlinks=False):
            dirs[:] = sorted(name for name in dirs if name not in {"__pycache__", ".pytest_cache"})
            for name in dirs:
                if (Path(current) / name).is_symlink():
                    raise ValueError("被测输入目录不能通过符号链接改变归属")
            paths.extend(Path(current) / name for name in files if not name.endswith((".pyc", ".pyo")))
    files = {}
    for path in sorted(set(paths)):
        if path.is_symlink() or not path.resolve(strict=True).is_relative_to(root):
            raise ValueError("被测输入路径不属于冻结工作树")
        files[path.relative_to(root).as_posix()] = hashlib.sha256(path.read_bytes()).hexdigest()
    payload = json.dumps(files, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return {"schema": IDENTITY_SCHEMA, "files": files, "digest": hashlib.sha256(payload).hexdigest()}


def validate_import_origin(environment: object, worktree: Path, python: Path) -> None:
    if not isinstance(environment, dict):
        raise ValueError("pytest 缺少实际执行环境记录")
    if Path(str(environment.get("python") or "")).absolute() != python.absolute():
        raise ValueError("pytest 使用了不同的 Python 可执行文件")
    origin = environment.get("flask_file")
    if not isinstance(origin, str) or not origin:
        raise ValueError("pytest 未记录实际导入的 Flask 来源")
    source = (worktree / "src" / "flask").resolve(strict=True)
    if not Path(origin).resolve(strict=True).is_relative_to(source):
        raise ValueError("pytest 导入的 Flask 穿透到了被测工作树之外")
    packages = environment.get("packages")
    if not isinstance(packages, list) or not packages:
        raise ValueError("pytest 未记录依赖环境身份")


def compare_failures(baseline: dict, current: dict) -> dict:
    """按 nodeid/阶段比较，聚合计数相等不代表相同失败。"""
    def failures(report):
        entries = report.get("failure_events")
        if not isinstance(entries, list):
            raise ValueError("pytest 缺少版本化失败集合")
        return {(item["nodeid"], item["phase"]) for item in entries}
    old, new = failures(baseline), failures(current)
    encode = lambda values: [{"nodeid": node, "phase": phase} for node, phase in sorted(values)]
    old_collection, new_collection = baseline.get("collected_nodeids"), current.get("collected_nodeids")
    if not isinstance(old_collection, list) or not isinstance(new_collection, list):
        raise ValueError("pytest 缺少收集集合")
    return {"added_failures": encode(new - old), "removed_failures": encode(old - new),
            "same_collection": sorted(old_collection) == sorted(new_collection)}
