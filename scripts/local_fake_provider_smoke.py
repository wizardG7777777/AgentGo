#!/usr/bin/env python3
"""用本地 Responses fake provider 验证真实 AgentGo 二进制的五层主链。

该脚本不访问外网、不读取任何 API key，也不运行 SWE/Flask。它驱动一个
mutating simple Graph，刻意让 Worker 重复读取直到 6-turn 周期 Observation checkpoint，
第一次 Observation 返回合法 Responses 正文但没有 required tool call，验证全新投影
只重试一次；
先 blocked 经 typed recovery 创建 work@2，再完成写入、run_check；Acceptance
同样跨过 6 个知识轮次并通过独立 Observation control 后才提交 verdict，最后
完成 final-report。
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import re
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path


TERMINAL_GRAPH = {"completed", "failed", "blocked", "cancelled"}
TERMINAL_TASK = {"completed", "failed", "blocked", "cancelled"}


def _all_strings(value):
    if isinstance(value, str):
        yield value
    elif isinstance(value, dict):
        for child in value.values():
            yield from _all_strings(child)
    elif isinstance(value, list):
        for child in value:
            yield from _all_strings(child)


class FakeState:
    def __init__(self):
        self.lock = threading.Lock()
        self.call_no = 0
        self.worker_reads = 0
        self.last_worker_call_id = ""
        self.worker_wrote = False
        self.worker_blocked = False
        self.worker_checked = False
        self.verifier_reads = 0
        self.check_evidence_seen = False
        self.acceptance_candidate_seen = False
        self.acceptance_check_ref = ""
        self.verifier_content_ref_requested = False
        self.verifier_content_ref_read = False
        self.final_reads = 0
        self.tools_seen: list[str] = []
        self.actions: list[tuple[str, dict]] = []
        self.errors: list[str] = []
        self.observation_wire_verified = False
        self.observation_malformed_sent = False
        self.cancel_mode = False
        self.cancel_delay_started = threading.Event()
        self.slow_final_sent = False

    def next_call(self) -> str:
        with self.lock:
            self.call_no += 1
            return f"fake-call-{self.call_no}"


def fake_handler(state: FakeState):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, _format, *_args):
            return

        def do_POST(self):  # noqa: N802 - BaseHTTPRequestHandler API
            try:
                length = int(self.headers.get("Content-Length", "0"))
                body = json.loads(self.rfile.read(length) or b"{}")
                tools = body.get("tools") or []
                names = [tool.get("name", "") for tool in tools if isinstance(tool, dict)]
                if state.cancel_mode and not state.cancel_delay_started.is_set() and not any(
                        name.startswith("agentgo_capability_probe_") for name in names):
                    state.cancel_delay_started.set()
                    time.sleep(10)
                if names == ["record_observation_delta"]:
                    reasoning = body.get("reasoning") or {}
                    choice = body.get("tool_choice") or {}
                    if reasoning.get("effort") != "none" or choice.get("type") != "function" or choice.get("name") != "record_observation_delta":
                        raise RuntimeError(
                            f"Observation Control lane 必须 reasoning=none + exact typed action: "
                            f"reasoning={reasoning!r} tool_choice={body.get('tool_choice')!r}"
                        )
                    state.observation_wire_verified = True
                    with state.lock:
                        malformed = not state.observation_malformed_sent
                        state.observation_malformed_sent = True
                    if malformed:
                        state.call_no += 1
                        payload = {
                            "id": f"fake-response-{state.call_no}", "object": "response", "status": "completed",
                            "output": [{
                                "type": "message", "id": f"fake-item-{state.call_no}", "status": "completed",
                                "role": "assistant", "content": [{"type": "output_text", "text": "retry observation", "annotations": []}],
                            }],
                            "usage": {"input_tokens": 20, "output_tokens": 5, "total_tokens": 25,
                                      "input_tokens_details": {"cached_tokens": 0},
                                      "output_tokens_details": {"reasoning_tokens": 0}},
                        }
                        encoded = json.dumps(payload, ensure_ascii=False).encode()
                        self.send_response(200)
                        self.send_header("Content-Type", "application/json")
                        self.send_header("Content-Length", str(len(encoded)))
                        self.end_headers()
                        self.wfile.write(encoded)
                        return
                if names == ["report_done"] and not state.slow_final_sent:
                    state.slow_final_sent = True
                    time.sleep(3)
                args, name = choose_action(state, body, tools, names)
                call_id = state.next_call()
                if name == "read_file":
                    state.last_worker_call_id = call_id
                state.tools_seen.append(name)
                state.actions.append((name, args))
                output = []
                output.append({
                    "type": "function_call",
                    "id": f"fake-item-{state.call_no}",
                    "call_id": call_id,
                    "status": "completed",
                    "name": name,
                    "arguments": json.dumps(args, ensure_ascii=False, separators=(",", ":")),
                })
                payload = {
                    "id": f"fake-response-{state.call_no}",
                    "object": "response",
                    "status": "completed",
                    "output": output,
                    "usage": {
                        "input_tokens": 20,
                        "output_tokens": 5,
                        "total_tokens": 25,
                        "input_tokens_details": {"cached_tokens": 0},
                        "output_tokens_details": {"reasoning_tokens": 0},
                    },
                }
                encoded = json.dumps(payload, ensure_ascii=False).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(encoded)))
                self.end_headers()
                self.wfile.write(encoded)
            except (BrokenPipeError, ConnectionResetError, ConnectionAbortedError):  # intentional timeout/cancel scenarios
                if not state.cancel_mode:
                    state.errors.append("provider connection closed unexpectedly")
                return
            except Exception as exc:  # pragma: no cover - smoke failure path
                state.errors.append(repr(exc))
                encoded = json.dumps({"error": {"message": repr(exc)}}).encode()
                self.send_response(500)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(encoded)))
                self.end_headers()
                self.wfile.write(encoded)

    return Handler


def choose_action(state: FakeState, body: dict, tools: list[dict], names: list[str]):
    if not names:
        raise RuntimeError("fake provider 收到无工具 Invocation")
    text = "\n".join(_all_strings(body.get("input") or body.get("messages") or []))
    probe = next((name for name in names if name.startswith("agentgo_capability_probe_")), "")
    if probe:
        tool = next(tool for tool in tools if tool.get("name") == probe)
        nonce = (((tool.get("parameters") or {}).get("properties") or {}).get("nonce") or {}).get("const")
        return {"nonce": nonce}, probe
    for name, args in (
        ("create_graph_draft", {}),
        ("configure_simple_graph_draft", {"execution_class": "mutating"}),
        ("validate_current_graph_draft", {}),
        ("commit_current_graph_draft", {}),
        ("start_current_graph", {}),
    ):
        if name in names:
            return args, name
    if "submit_proposal_verdict" in names:
        return {"verdict": "pass"}, "submit_proposal_verdict"
    if names == ["record_observation_delta"]:
        if not state.last_worker_call_id:
            raise RuntimeError("Observation checkpoint 缺少当前 Attempt tool call")
        return {
            "phase": "investigate",
            "facts": [{
                "text": "当前调查已读取一组互不相同的 evidence fixture，并形成后续实施候选",
                "evidence_refs": [f"tool-call:{state.last_worker_call_id}"],
            }],
            "resolved_candidates": [],
            "next_candidates": ["新 Attempt 直接写入 smoke artifact 并提交"],
        }, "record_observation_delta"
    if "report_done" in names and "read_graph" in names:
        graph_match = re.search(r"graph-[0-9a-f-]{16,}", text)
        graph_id = graph_match.group(0) if graph_match else ""
        if state.final_reads == 0:
            state.final_reads += 1
            return {"graph_id": graph_id}, "read_graph"
        task_match = re.search(r'"task_id"\s*:\s*"([^"]+)"', text)
        if state.final_reads == 1 and task_match:
            state.final_reads += 1
            return {"task_id": task_match.group(1)}, "get_task_result"
        raise RuntimeError("final-report 在两个 evidence turn 后仍未进入 exact report_done")
    if names == ["report_done"]:
        return {"summary": "本地 fake-provider Graph 已完成；Observation、写入、验收与 finalization 均已收口。"}, "report_done"
    if "submit_recovery_decision" in names:
        return {
            "decision": "retry", "changed_dimensions": ["strategy"],
            "strategy": "从已冻结 Observation 继续，完整覆盖 README 后提交修改决策",
            "first_action": {"tool": "read_file", "path": "README.md"},
            "evidence_contract": {"files": ["README.md"]},
            "expected_milestone": "verification CheckRecord pass",
            "summary": "已形成可验证的新执行策略，创建 work@2",
        }, "submit_recovery_decision"
    if names == ["read_file"] and "recovery-evidence" in text:
        properties = ((tools[0].get("parameters") or {}).get("properties") or {})
        return {
            "path": (properties.get("path") or {}).get("const", "README.md"),
            "offset": (properties.get("offset") or {}).get("const", 1),
            "limit": (properties.get("limit") or {}).get("const", 80),
            "force_full": True,
        }, "read_file"
    if names == ["submit_change_decision"]:
        return {
            "decision": "edit", "edit_steps": [
                {"tool": "edit_file", "path": "README.md"},
                {"tool": "write_file", "path": "local-smoke.txt"},
            ],
            "summary": "EvidenceContract 已完整覆盖，执行 README 修改并新增 smoke artifact",
        }, "submit_change_decision"
    if names == ["edit_file"]:
        return {
            "path": "README.md", "old_str": "# Local smoke\n",
            "new_str": "# Local smoke\n\nRecovery handoff v4 evidence decision mutation.\n",
        }, "edit_file"
    if names == ["run_check"]:
        properties = ((tools[0].get("parameters") or {}).get("properties") or {})
        return {
            "check_id": (properties.get("check_id") or {}).get("const", "verification"),
            "kind": (properties.get("kind") or {}).get("const", "verification"),
            "command": (properties.get("command") or {}).get("const", "go version"),
        }, "run_check"
    if names == ["write_file"]:
        state.worker_wrote = True
        return {"path": "local-smoke.txt", "content": "agentgo local fake provider smoke\n"}, "write_file"
    if "submit_task_result" in names:
        if "逐项核验" in text or "验收原始请求" in text:
            if not all(marker in text for marker in (
                '"kind":"check"', '"check_id":"verification"',
                '"check_status":"pass"', '"workspace_revision_ref":"workspace:sha256:',
            )):
                raise RuntimeError("Acceptance 未收到 fulfillment 引用的 typed Check Evidence")
            state.check_evidence_seen = True
            if not state.verifier_content_ref_requested:
                check_match = re.search(r'"check_ref":"(check:[^"]+)"', text)
                ref_match = re.search(r'"output_ref":"(content:[^"]+)"', text)
                if not check_match or not ref_match:
                    raise RuntimeError("Acceptance 未收到 typed CheckRef 或可解引用 output_ref")
                state.acceptance_check_ref = check_match.group(1)
                state.verifier_content_ref_requested = True
                return {"ref_id": ref_match.group(1), "offset": 0, "limit": 4096}, "read_content_ref"
            if not state.verifier_content_ref_read:
                if '"encoding":"utf-8"' not in text or "go version" not in text:
                    raise RuntimeError("Acceptance 未读到上游 Check ContentRef 输出")
                state.verifier_content_ref_read = True
            if state.verifier_reads < 4:
                state.verifier_reads += 1
                path = "local-smoke.txt" if state.verifier_reads == 1 else f"evidence-{state.verifier_reads}.txt"
                if state.verifier_reads == 1:
                    # read_file 本身仍由 workspace/Delivery 集成测试验证；这里
                    # 只记录 fake provider 已按契约请求候选文件，避免把 Context
                    # 投影策略变化误当成 v4 handoff 失败。
                    state.acceptance_candidate_seen = True
                return {"path": path}, "read_file"
            return {
                "summary": "本地验收通过", "verdict": "pass",
                "cited_evidence": state.acceptance_check_ref,
            }, "submit_task_result"
        if state.worker_reads < 12:
            state.worker_reads += 1
            return {"path": f"evidence-{state.worker_reads}.txt"}, "read_file"
        if not state.worker_blocked:
            state.worker_blocked = True
            return {"summary": "调查阶段完成，交 recovery 创建新 Activation",
                    "status": "blocked", "blocked_reason": "需要以冻结 Observation 启动新的实施 Activation"}, "submit_task_result"
        if not state.worker_wrote:
            state.worker_wrote = True
            return {"path": "local-smoke.txt", "content": "agentgo local fake provider smoke\n"}, "write_file"
        if not state.worker_checked:
            state.worker_checked = True
            return {"check_id": "verification", "kind": "verification", "command": "go version"}, "run_check"
        return {
            "summary": "已写入本地 smoke artifact",
                "checks_performed": "本地 fake provider deterministic check",
            "evidence": "local-smoke.txt",
        }, "submit_task_result"
    raise RuntimeError(f"fake provider 不认识工具面: {names}")


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def http_json(url: str, token: str, payload: dict | None = None):
    data = None if payload is None else json.dumps(payload).encode()
    request = urllib.request.Request(url, data=data)
    request.add_header("Authorization", f"Bearer {token}")
    if data is not None:
        request.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(request, timeout=5) as response:
        raw = response.read()
        return json.loads(raw) if raw else {}


def main() -> int:
    for stream in (sys.stdout, sys.stderr):
        reconfigure = getattr(stream, "reconfigure", None)
        if callable(reconfigure):
            reconfigure(encoding="utf-8", errors="backslashreplace")
    parser = argparse.ArgumentParser()
    default_binary = ".\\agentgo.exe" if sys.platform == "win32" else "./agentgo"
    parser.add_argument("--binary", default=default_binary)
    args = parser.parse_args()
    repo = Path(__file__).resolve().parents[1]
    binary = Path(args.binary).resolve()
    if not binary.is_file():
        raise SystemExit(f"未找到 AgentGo binary: {binary}")
    state = FakeState()
    provider = ThreadingHTTPServer(("127.0.0.1", 0), fake_handler(state))
    provider_thread = threading.Thread(target=provider.serve_forever, daemon=True)
    provider_thread.start()
    token = "agentgo-local-smoke"
    ui_port = free_port()
    run_id = "run-local-fake-provider-smoke"
    now = dt.datetime.now(dt.timezone.utc)
    with tempfile.TemporaryDirectory(prefix="agentgo-local-smoke-") as temp:
        root = Path(temp)
        (root / "README.md").write_text("# Local smoke\n", encoding="utf-8")
        for index in range(1, 13):
            (root / f"evidence-{index}.txt").write_text(f"unique evidence {index}\n", encoding="utf-8")
        config = {
            "llm": {
                "base_url": f"http://127.0.0.1:{provider.server_port}",
                "api_key": "local-test-only",
                "default_model": "fake-responses-model",
                "protocol": "responses",
                "timeout_sec": 2,
                "reasoning_effort": "high",
                "stream": False,
            },
            "tool_profiles": {
                "worker": ["read_file", "list_dir", "grep_search", "glob_search", "read_content_ref",
                           "write_file", "edit_file", "run_shell", "run_check", "submit_task_result"],
                "verifier": ["read_file", "list_dir", "grep_search", "glob_search", "read_content_ref",
                             "submit_task_result"],
            },
            "agents": [
                {"kind": "worker", "replicas": 1, "event_type": "", "profile": "worker",
                 "model": "fake-responses-model", "system_prompt_file": (repo / "prompts/swe/worker.md").as_posix(),
                 "task_max_retries": 3},
                {"kind": "verifier", "replicas": 1, "event_type": "acceptance.verify", "profile": "verifier",
                 "model": "fake-responses-model", "system_prompt_file": (repo / "prompts/swe/verifier.md").as_posix(),
                 "task_max_retries": 2},
            ],
            "scheduler": {"model": "fake-responses-model"},
            "project_root": root.as_posix(),
            "startup_probe": "tool",
            "startup_probe_timeout_sec": 5,
            "startup_probe_failure_action": "exit",
            "ui": {"frontends": ["web"], "web": {
                "listen": f"127.0.0.1:{ui_port}", "token": token, "auto_open": False,
            }},
        }
        config_path = root / "setting.json"
        config_path.write_text(json.dumps(config, ensure_ascii=False), encoding="utf-8")
        log_path = root / "agentgo.log"
        with log_path.open("w+", encoding="utf-8") as log:
            process = subprocess.Popen([str(binary), "-config", str(config_path)], cwd=repo,
                                       stdout=log, stderr=subprocess.STDOUT, text=True)
            try:
                base = f"http://127.0.0.1:{ui_port}"
                deadline = time.monotonic() + 20
                while time.monotonic() < deadline:
                    if process.poll() is not None:
                        break
                    try:
                        urllib.request.urlopen(base + "/healthz", timeout=1).read()
                        break
                    except (OSError, urllib.error.URLError):
                        time.sleep(0.1)
                else:
                    raise RuntimeError("真实二进制未在 20 秒内通过 healthz")
                if process.poll() is not None:
                    raise RuntimeError(f"真实二进制提前退出: code={process.returncode}")
                contract = {
                    "schema": "agentgo.run-contract/v2",
                    "run_id": run_id,
                    "created_at": now.isoformat().replace("+00:00", "Z"),
                    "deadline_at": (now + dt.timedelta(seconds=90)).isoformat().replace("+00:00", "Z"),
                    "recovery_reserve": 10_000_000_000,
                    "finalization_reserve": 5_000_000_000,
                    "verification_reserve": 15_000_000_000,
                    "budget_profile": "interactive/v3",
                }
                http_json(base + "/api/input", token, {
                    "text": "创建 local-smoke.txt，内容为一次本地 fake provider 验证，并完成独立验收。",
                    "run_contract": contract,
                })
                snapshot = {}
                deadline = time.monotonic() + 70
                while time.monotonic() < deadline:
                    snapshot = http_json(base + "/api/snapshot", token)
                    graphs = [g for g in snapshot.get("graphs", []) if g.get("run_id") == run_id]
                    tasks = [t for t in snapshot.get("tasks", []) if t.get("run_id") == run_id]
                    reports = [t for t in tasks if t.get("final_report_graph_id")]
                    if graphs and graphs[0].get("status") in TERMINAL_GRAPH and reports and reports[0].get("status") in TERMINAL_TASK:
                        break
                    if process.poll() is not None:
                        raise RuntimeError(f"真实二进制在 Graph 收口前退出: code={process.returncode}")
                    time.sleep(0.2)
                else:
                    raise RuntimeError("本地 fake-provider Graph 未在 70 秒内收口")
                graphs = [g for g in snapshot.get("graphs", []) if g.get("run_id") == run_id]
                reports = [t for t in snapshot.get("tasks", []) if t.get("run_id") == run_id and t.get("final_report_graph_id")]
                observations = list((root / ".agentgo" / "state" / "taskmem" / "observations").glob("*/*.json"))
                checks = list((root / ".agentgo" / "state" / "checks").glob("*/*/*.json"))
                delivery_files = list((root / ".agentgo" / "state" / "deliveries").glob("*.json"))
                budget_path = root / ".agentgo" / "state" / "run-budgets" / "run-budgets.jsonl"
                budget_records = [json.loads(line) for line in budget_path.read_text(encoding="utf-8").splitlines() if line.strip()]
                reservations = {record["reservation"]["reservation_id"] for record in budget_records if record.get("kind") == "reserve"}
                settlements = {record["settlement"]["reservation_id"] for record in budget_records if record.get("kind") == "settle"}
                trace_events = []
                for trace_path in (root / ".agentgo" / "sessions").glob("*/logs/*.jsonl"):
                    for line in trace_path.read_text(encoding="utf-8", errors="replace").splitlines():
                        try:
                            event = json.loads(line)
                        except json.JSONDecodeError:
                            continue
                        if event.get("run_id") == run_id:
                            trace_events.append(event)
                trace_model_calls = sum(event.get("kind") == "llm_call_end" for event in trace_events)
                ledger_model_calls = sum(
                    int(((record.get("settlement") or {}).get("usage") or {}).get("model_calls") or 0)
                    for record in budget_records if record.get("kind") == "settle"
                )
                checkpoint_preflight_failures = [
                    event for event in trace_events
                    if event.get("kind") == "observation_checkpoint_failed"
                    and event.get("reason") == "control_invocation_preflight_failed"
                ]
                checkpoint_failures = [event for event in trace_events if event.get("kind") == "observation_checkpoint_failed"]
                assert len(graphs) == 1, f"同 Run 顶层 Graph 数量={len(graphs)}"
                assert graphs[0].get("status") == "completed" and graphs[0].get("outcome") == "success", graphs[0]
                assert reports and reports[0].get("status") == "completed", reports
                assert state.final_reads == 2, f"final-report 补读次数={state.final_reads}"
                assert state.slow_final_sent, "final-report 未经过慢 provider fallback"
                assert "record_observation_delta" in state.tools_seen, state.tools_seen
                assert observations, "未找到 durable ObservationDelta"
                assert all(json.loads(path.read_text(encoding="utf-8")).get("schema") ==
                           "agentgo.observation-delta/v3" for path in observations), observations
                assert state.observation_wire_verified, "Observation 未使用独立 exact Control lane"
                assert state.observation_malformed_sent and len(checkpoint_failures) == 1, checkpoint_failures
                assert state.verifier_reads == 4, f"Acceptance 知识轮次数={state.verifier_reads}"
                assert state.verifier_content_ref_requested and state.verifier_content_ref_read, \
                    "Acceptance 未完成冻结上游 ContentRef 委托读取"
                assert state.acceptance_check_ref, "Acceptance 未引用 typed CheckRef 别名"
                assert len(observations) >= 2, f"Worker/Acceptance Observation 未全部落盘: {observations}"
                assert reservations and reservations == settlements, (reservations, settlements)
                assert ledger_model_calls == trace_model_calls, (ledger_model_calls, trace_model_calls)
                assert not checkpoint_preflight_failures, checkpoint_preflight_failures
                assert checks, "未找到 durable CheckRecord"
                assert len(delivery_files) == 1, f"Delivery Transaction 数量异常: {delivery_files}"
                delivery_tx = json.loads(delivery_files[0].read_text(encoding="utf-8"))
                assert delivery_tx.get("status") == "committed", delivery_tx
                assert (delivery_tx.get("candidate") or {}).get("ref"), delivery_tx
                assert delivery_tx.get("commit_effect_ref") and delivery_tx.get("committed_revision_ref"), delivery_tx
                workspace_root = root / ".agentgo" / "workspaces"
                assert not [path for path in workspace_root.glob("delivery-*") if path.is_dir()], \
                    "success promotion 后仍残留 Delivery workspace"
                assert "submit_recovery_decision" in state.tools_seen, state.tools_seen
                assert "submit_change_decision" in state.tools_seen, state.tools_seen
                assert "run_check" in state.tools_seen, state.tools_seen
                artifact_path = root / "local-smoke.txt"
                artifact_candidates = [str(path.relative_to(root)) for path in root.rglob("local-smoke.txt")]
                artifact_trace = [
                    event for event in trace_events
                    if event.get("kind") in {
                        "workspace_materialized", "workspace_merged", "workspace_cleaned",
                    } or (event.get("kind") == "tool_result" and event.get("tool") in {
                        "edit_file", "write_file", "run_check", "submit_task_result",
                    })
                ]
                assert artifact_path.is_file(), \
                    (f"promotion 后主根缺少 smoke artifact: candidates={artifact_candidates} "
                     f"delivery={delivery_tx} trace={artifact_trace}")
                assert artifact_path.read_text(encoding="utf-8").startswith("agentgo local"), "smoke artifact 错误"

                cancel_run_id = "run-local-fake-provider-cancel"
                cancel_now = dt.datetime.now(dt.timezone.utc)
                state.cancel_mode = True
                http_json(base + "/api/input", token, {
                    "text": "启动一个随后由用户取消的本地验证任务。",
                    "run_contract": {
                        "schema": "agentgo.run-contract/v2", "run_id": cancel_run_id,
                        "created_at": cancel_now.isoformat().replace("+00:00", "Z"),
                        "deadline_at": (cancel_now + dt.timedelta(seconds=60)).isoformat().replace("+00:00", "Z"),
                        "verification_reserve": 10_000_000_000,
                        "recovery_reserve": 5_000_000_000, "finalization_reserve": 5_000_000_000,
                        "budget_profile": "interactive/v3",
                    },
                })
                if not state.cancel_delay_started.wait(timeout=10):
                    raise RuntimeError("取消场景没有进入慢 provider invocation")
                cancel_task = None
                deadline = time.monotonic() + 10
                while time.monotonic() < deadline and cancel_task is None:
                    cancel_snapshot = http_json(base + "/api/snapshot", token)
                    cancel_task = next((task for task in cancel_snapshot.get("tasks", [])
                                        if task.get("run_id") == cancel_run_id), None)
                    if cancel_task is None:
                        time.sleep(0.1)
                if cancel_task is None:
                    raise RuntimeError("取消场景未找到当前 Run task")
                http_json(base + "/api/tasks/cancel", token, {"id_prefix": cancel_task["id"][:8]})
                cancel_reservations: set[str] = set()
                cancel_settlements: set[str] = set()
                deadline = time.monotonic() + 15
                while time.monotonic() < deadline:
                    cancel_records = [
                        json.loads(line) for line in budget_path.read_text(encoding="utf-8").splitlines()
                        if line.strip() and json.loads(line).get("run_id") == cancel_run_id
                    ]
                    cancel_reservations = {
                        record["reservation"]["reservation_id"] for record in cancel_records
                        if record.get("kind") == "reserve"
                    }
                    cancel_settlements = {
                        record["settlement"]["reservation_id"] for record in cancel_records
                        if record.get("kind") == "settle"
                    }
                    if cancel_reservations and cancel_reservations == cancel_settlements:
                        break
                    time.sleep(0.1)
                assert cancel_reservations == cancel_settlements, (cancel_reservations, cancel_settlements)
                assert not state.errors, state.errors
                print(json.dumps({
                    "schema": "agentgo.local-fake-provider-smoke/v1",
                    "graph_id": graphs[0].get("graph_id"),
                    "graph_status": graphs[0].get("status"),
                    "graph_outcome": graphs[0].get("outcome"),
                    "top_level_graphs": len(graphs),
                    "worker_reads_before_checkpoint": state.worker_reads,
                    "verifier_reads_before_checkpoint": state.verifier_reads,
                    "observation_records": len(observations),
                    "check_records": len(checks),
                    "check_evidence_seen": state.check_evidence_seen,
                    "acceptance_candidate_seen": state.acceptance_candidate_seen,
                    "acceptance_content_ref_read": state.verifier_content_ref_read,
                    "acceptance_check_ref_cited": bool(state.acceptance_check_ref),
                    "delivery_status": delivery_tx.get("status"),
                    "run_budget_records": len(budget_records),
                    "ledger_model_calls": ledger_model_calls,
                    "trace_model_calls": trace_model_calls,
                    "final_report_reads": state.final_reads,
                    "final_report_status": reports[0].get("status"),
                    "slow_final_fallback": state.slow_final_sent,
                    "observation_control_failures": len(checkpoint_failures),
                    "active_reservations": len(reservations - settlements),
                    "cancel_active_reservations": len(cancel_reservations - cancel_settlements),
                }, ensure_ascii=False))
            except Exception:
                print(json.dumps({"fake_actions": state.actions, "fake_errors": state.errors}, ensure_ascii=False))
                diagnostic = []
                for trace_path in (root / ".agentgo" / "sessions").glob("*/logs/*.jsonl"):
                    for line in trace_path.read_text(encoding="utf-8", errors="replace").splitlines():
                        try:
                            event = json.loads(line)
                        except json.JSONDecodeError:
                            continue
                        if event.get("kind") in {"task_failed", "task_blocked", "error", "tool_result", "llm_call_end"}:
                            diagnostic.append(event)
                diagnostic.sort(key=lambda event: event.get("ts", ""))
                print(json.dumps({"diagnostic_events": diagnostic[-80:]}, ensure_ascii=False))
                log.flush()
                log.seek(0)
                print(log.read())
                raise
            finally:
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=10)
                    except subprocess.TimeoutExpired:  # cleanup only, never a task verdict
                        process.kill()
                        process.wait(timeout=5)
    provider.shutdown()
    provider.server_close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
