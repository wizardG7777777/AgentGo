#!/usr/bin/env python3
"""本地双协议 SSE 二进制冒烟；不使用真实 provider 或 Flask/SWE。"""
from __future__ import annotations

import argparse
import datetime as dt
import json
from pathlib import Path
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

sys.path.insert(0, str(Path(__file__).resolve().parent / "swe_test_runner"))
from runtime_audit import collect_runtime, RETIRED_TOOLS


TERMINAL = {"completed", "failed", "blocked", "cancelled"}
ARTIFACT_TEXT = "agentgo local 测试交付\n"


def strings(value):
    if isinstance(value, str):
        yield value
    elif isinstance(value, dict):
        for child in value.values():
            yield from strings(child)
    elif isinstance(value, list):
        for child in value:
            yield from strings(child)


def task_node(node_id, objective, tools, route="default", inputs=None):
    return {"node_id": node_id, "kind": "agentTask", "title": node_id, "objective": objective,
            "execution": {"route_ref": route, "tools": tools},
            "result_schema": {"type": "object", "properties": {"summary": {"type": "string"}}, "required": ["summary"]},
            **({"inputs": inputs} if inputs else {})}


def state_from_text(text):
    decoder = json.JSONDecoder()
    for match in re.finditer(r"\{", text):
        try:
            value, _ = decoder.raw_decode(text[match.start():])
            if isinstance(value, dict) and value.get("schema") == "agentgo.graph/v6" and "executions" in value:
                return value
        except ValueError:
            continue
    return None


class Scenario:
    def __init__(self, protocol, team=False):
        self.protocol, self.team = protocol, team
        self.calls, self.team_step, self.worker_step, self.verifier_step, self.research_step = 0, 0, 0, 0, 0
        self.team_route, self.graph_id, self.observer_id = "", "", ""
        self.created = self.started = self.updated = self.completed = False
        self.names_seen, self.actions, self.errors = set(), [], []

    def choose(self, body):
        tools = [t.get("function", t) for t in body.get("tools", [])]
        names = [t["name"] for t in tools]
        self.names_seen.update(names)
        if RETIRED_TOOLS.intersection(names) or "submit_proposal_verdict" in names:
            raise RuntimeError("仍包含退役工具或强制 Proposal Acceptance")
        text = "\n".join(strings(body.get("input") or body.get("messages")))
        probe = next((t for t in tools if t["name"].startswith("agentgo_capability_probe_")), None)
        if probe:
            return probe["name"], {"nonce": probe["parameters"]["properties"]["nonce"].get("const")}
        if "apply_graph_change" in names:
            if self.team and not self.created:
                self.team_step += 1
                if self.team_step == 1:
                    return "provision_agent_team", {"template_ref": "builtin/generalist@2", "purpose": "执行文件任务", "graph_request_id": "local-create"}
                match = re.search(r'"event_type"\s*:\s*"(team:[a-zA-Z0-9-]+)"', text)
                if not match:
                    raise RuntimeError("Team 没有返回 ready route")
                self.team_route = match.group(1)
            if not self.created:
                self.created = True
                return "apply_graph_change", {"operation": "create", "request_id": "local-create", "definition": {"objective": "调查后追加任务，最终交付文件", "nodes": [task_node("research", "LOCAL_RESEARCH：读取 README 并提交调查结果", ["read_file", "submit_task_result"], self.team_route or "default")]}}
            if not self.graph_id:
                match = re.search(r'"graph_id"\s*:\s*"(graph-[a-f0-9]+)"', text)
                if not match:
                    raise RuntimeError("建图未返回回执：" + text[-1000:])
                self.graph_id = match.group(1)
            if not self.started:
                self.started = True
                return "control_graph", {"action": "start", "request_id": "start", "graph_id": self.graph_id, "expected_revision": 1}
            state = state_from_text(text)
            if state is None:
                raise RuntimeError("规划没有完整的版本化图事实：" + text[-1200:])
            if not self.updated:
                if "research" not in state["results"]:
                    return "submit_task_result", {"summary": "等待当前研究结果"}
                self.updated = True
                work = task_node("work", "LOCAL_WORK：创建 local-smoke.txt", ["read_file", "run_shell", "apply_change", "send_message", "inspect_node", "read_evidence", "submit_task_result"], self.team_route or "default", {"research": {"kind": "node_result", "node_id": "research"}})
                check = task_node("check", "LOCAL_CHECK：读取 local-smoke.txt 并报告结果", ["read_file", "submit_task_result"], "acceptance.verify", {"candidate": {"kind": "node_result", "node_id": "work"}})
                return "apply_graph_change", {"operation": "update", "request_id": "add-work", "graph_id": self.graph_id, "expected_revision": 1, "changes": {"add": [work, check]}}
            if not self.completed and "check" in state["results"]:
                self.completed = True
                return "control_graph", {"action": "complete", "request_id": "complete", "graph_id": self.graph_id, "expected_revision": 2, "outcome": "success", "summary": "文件已检查，提交候选", "result_refs": [state["results"]["check"]["ref"]]}
            return "submit_task_result", {"summary": "本次规划已完成，等待新事实"}
        if "apply_change" in names:
            self.worker_step += 1
            if self.worker_step <= 8:
                return "read_file", {"path": "README.md"}
            if self.worker_step == 9:
                return "send_message", {"to": self.observer_id, "content": "仅传递信息", "msg_type": "info"}
            if self.worker_step == 10:
                return "apply_change", {"operation": "create", "path": "local-smoke.txt", "content": ARTIFACT_TEXT}
            if self.worker_step == 11:
                return "run_shell", {"command": "echo local-shell-output"}
            if self.worker_step == 12:
                return "inspect_node", {}
            if self.worker_step == 13:
                return "submit_task_result", {"summary": "文件已写入候选", "result": {"summary": "文件已写入候选", "artifact": "local-smoke.txt"}}
            raise RuntimeError("Worker 未收口")
        if "read_file" in names:
            if "LOCAL_CHECK" in text:
                self.verifier_step += 1
                if self.verifier_step == 1:
                    return "read_file", {"path": "local-smoke.txt"}
                if ARTIFACT_TEXT.strip() not in text:
                    raise RuntimeError("检查节点没有读取候选文件")
                return "submit_task_result", {"summary": "候选内容正确"}
            self.research_step += 1
            if self.research_step == 1:
                return "read_file", {"path": "README.md"}
            return "submit_task_result", {"summary": "研究完成，下一步创建文件"}
        return "submit_task_result", {"summary": "图已完成，文件已实际交付"}


