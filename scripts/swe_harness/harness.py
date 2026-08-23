#!/usr/bin/env python3
"""AgentGo SWE 系统回归的无第三方依赖 Python 权威入口。

本文件统一处理考题准备、进程编排、运行契约、能力探针、终态判定、Judge 和脱敏
指标。Flask 题目、worktree、原始日志和凭证始终留在外部 testbed；输出不得包含
prompt、reasoning 或工具参数。
"""

from __future__ import annotations

import argparse
import collections
import csv
import datetime as dt
import glob
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import shutil
import signal
import socket
import ssl
import subprocess
import time
import urllib.error
import urllib.request
import uuid
import xml.etree.ElementTree as ET


RUN_SCHEMA = "agentgo.run-contract/v1"
RESULT_SCHEMA = "agentgo.swe-result/v2"
PROBE_NAME = "agentgo_capability_probe_test"
PROBE_NONCE = "nonce_test_7f3a"
TERMINAL_TASK = {"completed", "failed", "blocked", "cancelled"}
TERMINAL_GRAPH = {"completed", "failed", "blocked", "cancelled"}
TERMINAL_OUTCOME = {"success", "failed", "blocked", "cancelled"}
NANOSECOND = 1_000_000_000
TASK_ID_PATTERN = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}\Z")
EXIT_HARNESS_FAILURE = 1
EXIT_ARCHITECTURE_FAILURE = 2
EXIT_TASK_FAILURE = 3


class HarnessConfig:
    """从统一 SWE_* 环境变量解析的运行配置。"""

    def __init__(self, agentgo_root: Path, agentgo_bin: Path, testbed: Path,
                 tasks_file: Path, prompt_dir: Path, flask_repo: Path,
                 base_url: str, model: str, protocol: str):
        self.agentgo_root = agentgo_root
        self.agentgo_bin = agentgo_bin
        self.testbed = testbed
        self.tasks_file = tasks_file
        self.prompt_dir = prompt_dir
        self.flask_repo = flask_repo
        self.base_url = base_url
        self.model = model
        self.protocol = protocol

    @classmethod
    def from_env(cls) -> "HarnessConfig":
        repo_root = Path(__file__).resolve().parents[2]
        agentgo_root = Path(os.environ.get("SWE_AGENTGO_ROOT", repo_root)).resolve()
        testbed = Path(os.environ.get("SWE_TESTBED", "/tmp/agentgo-swe")).resolve()
        binary_default = agentgo_root / ("agentgo.exe" if os.name == "nt" else "agentgo")
        protocol = os.environ.get("SWE_PROTOCOL", "responses")
        if protocol not in {"responses", "chat_completions"}:
            raise ValueError(f"未知 SWE_PROTOCOL={protocol!r}")
        return cls(
            agentgo_root=agentgo_root,
            agentgo_bin=Path(os.environ.get("SWE_AGENTGO_BIN", binary_default)).resolve(),
            testbed=testbed,
            tasks_file=Path(os.environ.get(
                "SWE_TASKS_FILE", testbed / "harness" / "tasks.csv")).resolve(),
            prompt_dir=Path(os.environ.get(
                "SWE_PROMPT_DIR", testbed / "harness" / "prompts")).resolve(),
            flask_repo=Path(os.environ.get(
                "SWE_FLASK_REPO", "/Users/yanchenyu/Documents/PythonProjects/flask")).resolve(),
            base_url=os.environ.get("SWE_BASE_URL", "https://openrouter.ai/api/v1"),
            model=os.environ.get("SWE_MODEL", "openai/gpt-5.6-luna"),
            protocol=protocol,
        )

    def worktree(self, task_id: str) -> Path:
        return self.testbed / "worktrees" / validate_task_id(task_id)

    def run_dir(self, task_id: str) -> Path:
        return self.testbed / "runs" / validate_task_id(task_id)


class TaskSpec:
    def __init__(self, task_id: str, fix_sha: str, test_files: tuple[str, ...], title: str):
        self.task_id = validate_task_id(task_id)
        self.fix_sha = fix_sha
        self.test_files = test_files
        self.title = title


def validate_task_id(task_id: str) -> str:
    if not TASK_ID_PATTERN.fullmatch(task_id) or task_id in {".", ".."}:
        raise ValueError(f"非法考题 ID: {task_id!r}")
    return task_id


def load_tasks(path: str | Path) -> list[TaskSpec]:
    tasks: list[TaskSpec] = []
    seen: set[str] = set()
    with Path(path).open(newline="", encoding="utf-8") as handle:
        reader = csv.DictReader(handle)
        expected = {"task_id", "fix_sha", "test_files", "title"}
        if not reader.fieldnames or not expected.issubset(reader.fieldnames):
            raise ValueError(f"tasks.csv 缺少字段: {sorted(expected)}")
        for row in reader:
            task_id = validate_task_id((row.get("task_id") or "").strip())
            if task_id in seen:
                raise ValueError(f"tasks.csv 存在重复考题: {task_id}")
            seen.add(task_id)
            fix_sha = (row.get("fix_sha") or "").strip()
            if not re.fullmatch(r"[0-9a-fA-F]{7,64}", fix_sha):
                raise ValueError(f"考题 {task_id} 的 fix_sha 非法")
            test_files = tuple(shlex.split(row.get("test_files") or ""))
            if not test_files or any(
                    Path(item).is_absolute() or ".." in Path(item).parts
                    or not item.replace("\\", "/").startswith("tests/")
                    for item in test_files):
                raise ValueError(f"考题 {task_id} 的 test_files 非法")
            tasks.append(TaskSpec(task_id, fix_sha, test_files, row.get("title") or ""))
    return tasks


def find_task(config: HarnessConfig, task_id: str) -> TaskSpec:
    wanted = validate_task_id(task_id)
    for task in load_tasks(config.tasks_file):
        if task.task_id == wanted:
            return task
    raise ValueError(f"未知考题: {wanted}")


def utc_now() -> dt.datetime:
    return dt.datetime.now(dt.timezone.utc)


