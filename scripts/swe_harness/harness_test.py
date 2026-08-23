#!/usr/bin/env python3

import datetime as dt
import json
import os
from pathlib import Path
import sys
import tempfile
import time
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import harness  # noqa: E402


def probe_response(name=harness.PROBE_NAME, arguments=None, finish="tool_calls"):
    if arguments is None:
        arguments = {"nonce": harness.PROBE_NONCE}
    return {
        "choices": [{
            "finish_reason": finish,
            "message": {"tool_calls": [{
                "function": {"name": name, "arguments": json.dumps(arguments)},
            }]},
        }],
    }


class HarnessContractTest(unittest.TestCase):
    def test_run_contract_leaves_external_and_phase_reserves(self):
        now = dt.datetime(2026, 8, 22, 1, 2, 3, tzinfo=dt.timezone.utc)
        contract = harness.build_run_contract("automatic-options", 1200, now)
        self.assertEqual(contract["schema"], harness.RUN_SCHEMA)
        self.assertEqual(contract["budget_profile"], "swe/v1")
        self.assertEqual(contract["recovery_reserve"], 90 * harness.NANOSECOND)
        self.assertEqual(contract["finalization_reserve"], 30 * harness.NANOSECOND)
        deadline = dt.datetime.fromisoformat(contract["deadline_at"].replace("Z", "+00:00"))
        self.assertEqual((deadline - now).total_seconds(), 1140)
        with self.assertRaises(ValueError):
            harness.build_run_contract("too-short", 239, now)

    def test_probe_requires_typed_auto_singleton_call(self):
        self.assertEqual(harness.validate_probe_response(probe_response()), (True, "ok"))
        for payload in (
            {"choices": [{"finish_reason": "stop", "message": {"content": "ok"}}]},
            probe_response(name="wrong"),
            probe_response(arguments={"unexpected": "x"}),
            probe_response(finish="stop"),
        ):
            self.assertFalse(harness.validate_probe_response(payload)[0])

    def test_provider_probe_rejects_text_and_transport_errors(self):
        text_only = {"choices": [{"finish_reason": "stop", "message": {"content": "pong"}}]}
        with self.assertRaises(RuntimeError):
            harness.run_provider_probe(
                "https://provider.invalid/v1", "secret", "model", attempts=1, sleep_sec=0,
                protocol="chat_completions", transport=lambda *_: (200, text_only),
            )
        with self.assertRaises(RuntimeError):
            harness.run_provider_probe(
                "https://provider.invalid/v1", "secret", "model", attempts=1, sleep_sec=0,
                protocol="chat_completions",
                transport=lambda *_: (_ for _ in ()).throw(RuntimeError("offline")),
            )

    def test_provider_probe_accepts_auto_singleton_transport_result(self):
        def transport(_endpoint, _key, body, _timeout):
            self.assertEqual(body["tool_choice"], "auto")
            self.assertEqual(body["reasoning_effort"], "low")
            name = body["tools"][0]["function"]["name"]
            nonce = body["tools"][0]["function"]["parameters"]["properties"]["nonce"]["const"]
            return 200, probe_response(name=name, arguments={"nonce": nonce})

        harness.run_provider_probe(
            "https://provider.invalid/v1", "secret", "model", attempts=1, sleep_sec=0,
            protocol="chat_completions", transport=transport,
        )

    def test_responses_provider_probe_requires_typed_item_and_nonce(self):
        def transport(endpoint, _key, body, _timeout):
            self.assertTrue(endpoint.endswith("/responses"))
            self.assertEqual(body["tool_choice"], "auto")
            self.assertEqual(body["reasoning"], {"effort": "low"})
            name = body["tools"][0]["name"]
            nonce = body["tools"][0]["parameters"]["properties"]["nonce"]["const"]
            return 200, {
                "id": "resp-1", "status": "completed",
                "output": [{
                    "type": "function_call", "call_id": "call-1", "name": name,
                    "arguments": json.dumps({"nonce": nonce}),
                }],
            }

        harness.run_provider_probe(
            "https://provider.invalid/v1", "secret", "model", protocol="responses",
            attempts=1, sleep_sec=0, transport=transport,
        )

        text_only = {"id": "resp-2", "status": "completed", "output": [{
            "type": "message", "content": [{"type": "output_text", "text": "call tool"}],
        }]}
        with self.assertRaises(RuntimeError):
            harness.run_provider_probe(
                "https://provider.invalid/v1", "secret", "model", protocol="responses",
                attempts=1, sleep_sec=0, transport=lambda *_: (200, text_only),
            )

    def test_probe_accepts_provider_auto_singleton_fanout(self):
        chat = probe_response()
        chat["choices"][0]["message"]["tool_calls"].append({
            "function": {
                "name": harness.PROBE_NAME,
                "arguments": json.dumps({"nonce": harness.PROBE_NONCE}),
            },
        })
        self.assertEqual(harness.validate_probe_response(chat), (True, "ok"))

        responses = {
            "status": "completed",
            "output": [
                {
                    "type": "function_call", "call_id": f"call-{index}",
                    "name": harness.PROBE_NAME,
                    "arguments": json.dumps({"nonce": harness.PROBE_NONCE}),
                }
                for index in range(2)
            ],
        }
        self.assertEqual(
            harness.validate_probe_response(responses, protocol="responses"),
            (True, "ok"),
        )

    def test_snapshot_projection_preserves_terminal_outcomes(self):
        for status, outcome in (
            ("completed", "success"), ("failed", "failed"),
            ("blocked", "blocked"), ("cancelled", "cancelled"),
        ):
            snapshot = {
                "tasks": [
                    {"run_id": "run-1", "status": status},
                    {"run_id": "other", "status": "processing"},
                ],
                "graphs": [
                    {"run_id": "run-1", "status": status, "outcome": outcome},
                    {"run_id": "other", "status": "completed", "outcome": "success"},
                ],
                "pending_interactions": [],
            }
            projected = harness.project_snapshot(snapshot, "run-1")
            self.assertTrue(projected["graph_terminal"])
            self.assertTrue(projected["tasks_terminal"])
            self.assertEqual(projected["graph_outcomes"], [outcome])

    def test_pending_task_is_not_no_graph_terminal(self):
        projected = harness.project_snapshot({
            "tasks": [{"run_id": "run-1", "status": "pending"}],
            "graphs": [],
            "pending_interactions": [],
        }, "run-1")
        self.assertFalse(projected["tasks_terminal"])
        self.assertEqual(projected["active_tasks"], 1)

    def test_graph_terminal_task_scope_excludes_control_plane_scheduler(self):
        tasks = [
            {"id": "origin", "graph_id": "", "status": "completed"},
            {"id": "work", "graph_id": "graph-1", "status": "completed"},
            {"id": "acceptance", "graph_id": "graph-1", "status": "completed"},
            {"id": "final-report", "graph_id": "", "status": "processing"},
        ]
        scoped = harness.terminal_task_scope(tasks, [{"graph_id": "graph-1"}])
        self.assertEqual([task["id"] for task in scoped], ["work", "acceptance"])
        self.assertEqual(harness.terminal_task_scope(tasks, []), tasks)

    def test_monitor_distinguishes_process_exit_and_hard_kill(self):
        exited = harness.monitor_run("http://127.0.0.1:1", "token", 999_999_999, "run-1",
                                     time.time(), 10, os.devnull, poll_sec=0, terminal_grace_sec=0)
        self.assertEqual(exited["process_terminal"], "process_exited")
        killed = harness.monitor_run("http://127.0.0.1:1", "token", os.getpid(), "run-1",
                                     time.time() - 2, 1, os.devnull, poll_sec=0, terminal_grace_sec=0)
        self.assertEqual(killed["process_terminal"], "external_hard_kill")
        self.assertTrue(killed["external_hard_kill"])

    def test_monitor_graph_and_no_graph_terminals_do_not_use_quiet(self):
        graph_snapshot = {
            "tasks": [{"run_id": "run-1", "status": "completed"}],
            "graphs": [{"run_id": "run-1", "status": "completed", "outcome": "success"}],
            "pending_interactions": [],
        }
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch.object(harness, "process_alive", return_value=True), \
                mock.patch.object(harness, "http_json", return_value=(200, graph_snapshot)):
            result = harness.monitor_run("http://local", "token", 1, "run-1", time.time(), 10,
                                         str(Path(directory) / "snapshot.json"), poll_sec=0,
                                         terminal_grace_sec=0)
        self.assertEqual(result["process_terminal"], "graph_terminal")
        self.assertTrue(result["graph_lifecycle_terminal"])

        no_graph_snapshot = {
            "tasks": [{"run_id": "run-1", "status": "blocked"}],
            "graphs": [], "pending_interactions": [],
        }
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch.object(harness, "process_alive", return_value=True), \
                mock.patch.object(harness, "http_json", return_value=(200, no_graph_snapshot)):
            result = harness.monitor_run("http://local", "token", 1, "run-1", time.time(), 10,
                                         str(Path(directory) / "snapshot.json"), poll_sec=0,
                                         terminal_grace_sec=0)
        self.assertEqual(result["process_terminal"], "no_graph_terminal")
        self.assertFalse(result["graph_lifecycle_terminal"])

    def test_pending_intervention_prevents_no_graph_terminal(self):
        snapshot = {
            "tasks": [
                {"run_id": "run-1", "status": "blocked"},
                {"run_id": "run-1", "status": "processing", "event_type": "__scheduler__"},
            ],
            "graphs": [], "pending_interactions": [],
        }
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch.object(harness, "process_alive", side_effect=[True, False]), \
                mock.patch.object(harness, "http_json", return_value=(200, snapshot)), \
                mock.patch.object(harness.time, "sleep", return_value=None):
            result = harness.monitor_run("http://local", "token", 1, "run-1", time.time(), 10,
                                         str(Path(directory) / "snapshot.json"), poll_sec=0,
                                         terminal_grace_sec=0)
        self.assertEqual(result["process_terminal"], "process_exited")

    def test_outcome_projection_tracks_delivery_ack_without_body(self):
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory)
            journal = state / "task-outcomes" / "task-outcomes.jsonl"
            journal.parent.mkdir()
            entries = [
                {
                    "kind": "terminal_intent_commit",
                    "record": {
                        "outcome_ref": "outcome:1",
                        "outcome": {
                            "run_id": "run-1", "task_id": "task-1", "attempt_id": "attempt-1",
                            "status": "completed", "reason_code": "", "checkpoint_state": "sealed",
                            "summary": "must-not-leak", "result": {"secret": "must-not-leak"},
                        },
                    },
                },
                {"kind": "delivery_ack", "ack_ref": "outcome:1", "record": {}},
            ]
            journal.write_text("\n".join(json.dumps(entry) for entry in entries) + "\n", encoding="utf-8")
            outcomes, pending = harness.safe_outcomes(state, "run-1")
            self.assertEqual(pending, 0)
            self.assertTrue(outcomes[0]["delivery_acked"])
            serialized = json.dumps(outcomes)
            self.assertNotIn("must-not-leak", serialized)
            self.assertNotIn("result", serialized)

    def test_trace_metrics_detect_scheduler_batch_and_draft_call_index(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            log = root / ".agentgo" / "sessions" / "s1" / "logs" / "trace.jsonl"
            log.parent.mkdir(parents=True)
            events = [
                {"ts": "0", "kind": "context_manifest_built", "run_id": "run-1",
                 "task_id": "scheduler", "turn_id": "turn-1",
                 "description": json.dumps([{"source_ref": "prompt-phase:scheduler:draft-edit"}])},
                {"ts": "1", "kind": "llm_call_end", "run_id": "run-1", "task_id": "scheduler",
                 "turn_id": "turn-1",
                 "prompt_tokens": 3847, "completion_tokens": 100, "tool_calls_count": 2},
                {"ts": "2", "kind": "tool_call", "run_id": "run-1", "task_id": "scheduler",
                 "turn_id": "turn-1", "tool": "create_graph_draft"},
                {"ts": "3", "kind": "tool_call", "run_id": "run-1", "task_id": "scheduler",
                 "turn_id": "turn-1", "tool": "patch_graph_draft"},
            ]
            log.write_text("\n".join(json.dumps(event) for event in events) + "\n", encoding="utf-8")
            metrics, _ = harness.trace_metrics(root, "run-1", {"scheduler"})
            self.assertEqual(metrics["first_scheduler_prompt_tokens"], 3847)
            self.assertEqual(metrics["first_graph_draft_call_index"], 1)
            self.assertTrue(metrics["known_incidents"]["scheduler_tool_batch_exceeded"])

    def test_trace_metrics_allows_final_report_read_batch_and_skipped_phase_fanout(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            log = root / ".agentgo" / "sessions" / "s1" / "logs" / "trace.jsonl"
            log.parent.mkdir(parents=True)
            events = [
                {"ts": "1", "kind": "context_manifest_built", "run_id": "run-1",
                 "task_id": "scheduler", "turn_id": "final",
                 "description": json.dumps([{"source_ref": "prompt-phase:scheduler:final-report"}])},
                {"ts": "2", "kind": "llm_call_end", "run_id": "run-1", "task_id": "scheduler",
                 "turn_id": "final", "tool_calls_count": 2},
                {"ts": "3", "kind": "tool_call", "run_id": "run-1", "task_id": "scheduler",
                 "turn_id": "final", "tool": "read_graph"},
                {"ts": "4", "kind": "tool_call", "run_id": "run-1", "task_id": "scheduler",
                 "turn_id": "final", "tool": "read_content_ref"},
                {"ts": "5", "kind": "context_manifest_built", "run_id": "run-1",
                 "task_id": "scheduler", "turn_id": "create",
                 "description": json.dumps([{"source_ref": "prompt-phase:scheduler:draft-create"}])},
                {"ts": "6", "kind": "llm_call_end", "run_id": "run-1", "task_id": "scheduler",
                 "turn_id": "create", "tool_calls_count": 2},
                {"ts": "7", "kind": "tool_call", "run_id": "run-1", "task_id": "scheduler",
                 "turn_id": "create", "tool": "create_graph_draft"},
                {"ts": "8", "kind": "tool_call_skipped", "run_id": "run-1", "task_id": "scheduler",
                 "turn_id": "create", "tool": "create_graph_draft",
                 "reason": "phase_single_action_fanout"},
            ]
            log.write_text("\n".join(json.dumps(event) for event in events) + "\n", encoding="utf-8")
            metrics, _ = harness.trace_metrics(root, "run-1", {"scheduler"})
            self.assertFalse(metrics["known_incidents"]["scheduler_tool_batch_exceeded"])

    def test_trace_metrics_marks_provider_invalid_request_as_architecture_incident(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            log = root / ".agentgo" / "sessions" / "s1" / "logs" / "trace.jsonl"
            log.parent.mkdir(parents=True)
            event = {
                "ts": "1", "kind": "llm_call_end", "run_id": "run-1", "task_id": "worker",
                "failure_kind": "invalid_request", "http_status": 400,
                "error": "input: missing field `content`",
            }
            log.write_text(json.dumps(event) + "\n", encoding="utf-8")
            metrics, _ = harness.trace_metrics(root, "run-1", {"scheduler"})
            self.assertEqual(metrics["invocation_failures"], {"invalid_request": 1})
            self.assertTrue(metrics["known_incidents"]["provider_invalid_request"])

    def test_trace_metrics_marks_output_limit_failure_as_architecture_incident(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            log = root / ".agentgo" / "sessions" / "s1" / "logs" / "trace.jsonl"
            log.parent.mkdir(parents=True)
            event = {
                "ts": "1", "kind": "llm_call_end", "run_id": "run-1", "task_id": "verifier",
                "failure_kind": "output_limit_exceeded",
                "error": "tool_calls.count actual=3 limit=1",
            }
            log.write_text(json.dumps(event) + "\n", encoding="utf-8")
            metrics, _ = harness.trace_metrics(root, "run-1", {"scheduler"})
            self.assertTrue(metrics["known_incidents"]["invocation_output_limit_exceeded"])

    def test_finalize_keeps_architecture_and_task_verdict_separate(self):
        with tempfile.TemporaryDirectory() as directory:
            result_path = Path(directory) / "result.json"
            judge_path = Path(directory) / "judge.json"
            harness.atomic_json(result_path, {"architecture_ok": True, "graph_outcomes": ["success"]})
            harness.atomic_json(judge_path, {"verdict": "failed", "patch_lines": 5, "tampered": False})
            result = harness.finalize_result(str(result_path), str(judge_path))
            self.assertTrue(result["architecture_ok"])
            self.assertFalse(result["task_resolved"])


if __name__ == "__main__":
    unittest.main()
