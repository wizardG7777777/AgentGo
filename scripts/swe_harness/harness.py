#!/usr/bin/env python3
"""AgentGo SWE 系统回归的无第三方依赖权威工具。

本文件只处理运行契约、能力探针、终态判定和脱敏指标。Flask 题目、worktree、
原始日志和凭证始终留在外部 testbed；输出不得包含 prompt、reasoning 或工具参数。
"""

from __future__ import annotations

import argparse
import collections
import datetime as dt
import glob
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import uuid


RUN_SCHEMA = "agentgo.run-contract/v1"
RESULT_SCHEMA = "agentgo.swe-result/v2"
PROBE_NAME = "agentgo_capability_probe_test"
TERMINAL_TASK = {"completed", "failed", "blocked", "cancelled"}
TERMINAL_GRAPH = {"completed", "failed", "blocked", "cancelled"}
TERMINAL_OUTCOME = {"success", "failed", "blocked", "cancelled"}
NANOSECOND = 1_000_000_000


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


def probe_request(model: str, probe_name: str) -> dict:
    return {
        "model": model,
        "messages": [{
            "role": "user",
            "content": "Call the provided function exactly once. Do not answer with text.",
        }],
        "tools": [{
            "type": "function",
            "function": {
                "name": probe_name,
                "description": "Prove function-calling compatibility with an empty JSON object.",
                "parameters": {
                    "type": "object",
                    "additionalProperties": False,
                    "properties": {},
                },
            },
        }],
        "tool_choice": {"type": "function", "function": {"name": probe_name}},
        "max_tokens": 256,
        "stream": False,
    }


def validate_probe_response(payload: dict, probe_name: str = PROBE_NAME) -> tuple[bool, str]:
    choices = payload.get("choices") if isinstance(payload, dict) else None
    if not isinstance(choices, list) or len(choices) != 1:
        return False, "provider 未返回唯一 choice"
    choice = choices[0] if isinstance(choices[0], dict) else {}
    if choice.get("finish_reason") != "tool_calls":
        return False, f"finish_reason={choice.get('finish_reason')!r}，期望 tool_calls"
    message = choice.get("message") if isinstance(choice.get("message"), dict) else {}
    calls = message.get("tool_calls")
    if not isinstance(calls, list) or len(calls) != 1:
        return False, f"tool_calls 数量={len(calls) if isinstance(calls, list) else 0}，期望 1"
    call = calls[0] if isinstance(calls[0], dict) else {}
    function = call.get("function") if isinstance(call.get("function"), dict) else {}
    if function.get("name") != probe_name:
        return False, f"工具名={function.get('name')!r}，期望 {probe_name}"
    raw_args = function.get("arguments")
    try:
        arguments = json.loads(raw_args) if isinstance(raw_args, str) else raw_args
    except json.JSONDecodeError:
        return False, "工具参数不是合法 JSON"
    if arguments != {}:
        return False, "工具参数不是严格空 JSON object"
    return True, "ok"


def curl_probe_transport(endpoint: str, api_key: str, body: dict, timeout_sec: int) -> tuple[int, dict]:
    paths = []
    try:
        for content in (
            "Content-Type: application/json\nAuthorization: Bearer " + api_key + "\n",
            json.dumps(body),
            "",
        ):
            handle = tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", delete=False)
            os.chmod(handle.name, 0o600)
            handle.write(content)
            handle.close()
            paths.append(handle.name)
        command = [
            "curl", "--silent", "--show-error", "--max-time", str(timeout_sec),
            "--output", paths[2], "--write-out", "%{http_code}", "--request", "POST",
            "--header", "@" + paths[0], "--data-binary", "@" + paths[1], endpoint,
        ]
        completed = subprocess.run(command, check=False, capture_output=True, text=True, timeout=timeout_sec + 5)
        if completed.returncode != 0:
            raise RuntimeError(f"curl_exit_{completed.returncode}")
        try:
            status = int(completed.stdout.strip())
        except ValueError as error:
            raise RuntimeError("curl 未返回 HTTP 状态") from error
        payload = read_json(paths[2], {})
        return status, payload
    finally:
        for path in paths:
            try:
                os.unlink(path)
            except OSError:
                pass