def format_time(value: dt.datetime) -> str:
    return value.astimezone(dt.timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")


def atomic_json(path: str | Path, value: object) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    temp = target.with_name(target.name + ".tmp")
    temp.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    os.replace(temp, target)


def read_json(path: str | Path, default: object | None = None):
    try:
        return json.loads(Path(path).read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return {} if default is None else default


def iter_jsonl(pattern: str):
    for path in sorted(glob.glob(pattern, recursive=True)):
        try:
            with open(path, encoding="utf-8") as handle:
                for line in handle:
                    try:
                        yield path, json.loads(line)
                    except json.JSONDecodeError:
                        continue
        except OSError:
            continue


def build_run_contract(task_id: str, timeout_sec: int, now: dt.datetime | None = None) -> dict:
    if timeout_sec < 240:
        raise ValueError("SWE 外部时限必须至少为 240 秒，才能保留 Run 收割窗口")
    created = (now or utc_now()).astimezone(dt.timezone.utc)
    # 外部 hard kill 前固定留 60 秒；Run 内再冻结 90 秒 recovery 与 30 秒 finalization。
    deadline = created + dt.timedelta(seconds=timeout_sec - 60)
    safe_task = "".join(ch if ch.isalnum() or ch in "-_" else "-" for ch in task_id)[:48]
    return {
        "schema": RUN_SCHEMA,
        "run_id": f"run-swe-{safe_task}-{uuid.uuid4()}",
        "deadline_at": format_time(deadline),
        "finalization_reserve": 30 * NANOSECOND,
        "recovery_reserve": 90 * NANOSECOND,
        "budget_profile": "swe/v1",
        "budget": {},
        "created_at": format_time(created),
    }


def probe_request(model: str, probe_name: str, nonce: str, protocol: str) -> dict:
    tool = {
        "type": "function",
        "name": probe_name,
        "description": "Prove typed function calling with one required nonce.",
        "parameters": {
            "type": "object",
            "additionalProperties": False,
            "properties": {"nonce": {"type": "string", "const": nonce}},
            "required": ["nonce"],
        },
        "strict": True,
    }
    if protocol == "responses":
        return {
            "model": model,
            "input": f"Call the required function exactly once with nonce {nonce}.",
            # 真实验证 thinking + auto-singleton；DeepSeek thinking 会拒绝
            # exact/required choice，但 auto 仍必须返回下方 typed nonce call。
            "reasoning": {"effort": "low"},
            "tools": [tool],
            "tool_choice": "auto",
            "max_output_tokens": 256,
            "stream": False,
        }
    return {
        "model": model,
        "messages": [{
            "role": "user",
            "content": f"Call the provided function exactly once with nonce {nonce}. Do not answer with text.",
        }],
        "tools": [{
            "type": "function",
            "function": {
                key: value for key, value in tool.items() if key != "type"
            },
        }],
        "tool_choice": "auto",
        "reasoning_effort": "low",
        "max_tokens": 256,
        "stream": False,
    }


def validate_probe_response(payload: dict, probe_name: str = PROBE_NAME,
                            nonce: str = PROBE_NONCE, protocol: str = "chat_completions") -> tuple[bool, str]:
    if protocol == "responses":
        if not isinstance(payload, dict) or payload.get("status") != "completed":
            return False, f"Responses status={payload.get('status') if isinstance(payload, dict) else None!r}"
        output = payload.get("output")
        calls = [item for item in output if isinstance(item, dict) and item.get("type") == "function_call"] \
            if isinstance(output, list) else []
        if not calls:
            return False, "function_call item 数量=0，期望至少 1"
        for call in calls:
            if call.get("name") != probe_name or not call.get("call_id"):
                return False, "function_call name/call_id 不匹配"
            raw_args = call.get("arguments")
            try:
                arguments = json.loads(raw_args) if isinstance(raw_args, str) else raw_args
            except json.JSONDecodeError:
                return False, "工具参数不是合法 JSON"
            if arguments != {"nonce": nonce}:
                return False, "工具参数未逐值回传必填 nonce"
        return True, "ok"
    choices = payload.get("choices") if isinstance(payload, dict) else None
    if not isinstance(choices, list) or len(choices) != 1:
        return False, "provider 未返回唯一 choice"
    choice = choices[0] if isinstance(choices[0], dict) else {}
    if choice.get("finish_reason") != "tool_calls":
        return False, f"finish_reason={choice.get('finish_reason')!r}，期望 tool_calls"
    message = choice.get("message") if isinstance(choice.get("message"), dict) else {}
    calls = message.get("tool_calls")
    if not isinstance(calls, list) or not calls:
        return False, "tool_calls 数量=0，期望至少 1"
    for call in calls:
        call = call if isinstance(call, dict) else {}
        function = call.get("function") if isinstance(call.get("function"), dict) else {}
        if function.get("name") != probe_name:
            return False, f"工具名={function.get('name')!r}，期望 {probe_name}"
        raw_args = function.get("arguments")
        try:
            arguments = json.loads(raw_args) if isinstance(raw_args, str) else raw_args
        except json.JSONDecodeError:
            return False, "工具参数不是合法 JSON"
        if arguments != {"nonce": nonce}:
            return False, "工具参数未逐值回传必填 nonce"
    return True, "ok"


def verified_ssl_context() -> ssl.SSLContext:
    context = ssl.create_default_context()
    # python.org 的 macOS Framework Python 可能没有执行 Install Certificates，
    # 但系统仍维护 /etc/ssl/cert.pem。只补载系统 CA，绝不关闭证书校验。
    if ssl.get_default_verify_paths().cafile is None:
        for candidate in (
            "/etc/ssl/cert.pem",
            "/etc/ssl/certs/ca-certificates.crt",
            "/etc/pki/tls/certs/ca-bundle.crt",
        ):
            if Path(candidate).is_file():
                context.load_verify_locations(cafile=candidate)
                break
    return context


def urllib_probe_transport(endpoint: str, api_key: str, body: dict, timeout_sec: int) -> tuple[int, dict]:
    request = urllib.request.Request(
        endpoint,
        data=json.dumps(body).encode("utf-8"),
        method="POST",
        headers={
            "Accept": "application/json",
            "Authorization": "Bearer " + api_key,
            "Content-Type": "application/json",
        },
    )
    try:
        with urllib.request.urlopen(
                request, timeout=timeout_sec, context=verified_ssl_context()) as response:
            payload = json.load(response)
            return response.status, payload if isinstance(payload, dict) else {}
    except urllib.error.HTTPError as error:
        try:
            payload = json.loads(error.read().decode("utf-8", errors="replace"))
        except (OSError, UnicodeError, json.JSONDecodeError):
            payload = {}
        return error.code, payload if isinstance(payload, dict) else {}


def run_provider_probe(base_url: str, api_key: str, model: str, protocol: str = "responses",
                       timeout_sec: int = 45, attempts: int = 3, sleep_sec: int = 15,
                       transport=None) -> None:
    if protocol not in {"responses", "chat_completions"}:
        raise ValueError(f"未知 probe protocol={protocol}")
    endpoint = base_url.rstrip("/") + ("/responses" if protocol == "responses" else "/chat/completions")
    probe_name = "agentgo_capability_probe_" + uuid.uuid4().hex[:8]
    nonce = "nonce_" + uuid.uuid4().hex[:12]
    request_body = probe_request(model, probe_name, nonce, protocol)
    transport = transport or urllib_probe_transport
    last_reason = "未知错误"
    for attempt in range(1, attempts + 1):
        try:
            status, payload = transport(endpoint, api_key, request_body, timeout_sec)
            if status != 200:
                last_reason = f"HTTP {status}"
                raise RuntimeError(last_reason)
            ok, reason = validate_probe_response(payload, probe_name, nonce, protocol)
            if ok:
                return
            last_reason = reason
        except (OSError, TimeoutError, json.JSONDecodeError, subprocess.SubprocessError, RuntimeError) as error:
            if not last_reason.startswith("HTTP "):
                last_reason = str(error)[:160] or type(error).__name__
        if attempt < attempts:
            time.sleep(sleep_sec)
    raise RuntimeError(f"function-call 能力探针失败: {last_reason}")


def http_json(url: str, token: str, method: str = "GET", body: dict | None = None,
              timeout: int = 15) -> tuple[int, dict]:
    data = None if body is None else json.dumps(body).encode()
    request = urllib.request.Request(url, data=data, method=method, headers={
        "Accept": "application/json",
        "Authorization": "Bearer " + token,
        **({"Content-Type": "application/json"} if data is not None else {}),
    })
    with urllib.request.urlopen(request, timeout=timeout) as response:
        return response.status, json.load(response)


def inject_request(base_url: str, token: str, prompt_path: str, task_id: str,
                   timeout_sec: int, contract_path: str) -> dict:
    contract = build_run_contract(task_id, timeout_sec)
    prompt = Path(prompt_path).read_text(encoding="utf-8")
    status, response = http_json(base_url.rstrip("/") + "/api/input", token, "POST", {
        "text": prompt,
        "run_contract": contract,
    })
    if status != 200 or response.get("ok") is not True:
        raise RuntimeError(f"/api/input 拒绝 RunContract: HTTP {status}")
    atomic_json(contract_path, contract)
    return contract


def project_snapshot(snapshot: dict, run_id: str) -> dict:
    tasks = [task for task in snapshot.get("tasks", []) if task.get("run_id") == run_id]
    graphs = [graph for graph in snapshot.get("graphs", []) if graph.get("run_id") == run_id]
    graph_terminal = bool(graphs) and all(graph.get("status") in TERMINAL_GRAPH for graph in graphs)
    tasks_terminal = bool(tasks) and all(task.get("status") in TERMINAL_TASK for task in tasks)
    return {
        "task_count": len(tasks),
        "task_statuses": [task.get("status", "") for task in tasks],
        "active_tasks": sum(task.get("status") in {"pending", "processing"} for task in tasks),
        "graph_count": len(graphs),
        "graph_statuses": [graph.get("status", "") for graph in graphs],
        "graph_outcomes": [graph.get("outcome", "") for graph in graphs],
        "graph_terminal": graph_terminal,
        "tasks_terminal": tasks_terminal,
        "pending_interactions": len(snapshot.get("pending_interactions") or []),
    }


def process_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        return True


def monitor_run(base_url: str, token: str, pid: int, run_id: str, started_at: float,
                timeout_sec: int, snapshot_path: str, poll_sec: int = 3,
                terminal_grace_sec: int = 30) -> dict:
    candidate = ""
    candidate_since = 0.0
    last_projection = {}
    observed_activity = False
    identity_projection_seen = False
    while True:
        elapsed = max(0, int(time.time() - started_at))
        if not process_alive(pid):
            terminal = "process_exited"
            break
        if elapsed >= timeout_sec:
            terminal = "external_hard_kill"
            break
        try:
            _, snapshot = http_json(base_url.rstrip("/") + "/api/snapshot", token, timeout=10)
            atomic_json(snapshot_path, snapshot)
            projection = project_snapshot(snapshot, run_id)
            last_projection = projection
            if projection["task_count"] or projection["graph_count"]:
                identity_projection_seen = True
                observed_activity = True
            next_candidate = ""
            if projection["graph_terminal"]:
                next_candidate = "graph_terminal"
            elif projection["graph_count"] == 0 and projection["tasks_terminal"]:
                next_candidate = "no_graph_terminal"
            if next_candidate != candidate:
                candidate = next_candidate
                candidate_since = time.monotonic() if candidate else 0.0
            if candidate and time.monotonic() - candidate_since >= terminal_grace_sec:
                terminal = candidate
                break
        except (OSError, urllib.error.URLError, json.JSONDecodeError):
            pass
        time.sleep(poll_sec)
    return {
        "process_terminal": terminal,
        "external_hard_kill": terminal == "external_hard_kill",
        "wall_sec": max(0, int(time.time() - started_at)),
        "run_identity_visible": identity_projection_seen,
        "observed_activity": observed_activity,
        "graph_lifecycle_terminal": bool(last_projection.get("graph_terminal")),
        "graph_statuses": last_projection.get("graph_statuses", []),
        "graph_outcomes": last_projection.get("graph_outcomes", []),
        "task_statuses": last_projection.get("task_statuses", []),
    }


def safe_outcomes(state_dir: Path, run_id: str) -> tuple[list[dict], int]:
    journal = state_dir / "task-outcomes" / "task-outcomes.jsonl"
    commits: dict[str, dict] = {}
    acknowledgements: set[str] = set()
    for _, entry in iter_jsonl(str(journal)):
        if entry.get("kind") == "delivery_ack" and entry.get("ack_ref"):
            acknowledgements.add(entry["ack_ref"])
        record = entry.get("record") if isinstance(entry.get("record"), dict) else {}
        outcome = record.get("outcome") if isinstance(record.get("outcome"), dict) else {}
        ref = record.get("outcome_ref", "")
        if outcome.get("run_id") != run_id or not ref:
            continue
        commits[ref] = {
            "outcome_ref": ref,
            "task_id": outcome.get("task_id", ""),
            "graph_id": outcome.get("graph_id", ""),
            "node_id": outcome.get("node_id", ""),
            "activation_id": outcome.get("activation_id", ""),
            "attempt_id": outcome.get("attempt_id", ""),
            "attempt_no": outcome.get("attempt_no", 0),
            "status": outcome.get("status", ""),
            "reason_code": outcome.get("reason_code", ""),
            "checkpoint_state": outcome.get("checkpoint_state", ""),
        }
    values = []
    for ref in sorted(commits):
        projected = commits[ref]
        projected["delivery_acked"] = ref in acknowledgements
        values.append(projected)
    return values, sum(not value["delivery_acked"] for value in values)


def trace_metrics(project_root: Path, run_id: str, scheduler_task_ids: set[str]) -> tuple[dict, list[dict]]:
    events = []
    for _, event in iter_jsonl(str(project_root / ".agentgo" / "sessions" / "*" / "logs" / "*.jsonl")):
        if event.get("run_id") == run_id:
            events.append(event)
    events.sort(key=lambda event: event.get("ts", ""))
    llm_ends = [event for event in events if event.get("kind") == "llm_call_end"]
    scheduler_ends = [event for event in llm_ends if event.get("task_id") in scheduler_task_ids]
    failures = collections.Counter(
        event.get("failure_kind") for event in llm_ends if event.get("failure_kind")
    )
    # Provider 返回多个 tool calls 本身不再是事故：机械阶段会只 dispatch
    # 首个并为其余 call_id 写 skipped result，final-report 则允许串行执行多个
    # 只读调用。真正的回归是“非 final-report 的 Scheduler 单动作阶段实际
    # dispatch 了多个工具”。phase 来自冻结 Context manifest，而不是正文猜测。
    scheduler_phases = {}
    for event in events:
        if event.get("kind") != "context_manifest_built" or event.get("task_id") not in scheduler_task_ids:
            continue
        try:
            manifest = json.loads(event.get("description") or "[]")
        except (TypeError, json.JSONDecodeError):
            manifest = []
        for fragment in manifest if isinstance(manifest, list) else []:
            source_ref = fragment.get("source_ref", "") if isinstance(fragment, dict) else ""
            if source_ref.startswith("prompt-phase:"):
                scheduler_phases[event.get("turn_id", "")] = source_ref.removeprefix("prompt-phase:")
                break
    dispatched_by_turn = collections.Counter(
        event.get("turn_id", "") for event in events
        if event.get("kind") == "tool_call" and event.get("task_id") in scheduler_task_ids
    )
    scheduler_batches = [
        turn_id for turn_id, count in dispatched_by_turn.items()
        if count > 1 and scheduler_phases.get(turn_id) != "scheduler:final-report"
    ]
    create_calls = [
        event for event in events
        if event.get("kind") == "tool_call" and event.get("task_id") in scheduler_task_ids
        and event.get("tool") == "create_graph_draft"
    ]
    first_create_index = 0
    if create_calls:
        created_at = create_calls[0].get("ts", "")
        first_create_index = sum(event.get("ts", "") <= created_at for event in scheduler_ends)
    error_text = "\n".join(str(event.get("error", "")) for event in events if event.get("error"))
    known = {
        "fragment_limit_exceeded": "fragment_limit_exceeded" in error_text,
        # provider 400 表示冻结请求 wire 本身不合法，属于 Invocation/Context
        # 架构事故，不能因为 Graph 正常进入 failed 终态就计为 architecture_ok。
        "provider_invalid_request": any(
            event.get("failure_kind") == "invalid_request" for event in llm_ends
        ),
        "invocation_output_limit_exceeded": any(
            event.get("failure_kind") == "output_limit_exceeded" for event in llm_ends
        ),
        "premature_attempt_exhaustion": "attempt" in error_text.lower() and "exhaust" in error_text.lower(),
        "invalid_recovery_deadline": "recovery" in error_text.lower() and "deadline" in error_text.lower() and "invalid" in error_text.lower(),
        "scheduler_tool_batch_exceeded": bool(scheduler_batches),
        "request_timeout_rebuilt_context": any(
            event.get("failure_kind") == "request_timeout" and event.get("recovery_action") == "rebuild_context"
            for event in llm_ends
        ),
    }
    return {
        "model_calls": len(llm_ends),
        "prompt_tokens": sum(int(event.get("prompt_tokens") or 0) for event in llm_ends),
        "completion_tokens": sum(int(event.get("completion_tokens") or 0) for event in llm_ends),
        "first_scheduler_prompt_tokens": int(scheduler_ends[0].get("prompt_tokens") or 0) if scheduler_ends else 0,
        "scheduler_model_calls": len(scheduler_ends),
        "first_graph_draft_call_index": first_create_index,
        "invocation_failures": dict(sorted(failures.items())),
        "known_incidents": known,
    }, events


def context_metrics(state_dir: Path) -> dict:
    dispositions = collections.Counter()
    policies = set()
    snapshots = 0
    for _, entry in iter_jsonl(str(state_dir / "context-snapshots" / "context-snapshots.jsonl")):
        snapshot = (((entry.get("record") or {}).get("snapshot")) or {})
        if not isinstance(snapshot, dict):
            continue
        snapshots += 1
        if snapshot.get("context_policy_id"):
            policies.add(snapshot["context_policy_id"])
        for fragment in snapshot.get("fragments") or []:
            if isinstance(fragment, dict):
                dispositions[fragment.get("disposition", "unknown")] += 1
    return {
        "snapshots": snapshots,
        "policies": sorted(policies),
        "dispositions": dict(sorted(dispositions.items())),
    }


def loop_metrics(state_dir: Path, run_id: str) -> dict:
    attempts = set()
    interventions = 0
    max_no_progress = 0
    sealed = 0
    records = 0
    for _, entry in iter_jsonl(str(state_dir / "loop" / "*.jsonl")):
        candidates = [entry.get("checkpoint"), (entry.get("settlement") or {}).get("checkpoint")]
        matched = False
        for checkpoint in candidates:
            if not isinstance(checkpoint, dict) or checkpoint.get("run_id") != run_id:
                continue
            matched = True
            if checkpoint.get("attempt_id"):
                attempts.add(checkpoint["attempt_id"])
            max_no_progress = max(max_no_progress, int(checkpoint.get("no_progress_turns") or 0))
            interventions = max(interventions, int(checkpoint.get("intervention_count") or 0))
            sealed += int(checkpoint.get("sealed") is True)
        if matched:
            records += 1
    return {
        "records": records,
        "attempt_count": len(attempts),
        "max_no_progress_turns": max_no_progress,
        "max_intervention_count": interventions,
        "sealed_checkpoint_records": sealed,
    }


def count_jsonl(path: Path) -> int:
    try:
        with path.open(encoding="utf-8") as handle:
            return sum(1 for line in handle if line.strip())
    except OSError:
        return 0


def terminal_task_scope(tasks: list[dict], graphs: list[dict]) -> list[dict]:
    # Graph terminal 后，origin/final-report/intervention Scheduler 都是控制面
    # 任务，不属于 Graph execution 的 TaskOutcome delivery barrier。只要求
    # Graph activation tasks 终态；无 Graph 事故路径仍要求当前 Run 全部任务终态。
    if graphs:
        return [task for task in tasks if task.get("graph_id")]
    return tasks


def collect_result(snapshot_path: str, monitor_path: str, project_root: str, run_id: str,
                   startup_probe_passed: bool) -> dict:
    snapshot = read_json(snapshot_path, {})
    monitor = read_json(monitor_path, {})
    root = Path(project_root)
    state_dir = root / ".agentgo" / "state"
    tasks = [task for task in snapshot.get("tasks", []) if task.get("run_id") == run_id]
    graphs = [graph for graph in snapshot.get("graphs", []) if graph.get("run_id") == run_id]
    terminal_tasks = terminal_task_scope(tasks, graphs)
    scheduler_ids = {task.get("id", "") for task in tasks if task.get("event_type") == "__scheduler__"}
    outcomes, pending_ack = safe_outcomes(state_dir, run_id)
    committed_refs = {outcome["outcome_ref"] for outcome in outcomes}
    traces, _ = trace_metrics(root, run_id, scheduler_ids)
    contexts = context_metrics(state_dir)
    loops = loop_metrics(state_dir, run_id)
    known = dict(traces["known_incidents"])
    graph_outcomes = [graph.get("outcome", "") for graph in graphs]
    authoring_events = count_jsonl(state_dir / "graph-authoring" / "authoring.jsonl")
    if any(
        value.get("task_id") in scheduler_ids and value.get("reason_code") == "progress_authority_failure"
        and int(value.get("attempt_no") or 0) >= 3
        for value in outcomes
    ):
        known["premature_attempt_exhaustion"] = True
    if not graph_outcomes and any(
        value.get("task_id") in scheduler_ids and value.get("status") == "completed"
        for value in outcomes
    ):
        known["new_run_direct_answer"] = True
    else:
        known["new_run_direct_answer"] = False
    # Authoring intervention 本身不是事故：成功 create/patch/validate 会形成
    # accepted coordination fingerprint；只有后续重复/失败动作才累计 no-progress。
    # 映射正确性由 durable assessment 与 Go contract test 钉住，不再用
    # “有 authoring journal + 有 intervention”这一粗糙条件制造假阳性。
    known["authoring_false_no_progress"] = False
    architecture_checks = {
        "startup_function_probe": startup_probe_passed,
        "run_identity_visible": bool(monitor.get("run_identity_visible")),
        "first_prompt_at_most_8000": 0 < traces["first_scheduler_prompt_tokens"] <= 8000,
        "graph_draft_within_5_calls": 0 < traces["first_graph_draft_call_index"] <= 5,
        "known_incidents_absent": not any(known.values()),
        "external_hard_kill_absent": not bool(monitor.get("external_hard_kill")),
        "graph_terminal": bool(monitor.get("graph_lifecycle_terminal")),
        "graph_outcome_typed": bool(graph_outcomes) and all(
            value in TERMINAL_OUTCOME for value in graph_outcomes
        ),
        "all_graph_tasks_terminal": bool(terminal_tasks) and all(
            task.get("status") in TERMINAL_TASK for task in terminal_tasks
        ),
        "graph_task_outcomes_complete": bool(terminal_tasks) and all(
            task.get("outcome_ref") in committed_refs for task in terminal_tasks
        ),
        "task_outcome_delivery_acked": bool(outcomes) and pending_ack == 0 and all(
            outcome["delivery_acked"] for outcome in outcomes
        ),
    }
    result = {
        "schema": RESULT_SCHEMA,
        "run_id": run_id,
        "process_terminal": monitor.get("process_terminal", "unknown"),
        "external_hard_kill": bool(monitor.get("external_hard_kill")),
        "wall_sec": int(monitor.get("wall_sec") or 0),
        "graph_lifecycle_terminal": bool(monitor.get("graph_lifecycle_terminal")),
        "graph_statuses": [graph.get("status", "") for graph in graphs],
        "graph_outcomes": graph_outcomes,
        "task_statuses": [task.get("status", "") for task in tasks],
        "task_outcomes": outcomes,
        "pending_outcome_delivery_count": pending_ack,
        "metrics": {
            **traces,
            "context": contexts,
            "loop": loops,
            "graph_authoring_events": authoring_events,
            "graph_revisions": [int(graph.get("revision") or 0) for graph in graphs],
            "graph_activations": sum(
                1 for graph in graphs for node in graph.get("nodes", []) if node.get("activation_id")
            ),
            "effect_records": count_jsonl(state_dir / "effects.jsonl"),
            "artifact_records": count_jsonl(state_dir / "artifacts.jsonl"),
        },
        "known_incidents": known,
        "architecture_checks": architecture_checks,
        "architecture_ok": all(architecture_checks.values()),
    }
    return result


def finalize_result(result_path: str, judge_path: str) -> dict:
    result = read_json(result_path, {})
    judge = read_json(judge_path, {})
    task_checks = {
        "judge_resolved": judge.get("verdict") == "resolved",
        "patch_present": int(judge.get("patch_lines") or 0) > 0,
        "tests_not_tampered": judge.get("tampered") is False,
        "graph_success": bool(result.get("graph_outcomes")) and all(
            value == "success" for value in result.get("graph_outcomes", [])
        ),
    }
    result["judge_verdict"] = judge.get("verdict", "unknown")
    result["patch_lines"] = int(judge.get("patch_lines") or 0)
    result["task_checks"] = task_checks
    result["task_resolved"] = all(task_checks.values())
    atomic_json(result_path, result)
    return result


def summarize_runs(runs_dir: str, batch_start: float) -> list[dict]:
    rows = []
    for directory in sorted(Path(runs_dir).iterdir() if Path(runs_dir).is_dir() else []):
        if not directory.is_dir() or directory.name.startswith(("_", ".")):
            continue
        result_path = directory / "result.json"
        judge_path = directory / "judge.json"
        if not result_path.exists() and not judge_path.exists():
            continue
        result = read_json(result_path, {})
        judge = read_json(judge_path, {})
        stale = any(not path.exists() or path.stat().st_mtime < batch_start for path in (result_path, judge_path))
        metrics = result.get("metrics") if isinstance(result.get("metrics"), dict) else {}
        rows.append({
            "task": directory.name,
            "verdict": judge.get("verdict", "unknown"),
            "architecture_ok": bool(result.get("architecture_ok")),
            "task_resolved": bool(result.get("task_resolved")),
            "process_terminal": result.get("process_terminal", "unknown"),
            "graph_outcomes": result.get("graph_outcomes", []),
            "wall_sec": int(result.get("wall_sec") or 0),
            "prompt_tokens": int(metrics.get("prompt_tokens") or 0),
            "completion_tokens": int(metrics.get("completion_tokens") or 0),
            "llm_calls": int(metrics.get("model_calls") or 0),
            "patch_lines": int(judge.get("patch_lines") or 0),
            "external_hard_kill": bool(result.get("external_hard_kill")),
            "known_incidents": [name for name, value in (result.get("known_incidents") or {}).items() if value],
            "stale": stale,
        })
    return rows


def run_command(command: list[str], cwd: str | Path | None = None, check: bool = True,
                input_data: bytes | None = None, timeout: int | None = None) -> subprocess.CompletedProcess:
    completed = subprocess.run(
        command,
        cwd=str(cwd) if cwd is not None else None,
        input=input_data,
        capture_output=True,
        check=False,
        timeout=timeout,
    )
    if check and completed.returncode != 0:
        detail = (completed.stderr or completed.stdout).decode("utf-8", errors="replace").strip()
        detail = detail[-1200:] if detail else "无输出"
        raise RuntimeError(f"命令失败 exit={completed.returncode} program={Path(command[0]).name}: {detail}")
    return completed


def safe_remove_worktree(config: HarnessConfig, target: Path) -> None:
    worktrees = (config.testbed / "worktrees").resolve()
    if target.parent.resolve() != worktrees or target == worktrees:
        raise ValueError(f"拒绝清理非考题 worktree: {target}")
    run_command(
        ["git", "-C", str(config.flask_repo), "worktree", "remove", "--force", str(target)],
        check=False,
    )
    if target.is_symlink():
        target.unlink()
    elif target.exists():
        shutil.rmtree(target)


def venv_python(worktree: Path) -> Path:
    candidates = (
        worktree / ".venv" / "bin" / "python",
        worktree / ".venv" / "Scripts" / "python.exe",
    )
    for candidate in candidates:
        if candidate.is_file():
            return candidate
    raise RuntimeError(f"虚拟环境 Python 不存在: {worktree / '.venv'}")


def setup_task(config: HarnessConfig, task: TaskSpec, target: Path | None = None) -> Path:
    worktree = target or config.worktree(task.task_id)
    worktree.parent.mkdir(parents=True, exist_ok=True)
    safe_remove_worktree(config, worktree)
    run_command(["git", "clone", "--no-local", "--quiet", str(config.flask_repo), str(worktree)])
    run_command(["git", "checkout", "--quiet", "--detach", task.fix_sha + "^"], cwd=worktree)
    refs = run_command(["git", "for-each-ref", "--format=%(refname)"], cwd=worktree)
    for ref in refs.stdout.decode("utf-8", errors="replace").splitlines():
        if ref:
            run_command(["git", "update-ref", "-d", ref], cwd=worktree)
    run_command(["git", "reflog", "expire", "--expire=now", "--all"], cwd=worktree)
    run_command(["git", "gc", "--prune=now", "--quiet"], cwd=worktree)
    run_command(["git", "remote", "remove", "origin"], cwd=worktree, check=False)
    run_command(
        ["uv", "sync", "--frozen", "--no-default-groups", "--group", "tests", "--python", "3.13"],
        cwd=worktree,
    )
    python = venv_python(worktree)
    info = run_command([
        str(python), "-c",
        "import importlib.metadata as m,sys; "
        "print(f'env-ok: flask={m.version(\"flask\")} pytest={m.version(\"pytest\")} py={sys.version.split()[0]}')",
    ], cwd=worktree)
    print("[环境准备] " + info.stdout.decode("utf-8", errors="replace").strip())
    print(f"[环境准备] worktree 就绪: {worktree}")
    return worktree


def patch_from_fix(config: HarnessConfig, task: TaskSpec, pathspec: str) -> bytes:
    completed = run_command([
        "git", "-C", str(config.flask_repo), "show", task.fix_sha, "--", pathspec,
    ])
    if not completed.stdout:
        raise RuntimeError(f"考题 {task.task_id} 的 {pathspec} patch 为空")
    return completed.stdout


def apply_fix_slice(config: HarnessConfig, task: TaskSpec, worktree: Path, pathspec: str) -> None:
    run_command(["git", "apply", "-"], cwd=worktree,
                input_data=patch_from_fix(config, task, pathspec))


def parse_junit(path: Path) -> dict:
    try:
        root = ET.parse(path).getroot()
    except (OSError, ET.ParseError) as error:
        raise RuntimeError(f"pytest 未产生合法 JUnit 报告: {path}") from error
    suites = [root] if root.tag == "testsuite" else list(root.findall("testsuite"))
    tests = sum(int(suite.attrib.get("tests", 0)) for suite in suites)
    failed = sum(int(suite.attrib.get("failures", 0)) for suite in suites)
    errors = sum(int(suite.attrib.get("errors", 0)) for suite in suites)
    skipped = sum(int(suite.attrib.get("skipped", 0)) for suite in suites)
    return {
        "tests": tests,
        "passed": max(0, tests - failed - errors - skipped),
        "failed": failed,
        "errors": errors,
        "skipped": skipped,
    }


def run_pytest(worktree: Path, junit_path: Path, log_path: Path,
               test_files: tuple[str, ...] = ()) -> dict:
    junit_path.parent.mkdir(parents=True, exist_ok=True)
    junit_path.unlink(missing_ok=True)
    command = [
        str(venv_python(worktree)), "-m", "pytest", *test_files, "-q",
        f"--junitxml={junit_path}",
    ]
    completed = run_command(command, cwd=worktree, check=False)
    output = completed.stdout + completed.stderr
    log_path.write_bytes(output)
    result = parse_junit(junit_path)
    result["exit_code"] = completed.returncode
    result["summary_tail"] = output.decode("utf-8", errors="replace").splitlines()[-3:]
    return result


def print_stage_header(task_id: str, index: int, total: int, title: str,
                       scope: str, objective: str) -> None:
    print(f"\n[第{index}/{total}阶段][{title}][task={task_id}]")
    print(f"测试内容：{title}")
    print(f"测试范围：{scope}")
    print(f"判定目标：{objective}")


def print_pytest_stage_result(result: dict, expectation: str) -> bool:
    tail = result.get("summary_tail") or []
    if tail:
        print("pytest 原始摘要：")
        print("\n".join(str(line) for line in tail))
    if expectation == "red":
        matched = int(result.get("failed") or 0) + int(result.get("errors") or 0) > 0
        conclusion = "符合预期红态，目标缺陷已复现" if matched else "未形成预期红态，考题无效"
    elif expectation == "green":
        matched = (
            int(result.get("failed") or 0) == 0
            and int(result.get("errors") or 0) == 0
            and int(result.get("passed") or 0) > 0
        )
        conclusion = "测试通过" if matched else "测试未通过"
    else:
        raise ValueError(f"未知 pytest 阶段期望: {expectation}")
    print(
        f"阶段结果：{conclusion}；tests={int(result.get('tests') or 0)} "
        f"passed={int(result.get('passed') or 0)} failed={int(result.get('failed') or 0)} "
        f"errors={int(result.get('errors') or 0)} skipped={int(result.get('skipped') or 0)}"
    )
    return matched


def prepare_task(config: HarnessConfig, task: TaskSpec) -> dict:
    worktree = setup_task(config, task)
    run_dir = config.run_dir(task.task_id)
    run_dir.mkdir(parents=True, exist_ok=True)
    apply_fix_slice(config, task, worktree, "tests/")
    run_command(["git", "add", "-A", "--", "tests/"], cwd=worktree)
    run_command([
        "git", "-c", "user.name=agentgo-swe", "-c", "user.email=swe@harness.local",
        "commit", "-q", "-m", f"harness: 预置期望行为测试（考题 {task.task_id}）", "--", "tests/",
    ], cwd=worktree)

    print_stage_header(
        task.task_id, 1, 4, "目标测试红态确认",
        " ".join(task.test_files),
        "修复前至少出现 1 个 failed/error，证明目标缺陷可以稳定复现",
    )
    red = run_pytest(
        worktree, run_dir / "targeted-baseline.junit.xml",
        run_dir / "targeted-baseline.pytest.log", task.test_files,
    )
    if not print_pytest_stage_result(red, "red"):
        raise RuntimeError(f"未确认红状态，考题 {task.task_id} 准备失败")
    print_stage_header(
        task.task_id, 2, 4, "全量测试红态基线",
        "当前 Flask worktree 的完整 pytest 测试集",
        "记录 Agent 执行前的全量基线；允许目标缺陷失败，但必须准确保存 failed/error 计数",
    )
    baseline = run_pytest(
        worktree, run_dir / "baseline.junit.xml", run_dir / "baseline.pytest.log",
    )
    if not print_pytest_stage_result(baseline, "red"):
        raise RuntimeError(f"全量基线未保持红态，考题 {task.task_id} 准备失败")
    report = {
        key: baseline[key] for key in ("tests", "passed", "failed", "errors", "skipped", "exit_code")
    }
    report["note"] = "base 红态基线（agent 运行前全量 pytest）"
    atomic_json(run_dir / "baseline.json", report)
    (run_dir / "fix_sha").write_text(task.fix_sha + "\n", encoding="utf-8")
    print(f"阶段结论：考题 {task.task_id} 红态准备完成，进入 AgentGo 修复执行")
    return report


def yaml_template_value(value: str | Path) -> str:
    raw = str(value)
    if "\n" in raw or "\r" in raw:
        raise ValueError("SWE 配置值不得包含换行")
    return raw.replace("\\", "\\\\").replace('"', '\\"')


def render_setting(config: HarnessConfig, worktree: Path, run_dir: Path,
                   port: int, token: str) -> Path:
    template_path = config.agentgo_root / "setting.swe-flask.yaml"
    rendered = template_path.read_text(encoding="utf-8")
    replacements = {
        "__PROJECT_ROOT__": worktree,
        "__PORT__": str(port),
        "__TOKEN__": token,
        "__AGENTGO_ROOT__": config.agentgo_root,
        "__BASE_URL__": config.base_url,
        "__MODEL__": config.model,
        "__PROTOCOL__": config.protocol,
        "__KEY_VAR__": "SWE_API_KEY",
    }
    for marker, value in replacements.items():
        if marker not in rendered:
            raise RuntimeError(f"SWE 配置模板缺少占位符 {marker}")
        rendered = rendered.replace(marker, yaml_template_value(value))
    remaining = sorted(set(re.findall(r"__[A-Z0-9_]+__", rendered)))
    if remaining:
        raise RuntimeError(f"SWE 配置模板仍有未解析占位符: {remaining}")
    target = run_dir / "setting.yaml"
    temp = target.with_suffix(".yaml.tmp")
    temp.write_text(rendered, encoding="utf-8")
    os.replace(temp, target)
    return target


def reserve_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
        listener.bind(("127.0.0.1", 0))
        return int(listener.getsockname()[1])


def health_ready(url: str, timeout: float = 1.0) -> bool:
    try:
        with urllib.request.urlopen(url, timeout=timeout) as response:
            return 200 <= response.status < 300
    except (OSError, urllib.error.URLError):
        return False


def wait_for_agentgo(process: subprocess.Popen, base_url: str, log_path: Path,
                     timeout_sec: int = 90) -> None:
    deadline = time.monotonic() + timeout_sec
    ready = False
    while time.monotonic() < deadline:
        if process.poll() is not None:
            break
        if health_ready(base_url.rstrip("/") + "/healthz"):
            ready = True
            break
        time.sleep(1)
    if not ready:
        raise RuntimeError(f"SPAWN_ERROR: healthz 未就绪，见 {log_path}")

    marker = re.compile(r"\[OK\].*typed function-call/required-arguments")
    while time.monotonic() < deadline:
        try:
            if marker.search(log_path.read_text(encoding="utf-8", errors="replace")):
                return
        except OSError:
            pass
        if process.poll() is not None:
            break
        time.sleep(0.2)
    raise RuntimeError("SPAWN_ERROR: 产品 function-call probe 没有成功证据")


def terminate_process(process: subprocess.Popen) -> None:
    if process.poll() is not None:
        return
    try:
        if os.name == "posix":
            os.killpg(process.pid, signal.SIGTERM)
        else:
            process.terminate()
        process.wait(timeout=2)
        return
    except (OSError, subprocess.TimeoutExpired):
        pass
    try:
        if os.name == "posix":
            os.killpg(process.pid, signal.SIGKILL)
        else:
            process.kill()
        process.wait(timeout=2)
    except (OSError, subprocess.TimeoutExpired):
        pass


def clean_run_outputs(run_dir: Path) -> None:
    for name in (
        "result.json", "judge.json", "snapshot.final.json", "monitor.json",
        "run_contract.json", "model.patch", "judge.junit.xml", "judge.pytest.log",
    ):
        (run_dir / name).unlink(missing_ok=True)


def run_task(config: HarnessConfig, task: TaskSpec, timeout_sec: int) -> dict:
    if timeout_sec < 240:
        raise ValueError("timeout 必须至少 240 秒")
    if not os.environ.get("SWE_API_KEY"):
        raise RuntimeError("密钥环境变量 SWE_API_KEY 未设置")
    worktree = config.worktree(task.task_id)
    prompt = config.prompt_dir / f"{task.task_id}.md"
    if not worktree.is_dir():
        raise RuntimeError(f"worktree 不存在: {worktree}（先执行 prepare）")
    if not prompt.is_file():
        raise RuntimeError(f"prompt 不存在: {prompt}")
    if not config.agentgo_bin.is_file():
        raise RuntimeError(f"AgentGo 二进制不存在: {config.agentgo_bin}")

    print_stage_header(
        task.task_id, 3, 4, "AgentGo 修复执行",
        f"worktree={worktree}",
        "完成 Graph commit/start、代码修改、Acceptance 与 typed Graph outcome，并安全收口进程",
    )
    run_dir = config.run_dir(task.task_id)
    run_dir.mkdir(parents=True, exist_ok=True)
    clean_run_outputs(run_dir)
    port = reserve_port()
    token = secrets.token_hex(16)
    setting = render_setting(config, worktree, run_dir, port, token)
    base_url = f"http://127.0.0.1:{port}"
    started_at = time.time()
    log_path = run_dir / "agentgo.log"
    popen_options = {"start_new_session": True} if os.name == "posix" else {}
    with log_path.open("wb") as log_handle:
        process = subprocess.Popen(
            [str(config.agentgo_bin), "-config", str(setting)],
            cwd=worktree,
            stdout=log_handle,
            stderr=subprocess.STDOUT,
            **popen_options,
        )
        print(f"执行状态：AgentGo 已启动 pid={process.pid} port={port}")
        try:
            wait_for_agentgo(process, base_url, log_path)
            contract = inject_request(
                base_url, token, str(prompt), task.task_id, timeout_sec,
                str(run_dir / "run_contract.json"),
            )
            print("执行状态：RunContract 注入成功 " + json.dumps({
                "inject": 200, "run_id": contract["run_id"],
            }, ensure_ascii=False))
            monitor = monitor_run(
                base_url, token, process.pid, contract["run_id"], started_at, timeout_sec,
                str(run_dir / "snapshot.final.json"), poll_sec=3, terminal_grace_sec=30,
            )
            atomic_json(run_dir / "monitor.json", monitor)
            print("执行状态：Graph/进程监控终态 " + json.dumps(monitor, ensure_ascii=False))
            try:
                status, snapshot = http_json(base_url + "/api/snapshot", token)
                if status == 200:
                    atomic_json(run_dir / "snapshot.final.json", snapshot)
            except (OSError, urllib.error.URLError, json.JSONDecodeError):
                pass
        finally:
            terminate_process(process)

    contract = read_json(run_dir / "run_contract.json", {})
    run_id = contract.get("run_id")
    if not run_id:
        raise RuntimeError("AgentGo 运行未产生 run_id")
    result = collect_result(
        str(run_dir / "snapshot.final.json"), str(run_dir / "monitor.json"),
        str(worktree), run_id, True,
    )
    atomic_json(run_dir / "result.json", result)
    print(
        f"阶段结果：AgentGo 执行结束；architecture_ok={result.get('architecture_ok', False)} "
        f"graph_outcomes={result.get('graph_outcomes', [])} "
        f"external_hard_kill={result.get('external_hard_kill', False)}"
    )
    print("运行结果明细：" + json.dumps(result, ensure_ascii=False))
    return result


def test_files_at_fix(config: HarnessConfig, task: TaskSpec) -> list[str]:
    completed = run_command([
        "git", "-C", str(config.flask_repo), "show", task.fix_sha,
        "--name-only", "--format=", "--", "tests/",
    ])
    files = [line for line in completed.stdout.decode("utf-8", errors="replace").splitlines() if line]
    for name in files:
        path = Path(name)
        if path.is_absolute() or ".." in path.parts or not name.replace("\\", "/").startswith("tests/"):
            raise RuntimeError(f"fix commit 返回非法测试路径: {name!r}")
    return files


def judge_task(config: HarnessConfig, task: TaskSpec) -> dict:
    worktree = config.worktree(task.task_id)
    run_dir = config.run_dir(task.task_id)
    run_dir.mkdir(parents=True, exist_ok=True)
    print_stage_header(
        task.task_id, 4, 4, "最终全量 Judge",
        "AgentGo 修改后的完整 Flask pytest 测试集",
        "failed=0、errors=0 且至少 1 个测试通过；同时检查测试文件未被篡改并生成 model.patch",
    )
    pytest = run_pytest(
        worktree, run_dir / "judge.junit.xml", run_dir / "judge.pytest.log",
    )
    pytest_green = print_pytest_stage_result(pytest, "green")
    verdict = "resolved" if pytest_green else "failed"
    test_files = test_files_at_fix(config, task)
    tampered_files: list[str] = []
    for name in test_files:
        expected = run_command([
            "git", "-C", str(config.flask_repo), "cat-file", "-e", f"{task.fix_sha}:{name}",
        ], check=False)
        actual_path = worktree / name
        if expected.returncode == 0:
            expected_bytes = run_command([
                "git", "-C", str(config.flask_repo), "show", f"{task.fix_sha}:{name}",
            ]).stdout
            actual_digest = hashlib.sha256(actual_path.read_bytes()).digest() if actual_path.is_file() else None
            if actual_digest != hashlib.sha256(expected_bytes).digest():
                tampered_files.append(name)
        elif actual_path.exists():
            tampered_files.append(name + "(应已删除)")
    tampered = bool(tampered_files)
    if tampered:
        verdict = "test_tampered"
        print("测试篡改检测: " + " ".join(tampered_files))

    excludes = [":!.venv", ":!.agentgo", *(f":!{name}" for name in test_files)]
    run_command(["git", "reset", "-q"], cwd=worktree, check=False)
    run_command(["git", "add", "-A", "--", ".", *excludes], cwd=worktree)
    patch = run_command(["git", "diff", "--cached", "--", ".", *excludes], cwd=worktree).stdout
    (run_dir / "model.patch").write_bytes(patch)
    patch_lines = len(patch.splitlines())
    report = {
        "verdict": verdict,
        "tests": pytest["tests"],
        "passed": pytest["passed"],
        "failed": pytest["failed"],
        "errors": pytest["errors"],
        "skipped": pytest["skipped"],
        "patch_lines": patch_lines,
        "tampered": tampered,
    }
    baseline = read_json(run_dir / "baseline.json", {})
    if all(key in baseline for key in ("passed", "failed", "errors")):
        report["baseline"] = {key: baseline[key] for key in ("passed", "failed", "errors")}
        if verdict == "failed":
            if report["failed"] == baseline["failed"] and report["errors"] == baseline["errors"]:
                report["red_note"] = "红态与基线完全一致：补丁未造成新增破坏（但也未修复）"
            elif report["failed"] + report["errors"] > baseline["failed"] + baseline["errors"]:
                report["red_note"] = "红态重于基线：补丁引入了新增破坏"
            else:
                report["red_note"] = "红态轻于基线：部分修复但未全绿"
    atomic_json(run_dir / "judge.json", report)
    print(
        f"阶段结论：最终 Judge verdict={report['verdict']} patch_lines={report['patch_lines']} "
        f"tampered={report['tampered']}"
    )
    print("Judge 结构化结果：" + json.dumps(report, ensure_ascii=False))
    return report


def final_exit_code(result: dict) -> int:
    if not result.get("architecture_ok"):
        return EXIT_ARCHITECTURE_FAILURE
    if not result.get("task_resolved"):
        return EXIT_TASK_FAILURE
    return 0


def batch_exit_code(rows: list[dict], expected_count: int) -> int:
    if len(rows) != expected_count or any(
            row.get("stale") or not row.get("architecture_ok") for row in rows):
        return EXIT_ARCHITECTURE_FAILURE
    if any(not row.get("task_resolved") for row in rows):
        return EXIT_TASK_FAILURE
    return 0


def execute_task(config: HarnessConfig, task: TaskSpec, timeout_sec: int) -> dict:
    prepare_task(config, task)
    run_task(config, task, timeout_sec)
    judge_task(config, task)
    result = finalize_result(
        str(config.run_dir(task.task_id) / "result.json"),
        str(config.run_dir(task.task_id) / "judge.json"),
    )
    print("任务最终结论：" + json.dumps({
        "architecture_ok": result.get("architecture_ok", False),
        "task_resolved": result.get("task_resolved", False),
        "judge_verdict": result.get("judge_verdict", "unknown"),
    }, ensure_ascii=False))
    return result


def print_summary(rows: list[dict]) -> None:
    resolved = sum(row["task_resolved"] and not row["stale"] for row in rows)
    architecture = sum(row["architecture_ok"] and not row["stale"] for row in rows)
    print(f"\ntask_resolved {resolved}/{len(rows)} architecture_ok {architecture}/{len(rows)}")
    for row in rows:
        flags = []
        if row["stale"]:
            flags.append("STALE")
        if row["external_hard_kill"]:
            flags.append("HARD_KILL")
        if row["known_incidents"]:
            flags.append("INCIDENT=" + ",".join(row["known_incidents"]))
        print(
            f"  {row['task']:24s} verdict={row['verdict']:13s} arch={str(row['architecture_ok']):5s} "
            f"terminal={row['process_terminal']:18s} wall={row['wall_sec']:4d}s "
            f"calls={row['llm_calls']:3d} patch={row['patch_lines']:4d} {' '.join(flags)}"
        )


def require_api_key() -> str:
    value = os.environ.get("SWE_API_KEY")
    if not value:
        raise RuntimeError("密钥环境变量 SWE_API_KEY 未设置")
    return value


def preflight_probe(config: HarnessConfig, timeout_sec: int = 45) -> None:
    print("\n[前置检查][Provider typed function-call 能力探针]")
    print(
        f"检查内容：provider={config.base_url.rstrip('/')} "
        f"protocol={config.protocol} model={config.model}"
    )
    print("判定目标：返回工具名、call_id 与 nonce 参数均正确的 typed function call")
    run_provider_probe(
        config.base_url, require_api_key(), config.model, config.protocol, timeout_sec,
    )
    print(
        f"检查结果：typed function-call 活探针通过；provider={config.base_url.rstrip('/')} "
        f"protocol={config.protocol} model={config.model}"
    )


def verify_candidates(config: HarnessConfig) -> dict[str, int]:
    report_path = config.testbed / "harness" / "candidates_report.txt"
    report_path.parent.mkdir(parents=True, exist_ok=True)
    lines: list[str] = []
    counts = collections.Counter()
    for task in load_tasks(config.tasks_file):
        heading = f"=== {task.task_id} ({task.fix_sha}) {task.title}"
        lines.append(heading)
        print(f"\n{heading}")
        target = config.testbed / "worktrees" / f"verify-{task.task_id}"
        try:
            worktree = setup_task(config, task, target=target)
            run_dir = config.testbed / "runs" / f"verify-{task.task_id}"
            print_stage_header(
                task.task_id, 1, 3, "候选题干净基线",
                "修复前提交的完整 pytest 测试集（尚未应用 golden tests）",
                "failed=0 且 errors=0，排除环境或历史基线故障",
            )
            clean = run_pytest(
                worktree, run_dir / "clean.junit.xml", run_dir / "clean.pytest.log",
            )
            clean_ok = print_pytest_stage_result(clean, "green")
            if not clean_ok:
                status = "ENV-FAIL"
                detail = "干净 base 不全绿"
            else:
                apply_fix_slice(config, task, worktree, "tests/")
                print_stage_header(
                    task.task_id, 2, 3, "候选题 golden tests 红态",
                    "应用 golden tests 后的目标测试文件",
                    "至少出现 1 个 failed/error，证明候选题能复现目标缺陷",
                )
                red = run_pytest(
                    worktree, run_dir / "red.junit.xml", run_dir / "red.pytest.log", task.test_files,
                )
                red_ok = print_pytest_stage_result(red, "red")
                if not red_ok:
                    status = "INVALID"
                    detail = "base+test patch 不红"
                else:
                    apply_fix_slice(config, task, worktree, "src/")
                    print_stage_header(
                        task.task_id, 3, 3, "候选题 source fix 绿态",
                        "应用 golden source fix 后的完整 pytest 测试集",
                        "failed=0 且 errors=0，证明候选题存在有效官方修复",
                    )
                    green = run_pytest(
                        worktree, run_dir / "green.junit.xml", run_dir / "green.pytest.log",
                    )
                    green_ok = print_pytest_stage_result(green, "green")
                    if not green_ok:
                        status = "FIX-FAIL"
                        detail = "打完 fix 仍有失败"
                    else:
                        status = "OK"
                        detail = f"green passed={green['passed']}"
        except Exception as error:
            status = "ENV-FAIL"
            detail = str(error)[:240]
        counts[status] += 1
        line = f"{status:8s} {task.task_id}: {detail}"
        lines.append(line)
        print(line)
    lines.append("--- 汇总: " + " ".join(
        f"{name}={counts[name]}" for name in ("OK", "INVALID", "ENV-FAIL", "FIX-FAIL")
    ))
    report_path.write_text("\n".join(lines) + "\n", encoding="utf-8")
    return dict(counts)


def command_probe(args: argparse.Namespace) -> int:
    config = HarnessConfig.from_env()
    preflight_probe(config, args.timeout)
    return 0


def command_task(args: argparse.Namespace) -> int:
    config = HarnessConfig.from_env()
    print(f"\n===== 启动单题任务：{args.task_id} =====")
    preflight_probe(config, args.probe_timeout)
    result = execute_task(config, find_task(config, args.task_id), args.timeout)
    return final_exit_code(result)


def command_batch(args: argparse.Namespace) -> int:
    config = HarnessConfig.from_env()
    tasks = load_tasks(config.tasks_file)
    preflight_probe(config, args.probe_timeout)
    batch_start = time.time()
    runs_dir = config.testbed / "runs"
    runs_dir.mkdir(parents=True, exist_ok=True)
    (runs_dir / ".batch_start").write_text(str(batch_start) + "\n", encoding="utf-8")
    for task in tasks:
        print(
            f"\n===== 批次任务 {dt.datetime.now().strftime('%H:%M:%S')} "
            f"{task.task_id}: {task.title} ====="
        )
        result = execute_task(config, task, args.timeout)
        if not result.get("architecture_ok"):
            print(f"架构门失败，停止批次: {task.task_id}", file=os.sys.stderr)
            break
    selected = {task.task_id for task in tasks}
    rows = [row for row in summarize_runs(str(runs_dir), batch_start) if row["task"] in selected]
    atomic_json(runs_dir / "summary.json", rows)
    print_summary(rows)
    return batch_exit_code(rows, len(tasks))


def command_verify_candidates(_args: argparse.Namespace) -> int:
    counts = verify_candidates(HarnessConfig.from_env())
    return 0 if counts.get("OK", 0) > 0 and sum(
        count for name, count in counts.items() if name != "OK"
    ) == 0 else EXIT_TASK_FAILURE


def parser() -> argparse.ArgumentParser:
    root = argparse.ArgumentParser(description=__doc__)
    commands = root.add_subparsers(required=True)

    probe = commands.add_parser("probe", help="运行批次级真实 function-call 能力探针")
    probe.add_argument("--timeout", type=int, default=45)
    probe.set_defaults(func=command_probe)

    task = commands.add_parser("task", help="探针后完整执行 prepare -> run -> judge")
    task.add_argument("task_id")
    task.add_argument("--timeout", type=int, default=1200)
    task.add_argument("--probe-timeout", type=int, default=45)
    task.set_defaults(func=command_task)

    batch = commands.add_parser("batch", help="探针后串行执行 tasks.csv 全部考题")
    batch.add_argument("--timeout", type=int, default=1200)
    batch.add_argument("--probe-timeout", type=int, default=45)
    batch.set_defaults(func=command_batch)

    verify = commands.add_parser("verify-candidates", help="验证候选题目的红到绿语义")
    verify.set_defaults(func=command_verify_candidates)
    return root


def main() -> int:
    args = parser().parse_args()
    try:
        return int(args.func(args) or 0)
    except Exception as error:  # harness 顶层只打印有界类型/消息，绝不打印请求正文或凭证。
        print(f"SWE_HARNESS_ERROR: {error}", file=os.sys.stderr)
        return EXIT_HARNESS_FAILURE


if __name__ == "__main__":
    raise SystemExit(main())
