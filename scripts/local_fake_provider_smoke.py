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


def edge(to, event=None, verdict=None, target=None):
    result = {"to": to}
    if event:
        result["when"] = {"event": event}
    if verdict:
        result["when"] = {"path": "$.verdict", "operator": "eq", "value": verdict}
    if target:
        result["target_input"] = target
    return result


def worker_node(revision):
    return {"kind": "agent", "task": {"title": "写入文件", "description": f"LOCAL_WORK_V{revision}：创建 local-smoke.txt，完成后提交产物路径。"},
            "capability": {"tools": ["read_file", "run_shell", "apply_change", "send_message", "inspect_node", "read_evidence", "submit_task_result"], "isolation": "workspace"},
            "contract_bindings": {"deliverables": ["file"], "effects": ["file_write"]},
            "output_contract": {"summary_required": True, "fields": [{"path": "$.artifact", "type": "string", "required": True}]},
            "next": [edge("verify", "completed", target="candidate"), edge("work-failed", "failed"), edge("work-blocked", "blocked")]}


def graph_request(worker_route=""):
    nodes = {
        "adjust": {"kind": "controller", "task": {"title": "动态更新", "description": "LOCAL_ADJUST：读取图；拒绝非法修改，再更新尚未执行的 work 描述，保留当前在途节点，最后提交。"},
                   "next": [edge("work", "completed"), edge("adjust-failed", "failed"), edge("adjust-blocked", "blocked")]},
        "work": worker_node(1),
        "verify": {"kind": "acceptance", "task": {"title": "验收文件", "description": "LOCAL_VERIFY：读取 local-smoke.txt，核对内容为 agentgo local 测试交付，引用上游证据提交 verdict。", "required_inputs": ["candidate"]},
                   "capability": {"tools": ["read_file", "inspect_node", "read_evidence", "submit_task_result"]},
                   "next": [edge("done", verdict="pass"), edge("fixable", verdict="fixable"), edge("rejected", verdict="failed"),
                            edge("verify-failed", "failed"), edge("verify-blocked", "blocked")]},
    }
    if worker_route:
        nodes["work"]["metadata"] = {"route": worker_route}
    for name in ("adjust-failed", "adjust-blocked", "work-failed", "work-blocked", "fixable", "rejected", "verify-failed", "verify-blocked", "done"):
        nodes[name] = {"kind": "end", "end_outcome": "success" if name == "done" else ("blocked" if "blocked" in name or name == "fixable" else "failed"), "next": []}
    return {"operation": "create", "request_id": "local-create", "definition": {"root": "adjust", "nodes": nodes},
            "contract": {"execution_class": "mutating", "deliverables": [{"id": "file", "kind": "artifact"}],
                         "required_effects": ["file_write"], "requires_acceptance": True}}


