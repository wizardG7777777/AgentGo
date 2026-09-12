"""读取 AgentGo 的通用运行事实，不执行工具，也不判断 pytest 是否通过。"""

from __future__ import annotations

from collections import Counter
import json
import hashlib
from pathlib import Path


RESULT_SCHEMA = "agentgo.swe-result/v5"
TERMINAL_TASK = {"completed", "failed", "blocked", "cancelled"}
TERMINAL_GRAPH = {"completed", "failed", "blocked", "cancelled"}
TERMINAL_OUTCOME = {"success", "failed", "blocked", "cancelled"}
RETIRED_TOOLS = {
    "record_observation_delta", "submit_change_decision", "submit_recovery_decision", "run_check",
    "edit_file", "write_file", "list_dir", "grep_search", "glob_search", "read_content_ref", "publish_task",
    "create_graph_draft", "configure_simple_graph_draft", "validate_current_graph_draft",
    "commit_current_graph_draft", "start_graph", "patch_graph_draft",
}


class EvidenceReader:
    """缺失、损坏、版本错误独立记录；保留坏行之前的事实，不改写原文件。"""

    def __init__(self):
        self.issues = []

    def issue(self, code, source):
        item = {"code": code, "source": str(source)}
        if item not in self.issues:
            self.issues.append(item)

    def object(self, path):
        try:
            value = json.loads(path.read_text(encoding="utf-8"))
            if not isinstance(value, dict):
                raise ValueError()
            return value
        except (OSError, UnicodeError, ValueError):
            self.issue("required_json_unavailable", path)
            return {}

    def journal(self, paths, *, required=True, version=None, schema=None):
        paths = sorted(paths)
        if not paths and required:
            self.issue("required_journal_missing", "未匹配到运行记录")
        for path in paths:
            try:
                with path.open(encoding="utf-8") as handle:
                    for number, line in enumerate(handle, 1):
                        if not line.strip():
                            continue
                        source = f"{path}:{number}"
                        try:
                            entry = json.loads(line)
                            if not isinstance(entry, dict):
                                raise ValueError()
                        except ValueError:
                            self.issue("journal_record_invalid", source)
                            break
                        if version is not None and entry.get("version") != version:
                            self.issue("journal_version_rejected", source)
                            continue
                        if schema is not None and entry.get("schema") != schema:
                            self.issue("journal_schema_rejected", source)
                            continue
                        yield entry
            except (OSError, UnicodeError):
                self.issue("journal_unreadable", path)

    def unique(self, records, fields, source):
        result = {}
        for record in records:
            key = tuple(record.get(field) for field in fields)
            if not all(isinstance(value, str) and value for value in key):
                self.issue("record_identity_missing", source)
                continue
            if key in result and result[key] != record:
                self.issue("record_identity_conflict", "/".join(key))
            else:
                result[key] = record
        return result

    def mapping(self, value, source):
        if isinstance(value, dict):
            return value
        self.issue("record_object_invalid", source)
        return {}


def _objects(value):
    return [item for item in value if isinstance(item, dict)] if isinstance(value, list) else []


def _nonnegative(value, reader, source):
    if isinstance(value, bool) or not isinstance(value, int) or value < 0:
        reader.issue("usage_value_invalid", source)
        return 0
    return value


def go_json_digest(value):
    raw = json.dumps(value, ensure_ascii=False, separators=(",", ":"))
    raw = raw.replace("&", "\\u0026").replace("<", "\\u003c").replace(">", "\\u003e").replace("\u2028", "\\u2028").replace("\u2029", "\\u2029")
    return "sha256:" + hashlib.sha256(raw.encode("utf-8")).hexdigest()


def collect_runtime(snapshot_path, monitor_path, project_root, run_id, startup_probe_passed):
    reader = EvidenceReader()
    snapshot, monitor = reader.object(Path(snapshot_path)), reader.object(Path(monitor_path))
    return audit_runtime(snapshot, monitor, project_root, run_id, startup_probe_passed, reader)


