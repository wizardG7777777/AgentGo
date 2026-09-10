"""通用运行事实采集的离线样本；不启动模型、Shell 或 SWE 任务。"""

from pathlib import Path
import json
import tempfile
import unittest

from runtime_audit import EvidenceReader, collect_runtime, go_json_digest


def write_json(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value), encoding="utf-8")


def write_journal(path, entries):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("".join(json.dumps(entry) + "\n" for entry in entries), encoding="utf-8")


class RuntimeFixture:
    def __init__(self, root):
        self.root = root
        self.state = root / ".agentgo" / "state"
        self.session = root / ".agentgo" / "sessions" / "session-1"
        self.snapshot = root / "snapshot.final.json"
        self.monitor = root / "monitor.json"
        self.graph = {"graph_id": "graph-1", "run_id": "run-1", "revision": 1,
                      "status": "completed", "outcome": "success"}
        tasks = [
            {"id": "task-1", "run_id": "run-1", "graph_id": "graph-1", "status": "completed", "outcome_ref": "outcome:task"},
            {"id": "report-1", "run_id": "run-1", "final_report_graph_id": "graph-1", "status": "completed", "outcome_ref": "outcome:report"},
        ]
        write_json(self.snapshot, {"tasks": tasks, "graphs": [self.graph]})
        write_json(self.monitor, {"run_identity_visible": True, "process_terminal": "graph_terminal", "wall_sec": 1})
        self.identity = {"run_id": "run-1", "task_id": "task-1", "attempt_id": "attempt-1", "invocation_id": "invocation-1"}
        receipt = {"schema": "agentgo.graph-apply-receipt/v2", "status": "applied", "graph_id": "graph-1",
                   "revision": 1, "source_request": "request-1"}
        self.events = [
            {**self.identity, "kind": "llm_call_start"},
            {**self.identity, "kind": "llm_call_end", "prompt_tokens": 9000, "completion_tokens": 20},
            {**self.identity, "kind": "tool_call", "call_id": "create", "tool": "apply_graph_change"},
            {**self.identity, "kind": "tool_result", "call_id": "create", "tool": "apply_graph_change",
             "tool_dispatched": True, "tool_result_content": json.dumps(receipt)},
            {**self.identity, "kind": "tool_call", "call_id": "shell", "tool": "run_shell"},
            {**self.identity, "kind": "tool_result", "call_id": "shell", "tool": "run_shell", "tool_dispatched": True},
            {**self.identity, "kind": "shell_executed", "call_id": "shell", "tool": "run_shell",
             "shell_exec": {"schema": "agentgo.shell-execution/v2", "process_started": True,
                            "outcome": "failure", "exit_code": 3, "exit_code_scope": "whole_command"}},
        ]
        self.trace = self.session / "logs" / "task.jsonl"
        self.save_events()
        write_journal(self.session / "turns.jsonl", [{"schema": "agentgo.model-output/v1", "identity": self.identity,
            "status": "completed", "result": {"schema": "agentgo.model-result/v1"}}])
        write_journal(self.state / "context-snapshots-v2" / "context-snapshots.jsonl", [{"version": 1,
            "record": {"snapshot": {"schema": "agentgo.context/v2", "invocation_id": "invocation-1",
                                    "context_policy_id": "context:default/v11", "fragments": []}}}])
        outcomes = []
        for task in tasks:
            outcomes.extend([
                {"version": 1, "kind": "commit", "record": {"outcome_ref": task["outcome_ref"], "outcome": {
                    "schema": "agentgo.task-outcome/v4", "run_id": "run-1", "task_id": task["id"], "status": "completed"}}},
                {"version": 1, "kind": "delivery_ack", "ack_ref": task["outcome_ref"]},
            ])
        write_journal(self.state / "task-outcomes-v3" / "task-outcomes.jsonl", outcomes)
        self.usage = self.state / "run-usage-v2" / "run-budgets.jsonl"
        write_journal(self.usage, [
            {"schema": "agentgo.run-budget-record/v1", "run_id": "run-1", "kind": "reserve", "reservation": {"reservation_id": "reserve-1"}},
            {"schema": "agentgo.run-budget-record/v1", "run_id": "run-1", "kind": "settle", "settlement": {
                "reservation_id": "reserve-1", "usage": {"model_calls": 1, "tool_actions": 2}}},
        ])
        write_journal(self.state / "loop-facts-v3" / "task.jsonl", [{"schema": "agentgo.loop-store-record/v1",
            "checkpoint": {"run_id": "run-1", "attempt_id": "attempt-1"}}])
        current = {"schema": "agentgo.graph/v6", "state_version": 1, "status": "completed",
                   "definition": {"schema": "agentgo.graph/v6", "graph_id": "graph-1", "run_id": "run-1", "revision": 1,
                                  "nodes": [{"kind": "agentTask", "node_id": "work"}]},
                   "executions": {"work": {"status": "completed", "outcome_ref": "outcome:task", "inputs": {"values": {}}}},
                   "results": {"work": {"ref": "result:work"}},
                   "requests": {"request-1": {"action": "create"}, "start": {"action": "start"}},
                   "completion": {"schema": "agentgo.graph-completion/v1", "status": "committed", "outcome": "success", "result_refs": ["result:work"]}}
        entry = {"sequence": 1, "previous_digest": "", "digest": "", "snapshot": current}
        entry["digest"] = go_json_digest(entry)
        self.graph_journal = self.state / "graphs-v6" / "graph.jsonl"
        write_journal(self.graph_journal, [entry])

    def save_events(self):
        write_journal(self.trace, self.events)

    def collect(self):
        return collect_runtime(self.snapshot, self.monitor, self.root, "run-1", True)