class Scenario:
    def __init__(self, protocol, team=False):
        self.protocol = protocol
        self.team = team
        self.team_step = 0
        self.team_route = ""
        self.calls = 0
        self.graph_id = ""
        self.created = False
        self.started = False
        self.control_step = 0
        self.worker_step = 0
        self.verifier_step = 0
        self.final_step = 0
        self.observer_id = ""
        self.names_seen = set()
        self.actions = []
        self.errors = []
        self.invalid_rejected = False
        self.updated = False

    def choose(self, body):
        tools = [t.get("function", t) for t in body.get("tools", [])]
        names = [t["name"] for t in tools]
        self.names_seen.update(names)
        if RETIRED_TOOLS.intersection(names):
            raise RuntimeError("模型工具面仍包含退役工具")
        text = "\n".join(strings(body.get("input") or body.get("messages")))
        probe = next((t for t in tools if t["name"].startswith("agentgo_capability_probe_")), None)
        if probe:
            nonce = probe["parameters"]["properties"]["nonce"].get("const")
            return probe["name"], {"nonce": nonce}
        if "submit_proposal_verdict" in names:
            return "submit_proposal_verdict", {"verdict": "pass"}
        if "apply_graph_change" in names:
            if self.team and not self.created:
                self.team_step += 1
                if self.team_step == 1:
                    if "list_agent_templates" not in names:
                        raise RuntimeError("启用 Team 后模型看不到模板工具")
                    return "list_agent_templates", {}
                if self.team_step == 2:
                    return "provision_agent_team", {"template_ref": "builtin/generalist@2", "purpose": "执行本地文件任务", "graph_request_id": "local-create"}
                match = re.search(r'"event_type"\s*:\s*"(team:[a-zA-Z0-9-]+)"', text)
                if not match:
                    raise RuntimeError("Team 未返回实际 ready route：" + text[-1200:])
                self.team_route = match.group(1)
            if not self.created:
                self.created = True
                return "apply_graph_change", graph_request(self.team_route)
            if not self.graph_id:
                match = re.search(r'"graph_id"\s*:\s*"(graph-[a-f0-9]+)"', text)
                if not match:
                    raise RuntimeError("建图未返回正式回执：" + text[-1200:])
                self.graph_id = match.group(1)
            if not self.started:
                self.started = True
                return "control_graph", {"action": "start", "graph_id": self.graph_id, "expected_revision": 1}
            self.control_step += 1
            if self.control_step == 1:
                return "read_graph_definition", {"graph_id": self.graph_id}
            if self.control_step == 2:
                return "apply_graph_change", {"operation": "update", "request_id": "invalid-update", "graph_id": self.graph_id,
                    "expected_revision": 1, "changes": {"remove_nodes": ["adjust"]}, "reason": "验证非法更新拒绝", "in_flight": "preserve"}
            if self.control_step == 3:
                if "错误" not in text:
                    raise RuntimeError("非法图更新没有返回拒绝事实")
                self.invalid_rejected = True
                return "apply_graph_change", {"operation": "update", "request_id": "valid-update", "graph_id": self.graph_id,
                    "expected_revision": 1, "changes": {"upsert_nodes": [{"id": "work", **worker_node(2), **({"metadata": {"route": self.team_route}} if self.team_route else {})}]},
                    "reason": "更新未来节点，保留当前执行", "in_flight": "preserve"}
            if self.control_step == 4:
                if not re.search(r'"revision"\s*:\s*2', text) or not re.search(r'"changed_nodes"\s*:\s*\[\s*"work"', text):
                    raise RuntimeError("合法更新没有提交新 revision：" + text[-1200:])
                self.updated = True
                return "submit_task_result", {"summary": "已更新未来节点，当前执行保持冻结"}
            raise RuntimeError("控制节点未正常收口：" + text[-800:])
        if "apply_change" in names:
            if "LOCAL_WORK_V2" not in text:
                raise RuntimeError("Worker 未使用动态更新后的定义")
            self.worker_step += 1
            if self.worker_step <= 8:
                return "read_file", {"path": "README.md"}
            if self.worker_step == 9:
                return "send_message", {"to": self.observer_id, "content": "仅传递测试信息，请保持当前任务状态。", "msg_type": "info"}
            if self.worker_step == 10:
                return "apply_change", {"operation": "create", "path": "local-smoke.txt", "content": ARTIFACT_TEXT}
            if self.worker_step == 11:
                return "run_shell", {"command": "echo local-shell-output"}
            if self.worker_step == 12:
                return "inspect_node", {}
            if self.worker_step == 13:
                return "submit_task_result", {"summary": "已写入并检查实际文件", "result": {"artifact": "local-smoke.txt"}}
            raise RuntimeError("Worker 未正常收口：" + text[-1200:])
        if "read_file" in names:
            self.verifier_step += 1
            if self.verifier_step == 1:
                return "read_file", {"path": "local-smoke.txt"}
            if self.verifier_step == 2:
                refs = re.findall(r"ev:[A-Za-z0-9:._-]+", text)
                if not refs:
                    raise RuntimeError("验收没有可引用的上游 Evidence：" + text[-1200:])
                return "submit_task_result", {"summary": "文件内容与目标一致", "verdict": "pass", "cited_evidence": refs[0]}
            raise RuntimeError("验收未正常收口：" + text[-1200:])
        self.final_step += 1
        if self.final_step == 1:
            return "inspect_board", {}
        if self.final_step == 2:
            return "submit_task_result", {"summary": "本地文件修改已验收并交付"}
        raise RuntimeError("最终汇报未正常收口：" + text[-1200:])


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
            config = {"llm": {"request_contract": "agentgo.model-request/v1", "base_url": f"http://127.0.0.1:{provider.server_port}",
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
                    api(base, token, "/api/input", {"text": "创建 local-smoke.txt，按图独立验收后交付。", "run_contract": {
                        "schema": "agentgo.run-contract/v3", "run_id": run_id, "budget_profile": "local-smoke/v1",
                        "created_at": dt.datetime.now(dt.timezone.utc).isoformat().replace("+00:00", "Z")}})
                    deadline = time.monotonic() + 60
                    while time.monotonic() < deadline:
                        if scenario.errors:
                            raise RuntimeError("本地协议场景失败：" + scenario.errors[0])
                        if process.poll() is not None:
                            raise RuntimeError("执行中二进制退出")
                        snapshot = api(base, token, "/api/snapshot")
                        graphs = [g for g in snapshot.get("graphs", []) if g.get("run_id") == run_id]
                        tasks = [t for t in snapshot.get("tasks", []) if t.get("run_id") == run_id]
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
                    assert scenario.invalid_rejected and scenario.updated and graphs[0]["revision"] == 2
                    assert (root / "local-smoke.txt").read_text(encoding="utf-8") == ARTIFACT_TEXT
                    assert scenario.worker_step == 13 and scenario.verifier_step == 2
                    assert not any(scenario.observer_id in (t.get("agents") or []) for t in tasks), "信息投递创建了接收者任务"
                    # 事务回执稍晚于 UI 终态写入，等待既有账本收口，不重跑工具。
                    audit = None
                    for _ in range(40):
                        audit = collect_runtime(snapshot_path, monitor_path, root, run_id, True)
                        if audit["architecture_ok"] is True:
                            break
                        time.sleep(0.05)
                    assert audit and audit["architecture_ok"] is True, audit
                    outputs = [json.loads(line) for path in (root / ".agentgo" / "sessions").glob("*/turns.jsonl") for line in path.read_text(encoding="utf-8").splitlines() if line]
                    assert any("逐段回显" in output.get("text", "") for output in outputs)
                    print(json.dumps({"protocol": args.protocol, "dynamic_team": args.team, "graph_outcome": "success", "revision": 2,
                        "artifact": "local-smoke.txt", "continuous_reads": 8, "information_did_not_activate_receiver": True,
                        "architecture_ok": audit["architecture_ok"], "model_calls": audit["metrics"]["model_calls"]}, ensure_ascii=False))
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
