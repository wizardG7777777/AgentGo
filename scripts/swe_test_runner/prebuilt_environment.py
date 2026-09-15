"""Install an image-prepared Flask environment without invoking a builder."""
from __future__ import annotations

import hashlib
import json
from pathlib import Path
import platform
import shutil


def install_prebuilt(root: Path, task_id: str, fix_sha: str, worktree: Path) -> None:
    template = root / task_id
    manifest = json.loads((template / "manifest.json").read_text(encoding="utf-8"))
    if manifest["task_id"] != task_id or manifest["fix_sha"] != fix_sha:
        raise RuntimeError("预制 Python 环境的题目身份不匹配")
    if manifest.get("machine") and manifest["machine"] != platform.machine():
        raise RuntimeError("预制 Python 环境的 CPU 架构不匹配")
    for name, expected in manifest["inputs"].items():
        if name not in {"pyproject.toml", "uv.lock"}:
            raise RuntimeError("预制 Python 环境包含未知输入身份")
        if hashlib.sha256((worktree / name).read_bytes()).hexdigest() != expected:
            raise RuntimeError(f"预制 Python 环境的 {name} 身份不匹配")
    venv = worktree / ".venv"
    environment_dir = manifest.get("environment_dir", "venv")
    if environment_dir not in {"venv", ".venv"}:
        raise RuntimeError("预制 Python 环境目录无效")
    shutil.copytree(template / environment_dir, venv, symlinks=True)
    # Editable Flask must refer to THIS task, never the image build checkout.
    previous = manifest["build_root"]
    for path in venv.rglob("*.pth"):
        text = path.read_text(encoding="utf-8")
        if previous in text:
            path.write_text(text.replace(previous, worktree.as_posix()), encoding="utf-8")
    # Standard venv/pip scripts embed absolute paths. Relocate only text launch
    # and activation scripts; keep native binaries and interpreter links intact.
    previous_venv = manifest.get("venv_root")
    if previous_venv:
        for path in (venv / "bin").iterdir():
            if path.is_symlink() or not path.is_file():
                continue
            raw = path.read_bytes()
            if b"\0" not in raw and previous_venv.encode() in raw:
                path.write_bytes(raw.replace(previous_venv.encode(), venv.as_posix().encode()))
        for path in venv.rglob("direct_url.json"):
            text = path.read_text(encoding="utf-8")
            old_url = Path(previous).as_uri()
            if old_url in text:
                path.write_text(text.replace(old_url, worktree.as_uri()), encoding="utf-8")
