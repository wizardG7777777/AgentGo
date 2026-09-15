"""Image preparation only; never invoked by the target test container."""
from __future__ import annotations

import csv
import hashlib
import json
from pathlib import Path
import platform
import shutil
import subprocess
import sys


def call(argv, cwd=None):
    subprocess.run(argv, cwd=cwd, check=True)


def prepare(root=Path("/opt/swe"), app=Path("/opt/agentgo")):
    upstream = root / "upstream" / "flask"
    upstream.parent.mkdir(parents=True, exist_ok=True)
    call(["git", "clone", "--quiet", "https://github.com/pallets/flask.git", str(upstream)])
    suite = app / "scripts" / "swe_test_runner" / "suites" / "flask-8"
    with (suite / "tasks.csv").open(encoding="utf-8", newline="") as stream:
        tasks = list(csv.DictReader(stream))
    for task in tasks:
        task_id = task["task_id"]
        target = Path("/tmp/swe-dependencies") / task_id
        call(["git", "-C", str(upstream), "worktree", "add", "--detach", str(target), task["fix_sha"] + "^"])
        # uv is confined to this image preparation stage: it exports the
        # already locked dependencies. The final environment contains pip only.
        requirements = target / "swe-requirements.txt"
        call(["uv", "export", "--frozen", "--no-default-groups", "--group", "tests",
              "--no-emit-project", "--format", "requirements-txt", "--output-file", str(requirements)], target)
        # CPython's venv records the launcher's parent as `home`. Resolve the
        # /usr/local/bin symlink so copied environments still find encodings.
        call([str(Path(sys.executable).resolve()), "-m", "venv", ".venv"], target)
        python = target / ".venv" / "bin" / "python"
        call([str(python), "-m", "pip", "install", "--disable-pip-version-check", "--require-hashes",
              "--only-binary=:all:", "-r", str(requirements)], target)
        call([str(python), "-m", "pip", "install", "--disable-pip-version-check", "flit_core==3.12.0"], target)
        call([str(python), "-m", "pip", "install", "--disable-pip-version-check", "--no-build-isolation",
              "--no-deps", "--editable", "."], target)
        call([str(python), "-m", "pip", "uninstall", "--yes", "flit_core"], target)
        call([str(python), "-m", "pip", "check"], target)
        destination = root / "environments" / task_id
        destination.mkdir(parents=True)
        shutil.copytree(target / ".venv", destination / ".venv", symlinks=True)
        shutil.copy2(requirements, destination / "requirements.txt")
        base = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=target, text=True).strip()
        packages = subprocess.check_output([
            str(target / ".venv" / "bin" / "python"), "-c",
            "import importlib.metadata as m,json; print(json.dumps(sorted((d.metadata['Name'],d.version) for d in m.distributions())))",
        ], text=True)
        manifest = {
            "task_id": task_id, "fix_sha": task["fix_sha"], "base_sha": base,
            "build_root": str(target), "venv_root": str(target / ".venv"), "python": sys.version,
            "environment_dir": ".venv",
            "installer": "pip", "requirements_sha256": hashlib.sha256(requirements.read_bytes()).hexdigest(),
            "machine": platform.machine(), "packages": json.loads(packages),
            "inputs": {name: hashlib.sha256((target / name).read_bytes()).hexdigest()
                       for name in ("pyproject.toml", "uv.lock")},
        }
        (destination / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
        call(["git", "-C", str(upstream), "worktree", "remove", "--force", str(target)])


if __name__ == "__main__":
    prepare()