def run_provider_probe(base_url: str, api_key: str, model: str, timeout_sec: int = 45,
                       attempts: int = 3, sleep_sec: int = 15, transport=None) -> None:
    endpoint = base_url.rstrip("/") + "/chat/completions"
    probe_name = "agentgo_capability_probe_" + uuid.uuid4().hex[:8]
    request_body = probe_request(model, probe_name)
    transport = transport or curl_probe_transport
    last_reason = "未知错误"
    for attempt in range(1, attempts + 1):
        try:
            status, payload = transport(endpoint, api_key, request_body, timeout_sec)
            if status != 200:
                last_reason = f"HTTP {status}"
                raise RuntimeError(last_reason)
            ok, reason = validate_probe_response(payload, probe_name)
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
    scheduler_batches = [
        event for event in scheduler_ends if int(event.get("tool_calls_count") or 0) > 1
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


def command_probe(args: argparse.Namespace) -> None:
    run_provider_probe(args.base_url, os.environ[args.key_var], args.model, args.timeout)
    print(json.dumps({"probe": "passed", "model": args.model}, ensure_ascii=False))


def command_inject(args: argparse.Namespace) -> None:
    contract = inject_request(args.base_url, args.token, args.prompt, args.task_id, args.timeout, args.contract)
    print(json.dumps({"inject": 200, "run_id": contract["run_id"]}, ensure_ascii=False))


def command_monitor(args: argparse.Namespace) -> None:
    monitor = monitor_run(args.base_url, args.token, args.pid, args.run_id, args.started_at,
                          args.timeout, args.snapshot, args.poll, args.grace)
    atomic_json(args.output, monitor)
    print(json.dumps(monitor, ensure_ascii=False))


def command_collect(args: argparse.Namespace) -> None:
    result = collect_result(args.snapshot, args.monitor, args.project_root, args.run_id,
                            args.startup_probe_passed)
    atomic_json(args.output, result)
    print(json.dumps(result, ensure_ascii=False))


def command_finalize(args: argparse.Namespace) -> None:
    result = finalize_result(args.result, args.judge)
    print(json.dumps({
        "architecture_ok": result.get("architecture_ok", False),
        "task_resolved": result.get("task_resolved", False),
        "judge_verdict": result.get("judge_verdict", "unknown"),
    }, ensure_ascii=False))


def command_summarize(args: argparse.Namespace) -> None:
    rows = summarize_runs(args.runs, args.batch_start)
    atomic_json(args.output, rows)
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


def parser() -> argparse.ArgumentParser:
    root = argparse.ArgumentParser(description=__doc__)
    commands = root.add_subparsers(required=True)

    probe = commands.add_parser("probe")
    probe.add_argument("--base-url", required=True)
    probe.add_argument("--model", required=True)
    probe.add_argument("--key-var", required=True)
    probe.add_argument("--timeout", type=int, default=45)
    probe.set_defaults(func=command_probe)

    inject = commands.add_parser("inject")
    inject.add_argument("--base-url", required=True)
    inject.add_argument("--token", required=True)
    inject.add_argument("--prompt", required=True)
    inject.add_argument("--task-id", required=True)
    inject.add_argument("--timeout", type=int, required=True)
    inject.add_argument("--contract", required=True)
    inject.set_defaults(func=command_inject)

    monitor = commands.add_parser("monitor")
    monitor.add_argument("--base-url", required=True)
    monitor.add_argument("--token", required=True)
    monitor.add_argument("--pid", type=int, required=True)
    monitor.add_argument("--run-id", required=True)
    monitor.add_argument("--started-at", type=float, required=True)
    monitor.add_argument("--timeout", type=int, required=True)
    monitor.add_argument("--snapshot", required=True)
    monitor.add_argument("--output", required=True)
    monitor.add_argument("--poll", type=int, default=3)
    monitor.add_argument("--grace", type=int, default=30)
    monitor.set_defaults(func=command_monitor)

    collect = commands.add_parser("collect")
    collect.add_argument("--snapshot", required=True)
    collect.add_argument("--monitor", required=True)
    collect.add_argument("--project-root", required=True)
    collect.add_argument("--run-id", required=True)
    collect.add_argument("--startup-probe-passed", action="store_true")
    collect.add_argument("--output", required=True)
    collect.set_defaults(func=command_collect)

    finalize = commands.add_parser("finalize")
    finalize.add_argument("--result", required=True)
    finalize.add_argument("--judge", required=True)
    finalize.set_defaults(func=command_finalize)

    summarize = commands.add_parser("summarize")
    summarize.add_argument("--runs", required=True)
    summarize.add_argument("--batch-start", type=float, required=True)
    summarize.add_argument("--output", required=True)
    summarize.set_defaults(func=command_summarize)
    return root


def main() -> int:
    args = parser().parse_args()
    try:
        if args.func is command_probe and not os.environ.get(args.key_var):
            raise RuntimeError(f"密钥环境变量 {args.key_var} 未设置")
        args.func(args)
        return 0
    except Exception as error:  # harness 顶层只打印有界类型/消息，绝不打印请求正文或凭证。
        print(f"SWE_HARNESS_ERROR: {error}", file=os.sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