class RuntimeAuditTest(unittest.TestCase):
    def test_graph_digest_tampering_cannot_pass_audit(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = RuntimeFixture(Path(directory))
            entry = json.loads(fixture.graph_journal.read_text(encoding="utf-8"))
            entry["snapshot"]["completion"]["result_refs"] = ["forged"]
            write_journal(fixture.graph_journal, [entry])
            result = fixture.collect()
            self.assertIsNone(result["architecture_ok"])
            self.assertIn("dataflow_digest_invalid", {i["code"] for i in result["evidence_issues"]})

    def test_nonzero_shell_exit_and_large_prompt_do_not_mean_architecture_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = RuntimeFixture(Path(directory))
            result = fixture.collect()
            self.assertEqual(result["evidence_issues"], [])
            self.assertTrue(result["architecture_ok"], result["architecture_checks"])
            self.assertEqual(result["metrics"]["tools"]["shell_outcomes"], {"failure": 1})
            self.assertNotIn("first_prompt_at_most_8000", result["architecture_checks"])
            self.assertNotIn("task_resolved", result)

    def test_foreign_run_and_duplicate_identical_records_do_not_inflate_usage(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = RuntimeFixture(Path(directory))
            fixture.events.append(dict(fixture.events[1]))
            fixture.events.append({"run_id": "other", "kind": "tool_call", "tool": "run_check"})
            fixture.save_events()
            result = fixture.collect()
            self.assertTrue(result["architecture_ok"])
            self.assertEqual(result["metrics"]["completed_invocations"], 1)
            self.assertEqual(result["retired_tools_called"], [])

    def test_truncated_journal_is_reported_without_erasing_valid_prefix(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = RuntimeFixture(Path(directory))
            with fixture.trace.open("a", encoding="utf-8") as handle:
                handle.write('{"unfinished":')
            result = fixture.collect()
            self.assertIsNone(result["architecture_ok"])
            self.assertEqual(result["metrics"]["completed_invocations"], 1)
            self.assertIn("journal_record_invalid", {i["code"] for i in result["evidence_issues"]})

    def test_missing_new_directory_does_not_fall_back_to_old_ledger(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = RuntimeFixture(Path(directory))
            legacy = fixture.state / "run-budgets" / "run-budgets.jsonl"
            legacy.parent.mkdir()
            fixture.usage.rename(legacy)
            result = fixture.collect()
            self.assertFalse(result["metrics"]["run_usage"]["present"])
            self.assertTrue(result["evidence_issues"])

    def test_actual_shell_execution_must_have_its_own_fact(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = RuntimeFixture(Path(directory))
            fixture.events = [e for e in fixture.events if e["kind"] != "shell_executed"]
            fixture.save_events()
            codes = {i["code"] for i in fixture.collect()["evidence_issues"]}
            self.assertIn("successful_shell_execution_fact_missing", codes)

    def test_call_id_is_scoped_by_invocation_and_conflicting_receipts_reject(self):
        reader = EvidenceReader()
        fields = ("task_id", "attempt_id", "invocation_id", "call_id")
        first = dict(zip(fields, ("t", "a", "i1", "same")))
        second = {**first, "invocation_id": "i2"}
        self.assertEqual(len(reader.unique([first, second], fields, "test")), 2)
        self.assertEqual(reader.issues, [])
        reader.unique([first, {**first, "error": "冲突"}], fields, "test")
        self.assertEqual(reader.issues[0]["code"], "record_identity_conflict")

    def test_unfinished_invocation_is_not_reclassified_as_observation_or_stall(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = RuntimeFixture(Path(directory))
            fixture.events.append({**fixture.identity, "invocation_id": "in-flight", "kind": "llm_call_start"})
            fixture.save_events()
            result = fixture.collect()
            self.assertFalse(result["execution_complete"])
            self.assertEqual(result["incomplete_execution"]["invocations"], ["in-flight"])
            self.assertNotIn("control_checkpoint_unavailable", result["known_incidents"])


if __name__ == "__main__":
    unittest.main()
