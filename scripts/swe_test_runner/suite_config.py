"""Versioned native SWE suite metadata; the CLI and runtime remain shared."""
from __future__ import annotations

from dataclasses import dataclass, field
import json
from pathlib import Path, PurePosixPath
import re


def project_path(value: str) -> str:
    if not isinstance(value, str) or not value or any(c in value for c in "\\:\r\n\0"):
        raise ValueError("suite path must be a relative logical project path")
    path = PurePosixPath(value)
    if path.is_absolute() or any(part in {".", ".."} for part in value.split("/")):
        raise ValueError("suite path escapes its project")
    return path.as_posix()


@dataclass(frozen=True)
class Suite:
    suite_id: str
    project: str
    repository: str
    engine: str
    module: str = ""
    toolchain: str = ""
    test_timeout_sec: int = 180
    role_prompt_directory: str = "prompts/swe"
    options: dict = field(default_factory=dict)

    @classmethod
    def load(cls, directory: Path) -> "Suite":
        data = json.loads((directory / "suite.json").read_text(encoding="utf-8"))
        if data.get("schema") == "agentgo.swe-suite-group/v1":
            members = data.get("members")
            if not isinstance(members, list) or not members or len(set(members)) != len(members):
                raise ValueError("suite group requires distinct members")
            if any(not re.fullmatch(r"[a-z0-9][a-z0-9-]*", name) for name in members):
                raise ValueError("invalid suite group member")
            return cls(data["suite_id"], data["suite_id"], "", "group", options=data)
        if data.get("schema") == "agentgo.swe-suite/v3":
            for name in ("suite_id", "project"):
                if not re.fullmatch(r"[a-z0-9][a-z0-9-]*", data.get(name, "")):
                    raise ValueError("invalid native suite identity")
            if data.get("driver") not in {"go", "pytest", "vitest", "cargo", "ctest"}:
                raise ValueError("unsupported native driver")
            if data.get("task_count") != 8:
                raise ValueError("Core-96 suites require exactly eight tasks")
            upstream = data.get("upstream", {}).get("repository", "")
            if not re.fullmatch(r"https://github\.com/[\w.-]+/[\w.-]+\.git", upstream):
                raise ValueError("invalid native suite upstream")
            return cls(data["suite_id"], data["project"], upstream, "native",
                       role_prompt_directory="prompts/swe/native", options=data)
        if data.get("schema") == "agentgo.swe-suite/v1":
            if data.get("upstream", {}).get("repository") != "https://github.com/pallets/flask.git":
                raise ValueError("suite/v1 is the frozen Flask suite contract")
            return cls(data["suite_id"], "flask", data["upstream"]["repository"], "pytest")
        if data.get("schema") != "agentgo.swe-suite/v2":
            raise ValueError("unsupported SWE suite schema")
        if data.get("test_engine") != "go":
            raise ValueError("unsupported SWE test engine")
        for name in ("suite_id", "project"):
            if not re.fullmatch(r"[a-z0-9][a-z0-9-]*", data.get(name, "")):
                raise ValueError("invalid SWE suite identity")
        upstream = data.get("upstream", {}).get("repository", "")
        if not re.fullmatch(r"https://github\.com/[\w.-]+/[\w.-]+\.git", upstream):
            raise ValueError("suite upstream must identify a GitHub Git repository")
        go = data.get("go", {})
        if not re.fullmatch(r"go1\.\d+\.\d+", go.get("toolchain", "")):
            raise ValueError("Go suite requires an exact toolchain version")
        if not re.fullmatch(r"github\.com/[\w.-]+/[\w.-]+", go.get("module", "")):
            raise ValueError("Go suite requires its module identity")
        timeout = go.get("test_timeout_sec", 180)
        if type(timeout) is not int or timeout <= 0:
            raise ValueError("Go test process timeout must be positive")
        return cls(data["suite_id"], data["project"], upstream, "go", go["module"],
                   go["toolchain"], timeout, project_path(data["role_prompt_directory"]))