def audit_runtime(snapshot, monitor, project_root, run_id, startup_probe_passed, reader=None):
    """终态监控与最终判读使用同一份持久化事实审计，不建立第二套结算判据。"""
    reader = reader if reader is not None else EvidenceReader()
    root, state = Path(project_root), Path(project_root) / ".agentgo" / "state"
    for key in ("tasks", "graphs"):
        if not isinstance(snapshot.get(key), list):
            reader.issue("snapshot_collection_missing", key)
    tasks = [t for t in _objects(snapshot.get("tasks")) if t.get("run_id") == run_id]
    graphs = [g for g in _objects(snapshot.get("graphs")) if g.get("run_id") == run_id]
    graph_ids = {g.get("graph_id") for g in graphs}
    final_reports = [t for t in tasks if t.get("final_report_graph_id")]
    graph_tasks = [t for t in tasks if t.get("graph_id")]
    events = [e for e in reader.journal((root / ".agentgo" / "sessions").glob("*/logs/*.jsonl"))
              if e.get("run_id") == run_id]
    starts = reader.unique([e for e in events if e.get("kind") == "llm_call_start"],
                           ("invocation_id",), "llm_call_start")
    ends = reader.unique([e for e in events if e.get("kind") == "llm_call_end"],
                         ("invocation_id",), "llm_call_end")
    if tasks and not starts:
        reader.issue("model_invocation_facts_missing", run_id)
    unresolved_invocations = sorted(key[0] for key in starts.keys() - ends.keys())
    if ends.keys() - starts.keys():
        reader.issue("invocation_start_missing", run_id)
    call_fields = ("task_id", "attempt_id", "invocation_id", "call_id")
    tool_starts = reader.unique([e for e in events if e.get("kind") == "tool_call"], call_fields, "tool_call")
    tool_results = reader.unique([e for e in events if e.get("kind") == "tool_result"], call_fields, "tool_result")
    shell_events = reader.unique([e for e in events if e.get("kind") == "shell_executed"], call_fields, "shell_executed")
    unresolved_tools = sorted("/".join(key) for key in tool_starts.keys() - tool_results.keys())
    if tool_results.keys() - tool_starts.keys():
        reader.issue("tool_request_missing", run_id)
    for key in tool_starts:
        if (key[2],) not in starts:
            reader.issue("tool_invocation_missing", "/".join(key))
    for key, event in tool_results.items():
        if not isinstance(event.get("tool_dispatched"), bool):
            reader.issue("tool_dispatch_fact_missing", "/".join(key))
        if event.get("tool") == "run_shell" and not event.get("error") and key not in shell_events:
            reader.issue("successful_shell_execution_fact_missing", "/".join(key))
    shell_outcomes = Counter()
    for key, event in shell_events.items():
        result = reader.mapping(event.get("shell_exec"), "shell_exec")
        outcome = result.get("outcome")
        if result.get("schema") != "agentgo.shell-execution/v2" or not isinstance(result.get("process_started"), bool):
            reader.issue("shell_execution_schema_or_start_fact_missing", "/".join(key))
        if outcome not in {"success", "failure", "timeout", "cancelled", "start_failed"}:
            reader.issue("shell_outcome_invalid", "/".join(key))
        shell_outcomes[outcome or "unknown"] += 1
        if key not in tool_results:
            reader.issue("shell_tool_receipt_missing", "/".join(key))
        if outcome in {"success", "failure"} and (
                not isinstance(result.get("exit_code"), int) or isinstance(result.get("exit_code"), bool) or result.get("process_started") is not True
                or result.get("exit_code_scope") not in {"whole_command", "last_pipeline_command"}):
            reader.issue("shell_exit_fact_missing", "/".join(key))
    retired_calls = sorted({e.get("tool") for e in events if e.get("tool") in RETIRED_TOOLS})

    # Context 本体没有 RunID；只关联本 Run 的实际 Invocation 集合。
    context_entries = reader.journal([state / "context-snapshots-v3" / "context-snapshots.jsonl"], version=1)
    contexts = {}
    dispositions, policies = Counter(), set()
    for entry in context_entries:
        record = reader.mapping(entry.get("record"), "context.record")
        context = reader.mapping(record.get("snapshot"), "context.snapshot")
        invocation = context.get("invocation_id")
        if (invocation,) not in starts:
            continue
        if context.get("schema") != "agentgo.context/v3":
            reader.issue("context_schema_rejected", invocation)
            continue
        if invocation in contexts and contexts[invocation] != context:
            reader.issue("context_identity_conflict", invocation)
        contexts[invocation] = context
        policies.add(context.get("context_policy_id", ""))
        dispositions.update(f.get("disposition", "unknown") for f in _objects(context.get("fragments")))
    missing_context = sorted(key[0] for key in starts if key[0] not in contexts)
    if missing_context:
        reader.issue("invocation_context_missing", ",".join(missing_context))
    outputs = [o for o in reader.journal((root / ".agentgo" / "sessions").glob("*/turns.jsonl"))
               if reader.mapping(o.get("identity"), "model_output.identity").get("run_id") == run_id]
    output_ids = set()
    for output in outputs:
        identity = output.get("identity") or {}
        invocation = identity.get("invocation_id")
        if output.get("schema") != "agentgo.model-output/v1" or not invocation:
            reader.issue("model_output_schema_or_identity_invalid", run_id)
            continue
        if invocation in output_ids:
            reader.issue("duplicate_model_output_terminal", invocation)
        output_ids.add(invocation)
        if output.get("status") == "completed" and reader.mapping(output.get("result"), "model_output.result").get("schema") != "agentgo.model-result/v1":
            reader.issue("complete_model_result_missing", invocation)
    if any(key[0] not in output_ids for key in ends):
        reader.issue("invocation_output_missing", run_id)

    # TaskOutcome 提交与投递回执分别读取，禁止把 UI 文本当作完成历史。
    outcomes, acknowledgements = {}, set()
    for entry in reader.journal([state / "task-outcomes-v4" / "task-outcomes.jsonl"], version=1):
        if entry.get("kind") == "delivery_ack":
            acknowledgements.add(entry.get("ack_ref"))
        record = reader.mapping(entry.get("record", {}), "outcome.record")
        value = reader.mapping(record.get("outcome", {}), "outcome.value")
        if value.get("run_id") != run_id:
            continue
        ref = record.get("outcome_ref")
        schemas = {"agentgo.task-outcome/v5"}
        if not ref or value.get("schema") not in schemas:
            reader.issue("outcome_schema_or_identity_invalid", run_id)
            continue
        if ref in outcomes and outcomes[ref] != value:
            reader.issue("outcome_identity_conflict", ref)
        outcomes[ref] = value
    outcome_projection = [{**{field: value.get(field) for field in (
        "task_id", "graph_id", "node_id", "activation_id", "attempt_id", "status", "reason_code")},
        "outcome_ref": ref, "delivery_acked": ref in acknowledgements,
        "fulfillment_present": isinstance(value.get("fulfillment"), dict)} for ref, value in sorted(outcomes.items())]

    reservations, settlements, usage = {}, {}, Counter()
    usage_present = False
    for entry in reader.journal([state / "run-usage-v2" / "run-budgets.jsonl"], schema="agentgo.run-budget-record/v1"):
        if entry.get("run_id") != run_id:
            continue
        usage_present = True
        if entry.get("kind") == "reserve":
            value = reader.mapping(entry.get("reservation"), "run_usage.reservation")
            identity = value.get("reservation_id")
            if not identity or identity in reservations:
                reader.issue("reservation_identity_invalid", run_id)
            reservations[identity] = value
        if entry.get("kind") == "settle":
            value = reader.mapping(entry.get("settlement"), "run_usage.settlement")
            identity = value.get("reservation_id")
            if not identity:
                reader.issue("settlement_identity_missing", run_id)
                continue
            if identity in settlements:
                if settlements[identity] != value:
                    reader.issue("settlement_identity_conflict", identity)
                continue
            settlements[identity] = value
            charge = reader.mapping(value.get("usage"), "run_usage.usage")
            for field in ("prompt_tokens", "completion_tokens", "model_calls", "tool_actions", "attempts", "cost_micros"):
                usage[field] += _nonnegative(charge.get(field, 0), reader, identity)
    active_reservations = sorted(key for key in reservations if key not in settlements and key)
    if settlements.keys() - reservations.keys():
        reader.issue("settlement_reservation_missing", run_id)
    if not usage_present:
        reader.issue("run_usage_missing", run_id)
    attempts, loop_records = set(), 0
    for entry in reader.journal((state / "loop-facts-v3").glob("*.jsonl"), schema="agentgo.loop-store-record/v1"):
        checkpoint = entry.get("checkpoint") or reader.mapping(entry.get("settlement", {}), "loop.settlement").get("checkpoint") or {}
        checkpoint = reader.mapping(checkpoint, "loop.checkpoint")
        if checkpoint.get("run_id") == run_id:
            loop_records += 1
            if checkpoint.get("attempt_id"):
                attempts.add(checkpoint["attempt_id"])

    definitions, latest, graph_digests = {}, {}, {}
    for entry in reader.journal((state / "graphs-v8").glob("*.jsonl")):
        current = reader.mapping(entry.get("snapshot"), "dataflow.snapshot")
        definition = reader.mapping(current.get("definition"), "dataflow.definition")
        if definition.get("run_id") != run_id:
            continue
        graph_id, revision = definition.get("graph_id"), definition.get("revision")
        if current.get("schema") != "agentgo.graph/v7" or definition.get("schema") != "agentgo.graph/v7" or not graph_id or not isinstance(revision, int):
            reader.issue("dataflow_schema_or_identity_invalid", run_id)
            continue
        if not isinstance(definition.get("nodes"), list) or any(n.get("kind") != "agentTask" for n in _objects(definition.get("nodes"))):
            reader.issue("retired_graph_kind", graph_id)
        if any(k in definition for k in ("root", "next", "requires_acceptance")):
            reader.issue("retired_control_definition", graph_id)
        signed = dict(entry)
        signed["digest"] = ""
        if entry.get("digest") != go_json_digest(signed) or entry.get("previous_digest", "") != graph_digests.get(graph_id, ""):
            reader.issue("dataflow_digest_invalid", graph_id)
            continue
        graph_digests[graph_id] = entry["digest"]
        previous = latest.get(graph_id)
        if entry.get("sequence") != current.get("state_version") or (previous and current.get("state_version", 0) != previous.get("state_version", 0) + 1):
            reader.issue("dataflow_sequence_invalid", graph_id)
        key = (graph_id, revision)
        if key in definitions and definitions[key] != definition:
            reader.issue("graph_definition_identity_conflict", graph_id)
        definitions[key] = definition
        latest[graph_id] = current
    deliveries = []
    for path in sorted((state / "deliveries-v3").glob("*.json")):
        delivery = reader.object(path)
        if delivery.get("run_id") != run_id:
            continue
        if delivery.get("schema") != "agentgo.delivery/v2":
            reader.issue("delivery_schema_rejected", path)
        deliveries.append(delivery)
    delivery_incomplete = any(d.get("status") == "committed" and not all(d.get(f) for f in (
        "delivery_id", "completion_ref", "candidate_ref", "effect_ref", "graph_id")) for d in deliveries)
    applied_receipts = []
    for event in tool_results.values():
        if event.get("tool") != "apply_graph_change" or event.get("error"):
            continue
        try:
            receipt = json.loads(event.get("tool_result_content") or "")
            if not isinstance(receipt, dict) or receipt.get("schema") != "agentgo.graph-apply-receipt/v2" or receipt.get("status") != "applied":
                raise ValueError()
            key = (receipt.get("graph_id"), receipt.get("revision"))
            if key not in definitions or receipt.get("source_request") not in (latest.get(key[0], {}).get("requests") or {}):
                raise ValueError()
            applied_receipts.append(receipt)
        except (TypeError, ValueError):
            reader.issue("graph_apply_receipt_not_committed", event.get("call_id"))
    completions_valid = True
    missing_delivery = False
    for graph_id in graph_ids:
        current = latest.get(graph_id, {})
        if current.get("status") not in TERMINAL_GRAPH:
            completions_valid = False
            continue
        completion = reader.mapping(current.get("completion"), "dataflow.completion")
        if completion.get("schema") != "agentgo.graph-completion/v1" or completion.get("status") != "committed":
            completions_valid = False
        refs = {r.get("ref"): r for r in (current.get("results") or {}).values() if isinstance(r, dict)}
        if any(ref not in refs for ref in completion.get("result_refs", [])):
            reader.issue("completion_result_ref_invalid", graph_id)
        candidate = completion.get("candidate_ref")
        if completion.get("outcome") == "success" and candidate:
            if not any(d.get("delivery_id") == completion.get("delivery_ref") and d.get("candidate_ref") == candidate
                       and d.get("graph_id") == graph_id and d.get("status") == "committed" for d in deliveries):
                missing_delivery = True
        for node_id, execution in (current.get("executions") or {}).items():
            ref = execution.get("outcome_ref")
            if execution.get("status") in TERMINAL_TASK and ref not in outcomes:
                reader.issue("agent_task_outcome_missing", node_id)
            for slot, value in ((execution.get("inputs") or {}).get("values") or {}).items():
                source_ref = value.get("ref")
                if str(source_ref).startswith("result:") and source_ref not in refs:
                    reader.issue("agent_task_input_ref_invalid", node_id + ":" + slot)
    failures = Counter(e.get("failure_kind") for e in ends.values() if e.get("failure_kind"))
    model_incidents = [{"invocation_id": e["invocation_id"], "failure_kind": e.get("failure_kind"),
                        "provider_code": e.get("provider_code"), "failure_phase": e.get("failure_phase")}
                       for e in ends.values() if e.get("failure_kind") in {"invalid_request", "protocol_incompatible", "malformed_response", "output_truncated"}]
    graph_outcomes = [g.get("outcome", "") for g in graphs]
    settled = not unresolved_invocations and not unresolved_tools and not active_reservations
    known = {"retired_tool_called": bool(retired_calls),
             "model_usage_mismatch": settled and usage_present and usage["model_calls"] != len(ends),
             "success_without_delivery": missing_delivery,
             "committed_delivery_incomplete": delivery_incomplete}
    checks = {
        "run_identity_visible": bool(tasks) and bool(monitor.get("run_identity_visible")),
        "graph_definitions_committed": bool(graphs) and all((g.get("graph_id"), g.get("revision")) in definitions for g in graphs),
        "graph_apply_receipts_present": bool(graphs) and all(any(r.get("graph_id") == g.get("graph_id") for r in applied_receipts) for g in graphs),
        "graph_started": bool(graphs) and all(any(r.get("action") == "start" for r in (latest.get(g.get("graph_id"), {}).get("requests") or {}).values()) for g in graphs),
        "graph_terminal": bool(graphs) and all(g.get("status") in TERMINAL_GRAPH for g in graphs),
        "graph_outcome_typed": bool(graphs) and all(value in TERMINAL_OUTCOME for value in graph_outcomes),
        "graph_completion_committed": bool(graphs) and completions_valid,
        "all_tasks_terminal": bool(tasks) and all(t.get("status") in TERMINAL_TASK for t in tasks),
        "task_outcomes_complete": bool(graph_tasks) and all(t.get("outcome_ref") in outcomes for t in graph_tasks + final_reports),
        "task_outcomes_delivered": bool(outcomes) and all(ref in acknowledgements for ref in outcomes),
        "final_report_present": bool(final_reports),
        "final_report_scope_bound": bool(final_reports) and all(t.get("final_report_graph_id") in graph_ids for t in final_reports),
        "execution_settled": settled,
        "known_incidents_absent": not any(known.values()),
        "evidence_complete": not reader.issues,
    }
    execution_complete = checks["all_tasks_terminal"] and checks["graph_terminal"] and settled
    return {
        "schema": RESULT_SCHEMA, "run_id": run_id,
        "process_terminal": monitor.get("process_terminal", "unknown"),
        "external_hard_kill": bool(monitor.get("external_hard_kill")), "wall_sec": monitor.get("wall_sec", 0),
        "deadline_reached": bool(monitor.get("deadline_reached")),
        "completion_observed": bool(monitor.get("completion_observed")),
        "settlement_verified": bool(monitor.get("settlement_verified")),
        "graph_lifecycle_terminal": checks["graph_terminal"], "graph_outcomes": graph_outcomes,
        "graph_statuses": [g.get("status") for g in graphs], "task_statuses": [t.get("status") for t in tasks],
        "final_report_statuses": [t.get("status") for t in final_reports], "task_outcomes": outcome_projection,
        "pending_outcome_delivery_count": sum(ref not in acknowledgements for ref in outcomes),
        "metrics": {**dict(usage), "invocation_failures": dict(failures), "completed_invocations": len(ends),
                    "context": {"snapshots": len(contexts), "policies": sorted(policies), "dispositions": dict(dispositions)},
                    "loop": {"records": loop_records, "attempt_count": len(attempts)},
                    "run_usage": {"present": usage_present, "settled": dict(usage), "active_reservations": active_reservations},
                    "tools": {"requests": len(tool_starts), "results": len(tool_results), "shell_outcomes": dict(shell_outcomes)},
                    "graph_revisions": [g.get("revision") for g in graphs]},
        "incomplete_execution": {"invocations": unresolved_invocations, "tools": unresolved_tools,
                                 "reservations": active_reservations},
        "evidence_issues": reader.issues, "known_incidents": known, "retired_tools_called": retired_calls,
        "architecture_checks": checks,
        "architecture_ok": False if any(known.values()) else (all(checks.values()) if execution_complete and not reader.issues else None),
        "execution_complete": execution_complete,
        "model_contract_checks": {"startup_function_probe": bool(startup_probe_passed), "protocol_compatible": not model_incidents},
        "model_contract_incidents": model_incidents,
        "model_contract_compatible": bool(startup_probe_passed) and not model_incidents,
        "infrastructure_conditions": {"provider_quota_exhausted": failures["provider_quota_exhausted"]},
        "infrastructure_ok": not failures["provider_quota_exhausted"],
    }