def handler_for(scenario):
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def handle(self):
            try:
                super().handle()
            except (ConnectionResetError, ConnectionAbortedError, BrokenPipeError):
                pass

        def log_message(self, *_args):
            pass

        def do_POST(self):
            try:
                body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))))
                if body.get("stream") is not True:
                    raise RuntimeError("请求没有使用 SSE")
                if ("/responses" in self.path) != (scenario.protocol == "responses"):
                    raise RuntimeError("实际端点与显式协议不一致")
                name, args = scenario.choose(body)
                scenario.calls += 1
                call_id = f"native-call-{scenario.calls}"
                scenario.actions.append({"name": name, "args": args, "call_id": call_id})
                raw_args = json.dumps(args, ensure_ascii=False, separators=(",", ":"))
                cut = max(1, len(raw_args) // 2)
                if scenario.protocol == "responses":
                    item = {"type": "function_call", "id": f"item-{scenario.calls}", "call_id": call_id,
                            "status": "completed", "name": name, "arguments": raw_args}
                    message = {"type": "message", "id": f"message-{scenario.calls}", "role": "assistant", "status": "completed",
                               "content": [{"type": "output_text", "text": "逐段回显", "annotations": []}]}
                    events = [{"type": "response.output_text.delta", "output_index": 0, "item_id": message["id"], "delta": part} for part in ("逐段", "回显")]
                    events.append({"type": "response.output_item.done", "output_index": 0, "item": message})
                    events.extend({"type": "response.function_call_arguments.delta", "output_index": 1, "item_id": item["id"], "delta": part}
                                  for part in (raw_args[:cut], raw_args[cut:]))
                    events.append({"type": "response.output_item.done", "output_index": 1, "item": item})
                    events.append({"type": "response.completed", "response": {"id": f"response-{scenario.calls}", "object": "response", "status": "completed",
                        "output": [message, item], "usage": {"input_tokens": 30, "output_tokens": 10, "total_tokens": 40}}})
                else:
                    def chunk(delta, finish=None):
                        return {"id": f"chat-{scenario.calls}", "object": "chat.completion.chunk", "choices": [{"index": 0, "delta": delta, "finish_reason": finish}]}
                    events = [chunk({"role": "assistant", "content": "逐段"}), chunk({"content": "回显"}),
                              chunk({"tool_calls": [{"index": 0, "id": call_id, "type": "function", "function": {"name": name, "arguments": raw_args[:cut]}}]}),
                              chunk({"tool_calls": [{"index": 0, "function": {"arguments": raw_args[cut:]}}]}), chunk({}, "tool_calls"),
                              {"id": f"chat-{scenario.calls}", "object": "chat.completion.chunk", "choices": [], "usage": {"prompt_tokens": 30, "completion_tokens": 10, "total_tokens": 40}}]
                if name == "submit_proposal_verdict":
                    # 独立机械校验只接收 typed verdict；正文回显在业务调用验证。
                    if scenario.protocol == "responses":
                        events = [e for e in events if e.get("type") != "response.output_text.delta"
                                  and not (e.get("type") == "response.output_item.done" and e["item"]["type"] == "message")]
                        for event in events:
                            if "output_index" in event:
                                event["output_index"] = 0
                            if event.get("type") == "response.completed":
                                event["response"]["output"] = [item]
                    else:
                        events = [e for e in events if not any((c.get("delta") or {}).get("content") for c in e.get("choices", []))]
                encoded = b"".join(("data: " + json.dumps(e, ensure_ascii=False) + "\n\n").encode("utf-8") for e in events)
                if scenario.protocol == "chat_completions":
                    encoded += b"data: [DONE]\n\n"
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Content-Length", str(len(encoded)))
                self.end_headers()
                self.wfile.write(encoded)
                self.wfile.flush()
            except (BrokenPipeError, ConnectionResetError, ConnectionAbortedError):
                return
            except Exception as error:
                scenario.errors.append(str(error))
                payload = json.dumps({"error": {"message": str(error)}}).encode()
                self.send_response(400)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)
    return Handler


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def api(base, token, path, payload=None):
    request = urllib.request.Request(base + path, data=None if payload is None else json.dumps(payload).encode(),
        headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"})
    with urllib.request.urlopen(request, timeout=5) as response:
        return json.load(response)


def main():
    for stream in (sys.stdout, sys.stderr):
        if hasattr(stream, "reconfigure"):
            stream.reconfigure(encoding="utf-8")
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--protocol", choices=("responses", "chat_completions"), default="responses")
    parser.add_argument("--team", action="store_true", help="用动态 Team 承担 Worker，验证无静态 Worker 的初建接缝")
    args = parser.parse_args()
    binary = args.binary.resolve(strict=True)
    repo = Path(__file__).resolve().parents[1]
    scenario = Scenario(args.protocol, args.team)
    provider = ThreadingHTTPServer(("127.0.0.1", 0), handler_for(scenario))
    thread = threading.Thread(target=provider.serve_forever, daemon=True)
    thread.start()
    try:
        with tempfile.TemporaryDirectory(prefix="agentgo-native-smoke-") as directory:
            root = Path(directory)
            (root / "README.md").write_text("本地调查样本\n", encoding="utf-8")
            token, port = "local-smoke-only", free_port()
            base = f"http://127.0.0.1:{port}"
            profiles = {
                "worker": ["read_file", "run_shell", "apply_change", "send_message", "inspect_node", "read_evidence", "submit_task_result"],
                "verifier": ["read_file", "inspect_node", "read_evidence", "submit_task_result"],
                "observer": ["read_file", "submit_task_result"],
            }
            config = {"graph": {"request_contract": "agentgo.graph/v6"}, "llm": {"request_contract": "agentgo.model-request/v1", "base_url": f"http://127.0.0.1:{provider.server_port}",
                "api_key": "local-test-only", "default_model": "local-fixture", "protocol": args.protocol, "timeout_sec": 10},
                "project_root": root.as_posix(), "startup_probe": "tool", "startup_probe_timeout_sec": 10,
                "startup_probe_failure_action": "exit", "tool_profiles": profiles,
                "scheduler": {"model": "local-fixture"}, "agent_templates": {"enabled": args.team}, "agents": [
                    {"kind": kind, "replicas": 1, "event_type": route, "profile": kind, "model": "local-fixture",
                     "system_prompt_file": (repo / "prompts" / prompt).as_posix(), "task_max_retries": 1}
                    for kind, route, prompt in (("worker", "", "worker.md"), ("verifier", "acceptance.verify", "verifier.md"), ("observer", "unused", "explorer.md")) if not (args.team and kind == "worker")],
                "shell": {"timeout_sec": 5},
                "ui": {"frontends": ["web"], "web": {"listen": f"127.0.0.1:{port}", "token": token, "auto_open": False}}}
            config_path, log_path = root / "setting.json", root / "agentgo.log"
            config_path.write_text(json.dumps(config), encoding="utf-8")
            with log_path.open("wb") as log:
                process = subprocess.Popen([str(binary), "-config", str(config_path)], cwd=repo, stdout=log, stderr=subprocess.STDOUT)
                try:
                    deadline = time.monotonic() + 30
                    while time.monotonic() < deadline:
                        if process.poll() is not None:
                            raise RuntimeError("二进制启动失败")
                        try:
                            snapshot = api(base, token, "/api/snapshot")
                            agents = snapshot.get("agents") or []
                            observer = next((a for a in agents if str(a.get("id", "")).startswith("observer")), None)
                            if observer:
                                scenario.observer_id = observer["id"]
                                break
                        except (OSError, urllib.error.URLError):
                            pass
                        time.sleep(0.1)
                    else:
                        raise RuntimeError("启动后未发现空闲接收代理")
                    run_id = "run-local-native-smoke"
                    api(base, token, "/api/input", {"text": "创建 local-smoke.txt，通过不完整 agentTask 图逐步追加工作并交付。", "run_contract": {
                        "schema": "agentgo.run-contract/v3", "run_id": run_id, "budget_profile": "local-smoke/v1",
                        "created_at": dt.datetime.now(dt.timezone.utc).isoformat().replace("+00:00", "Z")}})
                    deadline = time.monotonic() + 60
                    while time.monotonic() < deadline:
                        if scenario.errors:
                            raise RuntimeError("本地协议场景失败：" + scenario.errors[0])
                        if process.poll() is not None:
                            raise RuntimeError("执行中二进制退出")
                        snapshot = api(base, token, "/api/snapshot")
                        graphs = [g for g in (snapshot.get("graphs") or []) if g.get("run_id") == run_id]
                        tasks = [t for t in (snapshot.get("tasks") or []) if t.get("run_id") == run_id]
                        reports = [t for t in tasks if t.get("final_report_graph_id")]
                        if graphs and reports and all(g.get("status") in TERMINAL for g in graphs) and all(t.get("status") in TERMINAL for t in tasks):
                            break
                        time.sleep(0.05)
                    else:
                        raise RuntimeError("本地场景未完成终态收口")
                    snapshot_path, monitor_path = root / "snapshot.final.json", root / "monitor.json"
                    snapshot_path.write_text(json.dumps(snapshot), encoding="utf-8")
                    monitor_path.write_text(json.dumps({"run_identity_visible": True, "process_terminal": "graph_terminal"}), encoding="utf-8")
                    assert graphs[0].get("outcome") == "success", graphs
                    assert scenario.updated and scenario.completed and graphs[0]["revision"] == 2
                    assert (root / "local-smoke.txt").read_text(encoding="utf-8") == ARTIFACT_TEXT
                    assert scenario.worker_step == 13 and scenario.verifier_step == 2
                    assert not any(scenario.observer_id in (t.get("agents") or []) for t in tasks), "信息投递创建了接收者任务"
                    journals = list((root / ".agentgo/state/graphs-v6").glob("*.jsonl"))
                    assert len(journals) == 1
                    current = json.loads(journals[0].read_text(encoding="utf-8").splitlines()[-1])["snapshot"]
                    assert current["completion"]["status"] == "committed"
                    assert current["completion"].get("delivery_ref")
                    assert all(n["kind"] == "agentTask" for n in current["definition"]["nodes"])
                    assert current["results"]["work"]["candidate_ref"] == current["results"]["check"]["candidate_ref"]
                    audit = collect_runtime(snapshot_path, monitor_path, root, run_id, True)
                    assert audit["architecture_ok"] is True, audit
                    outputs = [json.loads(line) for path in (root / ".agentgo" / "sessions").glob("*/turns.jsonl") for line in path.read_text(encoding="utf-8").splitlines() if line]
                    assert any("逐段回显" in output.get("text", "") for output in outputs)
                    print(json.dumps({"protocol": args.protocol, "dynamic_team": args.team, "graph_outcome": "success", "revision": 2,
                        "artifact": "local-smoke.txt", "continuous_reads": 8, "information_did_not_activate_receiver": True,
                        "dataflow_checks": True, "model_calls": scenario.calls}, ensure_ascii=False))
                except Exception:
                    log.flush()
                    print(log_path.read_text(encoding="utf-8", errors="replace")[-4000:], file=sys.stderr)
                    for trace_path in (root / ".agentgo" / "sessions").glob("*/logs/*.jsonl"):
                        for line in trace_path.read_text(encoding="utf-8", errors="replace").splitlines():
                            try:
                                event = json.loads(line)
                            except ValueError:
                                continue
                            if event.get("kind") in {"llm_call_end", "tool_result", "task_failed", "task_blocked"}:
                                print(json.dumps({k: event.get(k) for k in ("kind", "tool", "error", "reason", "failure_kind", "call_id")}, ensure_ascii=False), file=sys.stderr)
                    raise
                finally:
                    if process.poll() is None:
                        process.terminate()
                        try:
                            process.wait(timeout=10)
                        except subprocess.TimeoutExpired:
                            process.kill()
                            process.wait(timeout=10)
    finally:
        provider.shutdown()
        provider.server_close()
        thread.join(timeout=5)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
