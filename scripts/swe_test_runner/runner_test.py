#!/usr/bin/env python3

import datetime as dt
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import runner as swe_test_runner  # noqa: E402
import agentgo_swe_pytest_reporter as pytest_reporter  # noqa: E402

def probe_response(name=swe_test_runner.PROBE_NAME, arguments=None, finish="tool_calls"):
    if arguments is None:
        arguments = {"nonce": swe_test_runner.PROBE_NONCE}
    return {
        "choices": [{
            "finish_reason": finish,
            "message": {"tool_calls": [{
                "id": "probe-call-1",
                "function": {"name": name, "arguments": json.dumps(arguments)},
            }]},
        }],
    }

def pytest_payload(**overrides):
    payload = {
        "schema": swe_test_runner.PYTEST_REPORT_SCHEMA,
        "count_semantics": swe_test_runner.PYTEST_COUNT_SEMANTICS,
        "collected": 10,
        "passed": 7,
        "failed": 1,
        "errors": 1,
        "skipped": 1,
        "xfailed": 0,
        "xpassed": 0,
        "phase_errors": {"collection": 0, "setup": 0, "teardown": 1},
    }
    payload.update(overrides)
    payload.setdefault("collected_nodeids", [f"test::{i}" for i in range(payload["collected"])])
    payload.setdefault("failure_events", [
        {"nodeid": f"test::{i}", "phase": "call"} for i in range(payload["failed"])
    ] + [
        {"nodeid": f"test::{i}", "phase": phase}
        for phase, count in payload["phase_errors"].items() for i in range(count)
    ])
    return payload

def runtime_result_fixture(**overrides):
    result = {"schema": swe_test_runner.RESULT_SCHEMA, "run_id": "run-fixture", "architecture_ok": True,
              "model_contract_compatible": True, "infrastructure_ok": True, "execution_complete": True,
              "evidence_issues": [], "task_resolved": True, "stale": False}
    result.update(overrides)
    return result


def judge_fixture(**overrides):
    result = {"schema": "agentgo.swe-judge/v2", "run_id": "run-fixture", "verdict": "resolved",
              "patch_lines": 1, "tampered": False, "test_execution_ref": "fixture.execution.json",
              "test_input_digest": "digest-fixture"}
    result.update(overrides)
    return result


class SWETestRunnerContractTest(unittest.TestCase):
    def test_baseline_failure_context_is_bounded_authoritative_and_normalized(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "targeted-baseline.pytest.log"
            path.write_text(
                "\x1b[31mFAILED tests/test_basic.py::test_access\x1b[0m\r\n"
                "AttributeError: RequestContext has no attribute _session\r\n",
                encoding="utf-8",
            )
            rendered = swe_test_runner.render_baseline_failure_context(path)
        self.assertIn('authority="swe-test-runner"', rendered)
        self.assertIn("AttributeError", rendered)
        self.assertIn("_session", rendered)
        self.assertNotIn("\x1b", rendered)
        self.assertLessEqual(len(rendered), 7000)

    def test_console_streams_are_reconfigured_to_utf8(self):
        class ReconfigurableStream:
            def __init__(self):
                self.calls = []

            def reconfigure(self, **kwargs):
                self.calls.append(kwargs)

        stdout = ReconfigurableStream()
        stderr = ReconfigurableStream()
        with mock.patch.object(swe_test_runner.sys, "stdout", stdout), \
                mock.patch.object(swe_test_runner.sys, "stderr", stderr):
            swe_test_runner.configure_console_utf8()
        expected = [{"encoding": "utf-8", "errors": "backslashreplace"}]
        self.assertEqual(stdout.calls, expected)
        self.assertEqual(stderr.calls, expected)

    def test_required_environment_reports_every_missing_or_blank_name_without_values(self):
        environment = {
            name: f"value-for-{name.lower()}"
            for name in swe_test_runner.REQUIRED_ENV_VARS
        }
        environment["SWE_API_KEY"] = "secret-that-must-not-be-rendered"
        environment["SWE_FAST_MODEL"] = " \t"
        environment["SWE_FLAG_SHIP_MODEL"] = "\t "
        del environment["SWE_BASE_URL"]
        with mock.patch.dict(os.environ, environment, clear=True):
            with self.assertRaises(RuntimeError) as raised:
                swe_test_runner.required_environment_values()
        message = str(raised.exception)
        self.assertIn("SWE_FAST_MODEL", message)
        self.assertIn("SWE_FLAG_SHIP_MODEL", message)
        self.assertIn("SWE_BASE_URL", message)
        self.assertNotIn("SWE_API_KEY", message)
        self.assertNotIn("secret-that-must-not-be-rendered", message)

    def test_required_environment_reports_all_names_when_environment_is_empty(self):
        with mock.patch.dict(os.environ, {}, clear=True):
            with self.assertRaises(RuntimeError) as raised:
                swe_test_runner.required_environment_values()
        message = str(raised.exception)
        for name in swe_test_runner.REQUIRED_ENV_VARS:
            self.assertIn(f"- {name}", message)

    def test_obsolete_single_model_environment_does_not_satisfy_split_contract(self):
        environment = {
            "SWE_API_KEY": "secret",
            "SWE_BASE_URL": "https://provider.invalid/v1",
            "SWE_MODEL": "obsolete-model",
            "SWE_BASE_MODEL": "obsolete-base-model",
            "SWE_WORKER_MODEL": "obsolete-worker-model",
        }
        with mock.patch.dict(os.environ, environment, clear=True):
            with self.assertRaises(RuntimeError) as raised:
                swe_test_runner.required_environment_values()
        message = str(raised.exception)
        self.assertIn("SWE_FAST_MODEL", message)
        self.assertIn("SWE_FLAG_SHIP_MODEL", message)
        self.assertNotIn("obsolete-model", message)
        self.assertNotIn("obsolete-base-model", message)
        self.assertNotIn("obsolete-worker-model", message)

    def test_config_requires_explicit_environment_and_trims_values(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            environment = {
                "SWE_API_KEY": " secret ",
                "SWE_BASE_URL": " https://provider.invalid/v1 ",
                "SWE_FAST_MODEL": " fast-model ",
                "SWE_FLAG_SHIP_MODEL": " flag-ship-model ",
                "SWE_PROTOCOL": " responses ",
                "SWE_TESTBED": f" {root / 'testbed'} ",
                "SWE_TASKS_FILE": f" {root / 'tasks.csv'} ",
                "SWE_PROMPT_DIR": f" {root / 'prompts'} ",
                "SWE_FLASK_REPO": f" {root / 'flask'} ",
                "SWE_AGENTGO_ROOT": f" {root / 'agentgo'} ",
                "SWE_AGENTGO_BIN": f" {root / 'agentgo.exe'} ",
            }
            with mock.patch.dict(os.environ, environment, clear=True):
                config = swe_test_runner.SWETestRunnerConfig.from_env()
            self.assertEqual(config.base_url, "https://provider.invalid/v1")
            self.assertEqual(config.fast_model, "fast-model")
            self.assertEqual(config.flag_ship_model, "flag-ship-model")
            self.assertEqual(config.model_capabilities(), {
                "fast": "fast-model",
                "flag_ship": "flag-ship-model",
            })
            self.assertEqual(config.protocol, "responses")
            self.assertEqual(config.testbed, (root / "testbed").resolve())
            self.assertEqual(config.flask_repo, (root / "flask").resolve())
            self.assertEqual(config.agentgo_bin, (root / "agentgo.exe").resolve())

    def test_default_testbed_uses_platform_user_data_locations(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            windows = swe_test_runner.default_swe_testbed(
                environ={"LOCALAPPDATA": str(root)}, platform_name="win32", home=root / "home",
            )
            windows_fallback = swe_test_runner.default_swe_testbed(
                environ={"USERPROFILE": str(root / "profile")},
                platform_name="win32", home=root / "home",
            )
            macos = swe_test_runner.default_swe_testbed(
                environ={}, platform_name="darwin", home=root / "home",
            )
            linux_xdg = swe_test_runner.default_swe_testbed(
                environ={"XDG_DATA_HOME": str(root / "xdg")},
                platform_name="linux", home=root / "home",
            )
            linux_fallback = swe_test_runner.default_swe_testbed(
                environ={}, platform_name="linux", home=root / "home",
            )
        self.assertEqual(windows, (root / "AgentGo" / "swe").resolve())
        self.assertEqual(
            windows_fallback,
            (root / "profile" / "AppData" / "Local" / "AgentGo" / "swe").resolve(),
        )
        self.assertEqual(
            macos,
            (root / "home" / "Library" / "Application Support" / "AgentGo" / "swe").resolve(),
        )
        self.assertEqual(linux_xdg, (root / "xdg" / "agentgo" / "swe").resolve())
        self.assertEqual(
            linux_fallback,
            (root / "home" / ".local" / "share" / "agentgo" / "swe").resolve(),
        )

    def test_config_derives_optional_values_from_repo_and_user_data(self):
        environment = {
            "SWE_API_KEY": "secret",
            "SWE_BASE_URL": "https://provider.invalid/v1",
            "SWE_FAST_MODEL": "fast-model",
            "SWE_FLAG_SHIP_MODEL": "flag-ship-model",
        }
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            if sys.platform == "win32":
                environment["LOCALAPPDATA"] = directory
                expected_testbed = root / "AgentGo" / "swe"
            elif sys.platform == "darwin":
                expected_testbed = root / "Library" / "Application Support" / "AgentGo" / "swe"
            else:
                environment["XDG_DATA_HOME"] = directory
                expected_testbed = root / "agentgo" / "swe"
            with mock.patch.dict(os.environ, environment, clear=True), \
                    mock.patch.object(swe_test_runner.Path, "home", return_value=root):
                config = swe_test_runner.SWETestRunnerConfig.from_env()
        repo_root = Path(swe_test_runner.__file__).resolve().parents[2]
        testbed = expected_testbed.resolve()
        binary_name = "agentgo.exe" if os.name == "nt" else "agentgo"
        self.assertEqual(config.protocol, "responses")
        self.assertEqual(config.agentgo_root, repo_root)
        self.assertEqual(config.agentgo_bin, repo_root / binary_name)
        self.assertEqual(config.testbed, testbed)
        self.assertEqual(config.tasks_file, swe_test_runner.DEFAULT_SUITE_DIR / "tasks.csv")
        self.assertEqual(config.prompt_dir, swe_test_runner.DEFAULT_SUITE_DIR / "prompts")
        self.assertEqual(config.flask_repo, testbed / "upstream" / "flask")

    def test_readonly_removal_retry_is_windows_permission_only(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "readonly-object"
            target.write_bytes(b"git-object")
            target.chmod(stat.S_IREAD)
            removed = []

            def remove(path):
                removed.append(Path(path))
                Path(path).unlink()

            swe_test_runner.retry_windows_readonly_removal(
                remove, str(target), PermissionError("access denied"), platform_name="nt",
            )
            self.assertEqual(removed, [target])
            self.assertFalse(target.exists())

        for platform_name, error in (
                ("posix", PermissionError("access denied")),
                ("nt", OSError("真实 IO 故障"))):
            with self.subTest(platform_name=platform_name, error=type(error).__name__):
                with self.assertRaises(type(error)) as raised:
                    swe_test_runner.retry_windows_readonly_removal(
                        lambda _path: self.fail("不应重试"), "ignored", error,
                        platform_name=platform_name,
                    )
                self.assertIs(raised.exception, error)

    @unittest.skipUnless(os.name == "nt", "仅 Windows 映射 ReadOnly 文件属性")
    def test_safe_remove_worktree_repeatedly_deletes_readonly_git_objects_on_windows(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = SimpleNamespace(testbed=root, flask_repo=root / "upstream")
            target = root / "worktrees" / "task"
            with mock.patch.object(swe_test_runner, "run_command"):
                for attempt in range(2):
                    packed = target / ".git" / "objects" / "pack" / f"pack-{attempt}.pack"
                    packed.parent.mkdir(parents=True)
                    packed.write_bytes(b"packed-object")
                    packed.chmod(stat.S_IREAD)
                    swe_test_runner.safe_remove_worktree(config, target)
                    self.assertFalse(target.exists(), f"第 {attempt + 1} 次清理失败")

    def test_safe_remove_worktree_rejects_outside_testbed(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = SimpleNamespace(testbed=root / "testbed", flask_repo=root / "upstream")
            with self.assertRaisesRegex(ValueError, "拒绝清理非考题 worktree"):
                swe_test_runner.safe_remove_worktree(config, root / "outside")

    def test_pytest_output_has_stage_scope_objective_and_structured_counts(self):
        result = {
            "tests": 133,
            "collected": 133,
            "passed": 132,
            "failed": 1,
            "errors": 0,
            "skipped": 0,
            "xfailed": 0,
            "xpassed": 0,
            "summary_tail": ["FAILED tests/test_basic.py::test_ipv6", "1 failed, 132 passed"],
        }
        with mock.patch("builtins.print") as printer:
            swe_test_runner.print_stage_header(
                "ipv6-server-name", 1, 4, "目标测试红态确认",
                "tests/test_basic.py", "至少出现 1 个 failed/error",
            )
            self.assertTrue(swe_test_runner.print_pytest_stage_result(result, "red"))
        rendered = "\n".join(str(call.args[0]) for call in printer.call_args_list)
        for expected in (
            "[第1/4阶段]", "目标测试红态确认", "测试范围：tests/test_basic.py",
            "判定目标：至少出现 1 个 failed/error", "pytest 原始摘要",
            "符合预期红态",
            "collected=133 passed=132 failed=1 error_events=0 skipped=0 xfailed=0 xpassed=0",
        ):
            self.assertIn(expected, rendered)

    def test_cli_exposes_only_complete_transactions(self):
        root = swe_test_runner.parser()
        subparsers = next(
            action for action in root._actions
            if isinstance(action, swe_test_runner.argparse._SubParsersAction)
        )
        self.assertEqual(
            set(subparsers.choices),
            {"probe", "task", "batch", "verify-candidates"},
        )
        for removed in ("prepare", "run", "judge", "inject", "monitor", "collect", "finalize", "summarize"):
            self.assertNotIn(removed, subparsers.choices)

    def test_tasks_csv_uses_structured_parser_and_rejects_unsafe_ids(self):
        with tempfile.TemporaryDirectory() as directory:
            tasks = Path(directory) / "tasks.csv"
            tasks.write_text(
                "task_id,fix_sha,test_files,title\n"
                'safe-task,abcdef1,"tests/test_a.py tests/test_b.py","title, with comma"\n',
                encoding="utf-8",
            )
            loaded = swe_test_runner.load_tasks(tasks)
            self.assertEqual(loaded[0].task_id, "safe-task")
            self.assertEqual(loaded[0].test_files, ("tests/test_a.py", "tests/test_b.py"))
            self.assertEqual(loaded[0].title, "title, with comma")
            tasks.write_text(
                "task_id,fix_sha,test_files,title\n"
                "../escape,abcdef1,tests/test_a.py,bad\n",
                encoding="utf-8",
            )
            with self.assertRaises(ValueError):
                swe_test_runner.load_tasks(tasks)

    def test_versioned_default_suite_is_complete_and_cross_platform(self):
        environment = {
            "SWE_API_KEY": "secret",
            "SWE_BASE_URL": "https://provider.invalid/v1",
            "SWE_FAST_MODEL": "fast-model",
            "SWE_FLAG_SHIP_MODEL": "flag-ship-model",
        }
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            if sys.platform == "win32":
                environment["LOCALAPPDATA"] = directory
            elif sys.platform != "darwin":
                environment["XDG_DATA_HOME"] = directory
            with mock.patch.dict(os.environ, environment, clear=True), \
                    mock.patch.object(swe_test_runner.Path, "home", return_value=root):
                config = swe_test_runner.SWETestRunnerConfig.from_env()
        suite = Path(swe_test_runner.__file__).resolve().parent / "suites" / "flask-8"
        self.assertEqual(config.tasks_file, (suite / "tasks.csv").resolve())
        self.assertEqual(config.prompt_dir, (suite / "prompts").resolve())
        self.assertEqual(config.flask_repo, (config.testbed / "upstream" / "flask").resolve())

        metadata = json.loads((suite / "suite.json").read_text(encoding="utf-8"))
        tasks = swe_test_runner.load_tasks(config.tasks_file)
        self.assertEqual(metadata["schema"], "agentgo.swe-suite/v1")
        self.assertEqual(metadata["task_count"], len(tasks))
        self.assertEqual(len(tasks), 8)
        for task in tasks:
            self.assertEqual(len(task.fix_sha), 40)
            prompt = config.prompt_dir / f"{task.task_id}.md"
            self.assertTrue(prompt.is_file(), task.task_id)
            content = prompt.read_text(encoding="utf-8")
            self.assertIn("uv run --no-sync python -m pytest -q", content)
            self.assertNotIn(".venv/bin/python", content)

    def test_phase_counter_keeps_call_outcomes_and_phase_errors_separate(self):
        counter = pytest_reporter.PhaseCounter()
        for report in (
            SimpleNamespace(nodeid="pass", when="call", passed=True, failed=False, skipped=False),
            SimpleNamespace(nodeid="fail", when="call", passed=False, failed=True, skipped=False),
            SimpleNamespace(nodeid="pass-teardown", when="call", passed=True, failed=False, skipped=False),
            SimpleNamespace(nodeid="pass-teardown", when="teardown", passed=False, failed=True, skipped=False),
            SimpleNamespace(nodeid="fail-teardown", when="call", passed=False, failed=True, skipped=False),
            SimpleNamespace(nodeid="fail-teardown", when="teardown", passed=False, failed=True, skipped=False),
            SimpleNamespace(nodeid="setup-error", when="setup", passed=False, failed=True, skipped=False),
            SimpleNamespace(nodeid="skip", when="setup", passed=False, failed=False, skipped=True),
            SimpleNamespace(
                nodeid="xfail", when="call", passed=False, failed=False, skipped=True,
                wasxfail="known",
            ),
            SimpleNamespace(
                nodeid="xpass", when="call", passed=True, failed=False, skipped=False,
                wasxfail="unexpected",
            ),
        ):
            counter.record_runtest(report)
        counter.record_collect(SimpleNamespace(nodeid="bad.py", failed=True, skipped=False))
        counter.collected_nodeids = ["pass", "fail", "pass-teardown", "fail-teardown", "setup-error", "skip", "xfail", "xpass"]
        self.assertEqual(counter.result(8), {
            "collected_nodeids": counter.collected_nodeids,
            "failure_events": [
                {"nodeid": "fail", "phase": "call"},
                {"nodeid": "pass-teardown", "phase": "teardown"},
                {"nodeid": "fail-teardown", "phase": "call"},
                {"nodeid": "fail-teardown", "phase": "teardown"},
                {"nodeid": "setup-error", "phase": "setup"},
                {"nodeid": "bad.py", "phase": "collection"},
            ],
            "schema": swe_test_runner.PYTEST_REPORT_SCHEMA,
            "count_semantics": swe_test_runner.PYTEST_COUNT_SEMANTICS,
            "collected": 8,
            "passed": 2,
            "failed": 2,
            "errors": 4,
            "skipped": 1,
            "xfailed": 1,
            "xpassed": 1,
            "phase_errors": {"collection": 1, "setup": 1, "teardown": 2},
        })

    def test_pytest_sidecar_is_authority_when_junit_totals_overlap(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            junit = root / "judge.junit.xml"
            sidecar = root / "judge.pytest.json"
            junit.write_text(
                '<?xml version="1.0"?><testsuites>'
                '<testsuite tests="957" failures="19" errors="481" skipped="0" />'
                '</testsuites>',
                encoding="utf-8",
            )
            sidecar.write_text(json.dumps(pytest_payload(
                collected=495, passed=476, failed=19, errors=481, skipped=0,
                phase_errors={"collection": 0, "setup": 0, "teardown": 481},
            )), encoding="utf-8")
            result = swe_test_runner.load_pytest_report(sidecar)
            swe_test_runner.validate_junit(junit, result)
            self.assertEqual(result["tests"], 495)
            self.assertEqual(result["passed"], 476)
            self.assertEqual(result["errors"], 481)

    def test_pytest_sidecar_missing_malformed_schema_and_counts_fail_closed(self):
        with tempfile.TemporaryDirectory() as directory:
            report = Path(directory) / "pytest.json"
            with self.assertRaises(RuntimeError):
                swe_test_runner.load_pytest_report(report)
            report.write_text("{", encoding="utf-8")
            with self.assertRaises(RuntimeError):
                swe_test_runner.load_pytest_report(report)
            report.write_text(json.dumps(pytest_payload(schema="wrong")), encoding="utf-8")
            with self.assertRaises(RuntimeError):
                swe_test_runner.load_pytest_report(report)
            report.write_text(json.dumps(pytest_payload(
                errors=2, phase_errors={"collection": 0, "setup": 0, "teardown": 1},
            )), encoding="utf-8")
            with self.assertRaises(RuntimeError):
                swe_test_runner.load_pytest_report(report)

    def test_junit_and_sidecar_key_count_conflict_fails_closed(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            junit = root / "pytest.xml"
            sidecar = root / "pytest.json"
            junit.write_text(
                '<testsuite tests="10" failures="2" errors="1" skipped="1" />',
                encoding="utf-8",
            )
            sidecar.write_text(json.dumps(pytest_payload()), encoding="utf-8")
            with self.assertRaises(RuntimeError):
                swe_test_runner.validate_junit(junit, swe_test_runner.load_pytest_report(sidecar))

    def test_run_pytest_loads_phase_reporter_and_preserves_overlap_counts(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            junit = root / "judge.junit.xml"
            log = root / "judge.pytest.log"
            source = root / "src" / "flask" / "__init__.py"
            source.parent.mkdir(parents=True)
            source.write_text("", encoding="utf-8")

            def fake_run(command, **kwargs):
                if command == ["git", "rev-parse", "HEAD"]:
                    return SimpleNamespace(stdout=b"a" * 40)
                self.assertIn("-p", command)
                self.assertIn(swe_test_runner.PYTEST_REPORTER_MODULE, command)
                environment = kwargs["env"]
                self.assertIn(str(Path(swe_test_runner.__file__).resolve().parent), environment["PYTHONPATH"])
                report_path = Path(environment[swe_test_runner.PYTEST_REPORT_ENV])
                self.assertEqual(report_path, root / "judge.pytest.json")
                report_path.write_text(json.dumps(pytest_payload(
                    collected=2, passed=1, failed=1, errors=1, skipped=0,
                    execution_environment={"python": str(root / "python"), "flask_file": str(source),
                                           "packages": [["pytest", "test-version"]], "exit_code": 1},
                    phase_errors={"collection": 0, "setup": 0, "teardown": 1},
                )), encoding="utf-8")
                junit.write_text(
                    '<testsuite tests="2" failures="1" errors="1" skipped="0" />',
                    encoding="utf-8",
                )
                return SimpleNamespace(returncode=1, stdout=b"1 failed, 1 passed, 1 error\n", stderr=b"")

            with mock.patch.object(swe_test_runner, "venv_python", return_value=root / "python"), \
                    mock.patch.object(swe_test_runner, "run_command", side_effect=fake_run):
                result = swe_test_runner.run_pytest(root, junit, log, task_id="fixture")
            self.assertEqual(result["tests"], 2)
            self.assertEqual(result["passed"], 1)
            self.assertEqual(result["errors"], 1)
            self.assertEqual(result["exit_code"], 1)

    def test_setting_renderer_replaces_all_markers_without_sed(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "setting.swe-flask.yaml").write_text(
                'root: "__PROJECT_ROOT__"\nport: __PORT__\ntoken: "__TOKEN__"\n'
                'agentgo: "__AGENTGO_ROOT__"\nbase: "__BASE_URL__"\n'
                'fast_model: "__FAST_MODEL__"\nflag_ship_model: "__FLAG_SHIP_MODEL__"\n'
                'protocol: "__PROTOCOL__"\nkey: ${__KEY_VAR__}\n',
                encoding="utf-8",
            )
            config = swe_test_runner.SWETestRunnerConfig(
                agentgo_root=root,
                agentgo_bin=root / "agentgo",
                testbed=root / "testbed",
                tasks_file=root / "tasks.csv",
                prompt_dir=root / "prompts",
                flask_repo=root / "flask",
                base_url="https://provider.invalid/v1",
                fast_model='fast-"quoted',
                flag_ship_model='flag-ship-"quoted',
                protocol="responses",
            )
            run_dir = root / "run"
            run_dir.mkdir()
            rendered = swe_test_runner.render_setting(config, root / "worktree", run_dir, 8123, "nonce")
            content = rendered.read_text(encoding="utf-8")
            self.assertNotRegex(content, r"__[A-Z0-9_]+__")
            self.assertIn('fast_model: "fast-\\"quoted"', content)
            self.assertIn('flag_ship_model: "flag-ship-\\"quoted"', content)
            self.assertIn("port: 8123", content)
            root_value = str(root).replace("\\", "/")
            worktree_value = str(root / "worktree").replace("\\", "/")
            self.assertIn(f'root: "{worktree_value}"', content)
            self.assertIn(f'agentgo: "{root_value}"', content)
            self.assertNotIn("\\", next(
                line for line in content.splitlines() if line.startswith("root:")
            ))
            self.assertNotIn("\\", next(
                line for line in content.splitlines() if line.startswith("agentgo:")
            ))

    def test_yaml_template_value_normalizes_only_path_values(self):
        self.assertEqual(
            swe_test_runner.yaml_template_value(Path(r"C:\Users\tester\AgentGo")),
            "C:/Users/tester/AgentGo",
        )
        self.assertEqual(
            swe_test_runner.yaml_template_value(r"literal\value"),
            r"literal\\value",
        )

    def test_versioned_setting_assigns_capability_models_by_role(self):
        repo_root = Path(swe_test_runner.__file__).resolve().parents[2]
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            run_dir = root / "run"
            run_dir.mkdir()
            config = swe_test_runner.SWETestRunnerConfig(
                agentgo_root=repo_root, agentgo_bin=repo_root / "agentgo",
                testbed=root / "testbed", tasks_file=root / "tasks.csv",
                prompt_dir=root / "prompts", flask_repo=root / "flask",
                base_url="https://provider.invalid/v1", fast_model="fast-model",
                flag_ship_model="flag-ship-model", protocol="responses",
            )
            setting = swe_test_runner.render_setting(
                config, root / "worktree", run_dir, 8123, "nonce",
            )
            content = setting.read_text(encoding="utf-8")

        self.assertIn('default_model: "fast-model"', content)
        for kind, expected in (
            ("explorer", "flag-ship-model"),
            ("worker", "flag-ship-model"),
            ("verifier", "fast-model"),
        ):
            block = re.search(
                rf"  - kind: {kind}\n(?P<body>.*?)(?=\n  - kind:|\nscheduler:)",
                content,
                re.DOTALL,
            )
            self.assertIsNotNone(block, kind)
            self.assertIn(f'model: "{expected}"', block.group("body"))
            self.assertNotIn("observation_model:", block.group("body"))
        scheduler = content.split("\nscheduler:\n", 1)[1].split("\n\n", 1)[0]
        self.assertIn('model: "fast-model"', scheduler)

    def test_batch_exit_code_fails_when_any_gate_is_not_satisfied(self):
        good = runtime_result_fixture(**{"stale": False, "architecture_ok": True, "task_resolved": True})
        self.assertEqual(swe_test_runner.batch_exit_code([good], 1), 0)
        self.assertEqual(swe_test_runner.batch_exit_code([], 1), swe_test_runner.EXIT_SWE_TEST_RUNNER_FAILURE)
        self.assertEqual(
            swe_test_runner.batch_exit_code([{**good, "architecture_ok": False}], 1),
            swe_test_runner.EXIT_ARCHITECTURE_FAILURE,
        )
        self.assertEqual(
            swe_test_runner.batch_exit_code([{**good, "task_resolved": False}], 1),
            swe_test_runner.EXIT_TASK_FAILURE,
        )
        self.assertEqual(
            swe_test_runner.batch_exit_code([
                {**good, "run_state": "infrastructure_error", "architecture_ok": None},
                {**good, "run_state": "not_run", "architecture_ok": None},
            ], 2),
            swe_test_runner.EXIT_SWE_TEST_RUNNER_FAILURE,
        )
        self.assertEqual(
            swe_test_runner.batch_exit_code([
                {**good, "run_state": "completed_with_infrastructure_error"},
            ], 1),
            swe_test_runner.EXIT_SWE_TEST_RUNNER_FAILURE,
        )

    def test_startup_failure_preserves_provider_quota_reason(self):
        with tempfile.TemporaryDirectory() as directory:
            log = Path(directory) / "agentgo.log"
            log.write_text(
                '[错误] 启动失败: POST "https://provider.invalid/responses": '
                '402 Payment Required {"message":"Insufficient Balance"}\n',
                encoding="utf-8",
            )
            reason, detail = swe_test_runner.startup_failure_from_log(log)
            self.assertEqual(reason, "provider_quota_exhausted")
            self.assertIn("Insufficient Balance", detail)
            process = SimpleNamespace(poll=lambda: 1)
            with self.assertRaises(swe_test_runner.SWETestRunnerInfrastructureError) as raised:
                swe_test_runner.raise_startup_failure(process, log, "healthz 未就绪")
            self.assertEqual(raised.exception.reason_code, "provider_quota_exhausted")
            self.assertEqual(raised.exception.exit_code, 1)

    def test_batch_summary_synthesizes_infra_and_not_run_without_stale_reuse(self):
        with tempfile.TemporaryDirectory() as directory:
            runs = Path(directory)
            tasks = [
                swe_test_runner.TaskSpec("done", "a" * 40, (), "done"),
                swe_test_runner.TaskSpec("infra", "b" * 40, (), "infra"),
                swe_test_runner.TaskSpec("later", "c" * 40, (), "later"),
            ]
            batch_start = swe_test_runner.record_batch_start(runs)
            done = runs / "done"
            done.mkdir()
            swe_test_runner.atomic_json(done / "result.json", runtime_result_fixture(**{
                "architecture_ok": True, "task_resolved": True,
                "process_terminal": "graph_terminal", "graph_outcomes": ["success"],
                "metrics": {"model_calls": 3},
            }))
            swe_test_runner.atomic_json(done / "judge.json", judge_fixture(**{
                "verdict": "resolved", "patch_lines": 2,
            }))
            later = runs / "later"
            later.mkdir()
            swe_test_runner.atomic_json(later / "result.json", runtime_result_fixture(**{
                "architecture_ok": True, "task_resolved": True,
            }))
            swe_test_runner.atomic_json(later / "judge.json", {"verdict": "resolved"})
            old = batch_start - 100
            os.utime(later / "result.json", (old, old))
            os.utime(later / "judge.json", (old, old))
            failure = {
                "schema": "agentgo.swe-infrastructure-error/v1", "task": "infra",
                "reason_code": "provider_quota_exhausted", "stage": "agentgo_startup",
                "message": "quota",
            }
            rows = swe_test_runner.summarize_batch_runs(
                tasks, str(runs), batch_start, failure, "previous_infrastructure_error",
            )
            self.assertEqual([row["run_state"] for row in rows], [
                "completed", "infrastructure_error", "not_run",
            ])
            self.assertEqual(rows[1]["infrastructure_error"]["reason_code"],
                             "provider_quota_exhausted")
            self.assertEqual(rows[2]["verdict"], "not_run")
            self.assertIsNone(rows[2]["task_resolved"])
            self.assertEqual(swe_test_runner.batch_exit_code(rows, 3), swe_test_runner.EXIT_SWE_TEST_RUNNER_FAILURE)

    def test_command_batch_finalizes_current_summary_after_task_exception(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = swe_test_runner.SWETestRunnerConfig(
                agentgo_root=root, agentgo_bin=root / "agentgo",
                testbed=root / "testbed", tasks_file=root / "tasks.csv",
                prompt_dir=root / "prompts", flask_repo=root / "flask",
                base_url="https://provider.invalid/v1", fast_model="fast-model",
                flag_ship_model="flag-ship-model", protocol="responses",
            )
            tasks = [
                swe_test_runner.TaskSpec("done", "a" * 40, (), "done"),
                swe_test_runner.TaskSpec("infra", "b" * 40, (), "infra"),
                swe_test_runner.TaskSpec("later", "c" * 40, (), "later"),
            ]

            def fake_execute(_config, task, _timeout):
                if task.task_id == "infra":
                    raise swe_test_runner.SWETestRunnerInfrastructureError(
                        "provider_quota_exhausted", "agentgo_startup", "余额不足",
                        exit_code=1, log_path=config.run_dir(task.task_id) / "agentgo.log",
                    )
                run_dir = config.run_dir(task.task_id)
                run_dir.mkdir(parents=True, exist_ok=True)
                swe_test_runner.atomic_json(run_dir / "result.json", runtime_result_fixture(**{
                    "architecture_ok": True, "task_resolved": True,
                    "process_terminal": "graph_terminal", "graph_outcomes": ["success"],
                    "metrics": {"model_calls": 2},
                }))
                swe_test_runner.atomic_json(run_dir / "judge.json", judge_fixture(**{
                    "verdict": "resolved", "patch_lines": 1,
                }))
                return runtime_result_fixture(**{"architecture_ok": True, "task_resolved": True})

            with mock.patch.object(swe_test_runner.SWETestRunnerConfig, "from_env", return_value=config), \
                    mock.patch.object(swe_test_runner, "load_tasks", return_value=tasks), \
                    mock.patch.object(swe_test_runner, "preflight_probe"), \
                    mock.patch.object(swe_test_runner, "execute_task", side_effect=fake_execute):
                code = swe_test_runner.command_batch(SimpleNamespace(timeout=1200, probe_timeout=45))
            self.assertEqual(code, swe_test_runner.EXIT_SWE_TEST_RUNNER_FAILURE)
            rows = json.loads((config.testbed / "runs" / "summary.json").read_text(encoding="utf-8"))
            self.assertEqual([row["run_state"] for row in rows], [
                "completed", "infrastructure_error", "not_run",
            ])
            self.assertEqual(rows[1]["infrastructure_error"]["reason_code"],
                             "provider_quota_exhausted")
            self.assertTrue((config.run_dir("infra") / "infrastructure_error.json").is_file())

    def test_command_batch_stops_on_runtime_provider_quota_with_current_result(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = swe_test_runner.SWETestRunnerConfig(
                agentgo_root=root, agentgo_bin=root / "agentgo",
                testbed=root / "testbed", tasks_file=root / "tasks.csv",
                prompt_dir=root / "prompts", flask_repo=root / "flask",
                base_url="https://provider.invalid/v1", fast_model="fast-model",
                flag_ship_model="flag-ship-model", protocol="responses",
            )
            tasks = [
                swe_test_runner.TaskSpec("quota", "a" * 40, (), "quota"),
                swe_test_runner.TaskSpec("later", "b" * 40, (), "later"),
            ]

            def fake_execute(_config, task, _timeout):
                self.assertEqual(task.task_id, "quota")
                run_dir = config.run_dir(task.task_id)
                run_dir.mkdir(parents=True, exist_ok=True)
                result = runtime_result_fixture(**{
                    "architecture_ok": True, "task_resolved": False,
                    "infrastructure_ok": False,
                    "infrastructure_conditions": {"provider_quota_exhausted": 1},
                    "process_terminal": "graph_terminal", "graph_outcomes": ["blocked"],
                    "metrics": {"model_calls": 4},
                })
                swe_test_runner.atomic_json(run_dir / "result.json", result)
                swe_test_runner.atomic_json(run_dir / "judge.json", judge_fixture(**{
                    "verdict": "resolved", "patch_lines": 2,
                }))
                return result

            with mock.patch.object(swe_test_runner.SWETestRunnerConfig, "from_env", return_value=config), \
                    mock.patch.object(swe_test_runner, "load_tasks", return_value=tasks), \
                    mock.patch.object(swe_test_runner, "preflight_probe"), \
                    mock.patch.object(swe_test_runner, "execute_task", side_effect=fake_execute):
                code = swe_test_runner.command_batch(SimpleNamespace(timeout=1200, probe_timeout=45))
            self.assertEqual(code, swe_test_runner.EXIT_SWE_TEST_RUNNER_FAILURE)
            rows = json.loads((config.testbed / "runs" / "summary.json").read_text(encoding="utf-8"))
            self.assertEqual([row["run_state"] for row in rows], [
                "completed_with_infrastructure_error", "not_run",
            ])
            self.assertEqual(rows[0]["infrastructure_error"]["reason_code"],
                             "provider_quota_exhausted")

    def test_command_batch_checkpoints_before_next_task_and_handles_interrupt(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = swe_test_runner.SWETestRunnerConfig(
                agentgo_root=root, agentgo_bin=root / "agentgo",
                testbed=root / "testbed", tasks_file=root / "tasks.csv",
                prompt_dir=root / "prompts", flask_repo=root / "flask",
                base_url="https://provider.invalid/v1", fast_model="fast-model",
                flag_ship_model="flag-ship-model", protocol="responses",
            )
            tasks = [
                swe_test_runner.TaskSpec("done", "a" * 40, (), "done"),
                swe_test_runner.TaskSpec("interrupted", "b" * 40, (), "interrupted"),
                swe_test_runner.TaskSpec("later", "c" * 40, (), "later"),
            ]
            observed_checkpoint = None

            def fake_execute(_config, task, _timeout):
                nonlocal observed_checkpoint
                if task.task_id == "interrupted":
                    observed_checkpoint = json.loads(
                        (config.testbed / "runs" / "summary.json").read_text(encoding="utf-8")
                    )
                    raise KeyboardInterrupt()
                run_dir = config.run_dir(task.task_id)
                run_dir.mkdir(parents=True, exist_ok=True)
                swe_test_runner.atomic_json(run_dir / "result.json", runtime_result_fixture(**{
                    "architecture_ok": True, "task_resolved": True,
                    "process_terminal": "graph_terminal", "graph_outcomes": ["success"],
                    "metrics": {"model_calls": 2},
                }))
                swe_test_runner.atomic_json(run_dir / "judge.json", judge_fixture(**{
                    "verdict": "resolved", "patch_lines": 1,
                }))
                return runtime_result_fixture(**{"architecture_ok": True, "task_resolved": True})

            with mock.patch.object(swe_test_runner.SWETestRunnerConfig, "from_env", return_value=config), \
                    mock.patch.object(swe_test_runner, "load_tasks", return_value=tasks), \
                    mock.patch.object(swe_test_runner, "preflight_probe"), \
                    mock.patch.object(swe_test_runner, "execute_task", side_effect=fake_execute):
                code = swe_test_runner.command_batch(SimpleNamespace(timeout=1200, probe_timeout=45))
            self.assertEqual(code, swe_test_runner.EXIT_SWE_TEST_RUNNER_FAILURE)
            self.assertEqual(observed_checkpoint[0]["run_state"], "completed")
            self.assertEqual(observed_checkpoint[1]["not_run_reason"], "batch_in_progress")
            rows = json.loads((config.testbed / "runs" / "summary.json").read_text(encoding="utf-8"))
            self.assertEqual([row["run_state"] for row in rows], ["completed", "not_run", "not_run"])
            self.assertEqual(rows[1]["not_run_reason"], "batch_interrupted")

    def test_task_execution_lock_rejects_overlapping_cleanup_and_releases(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = swe_test_runner.SWETestRunnerConfig(
                agentgo_root=root, agentgo_bin=root / "agentgo",
                testbed=root / "testbed", tasks_file=root / "tasks.csv",
                prompt_dir=root / "prompts", flask_repo=root / "flask",
                base_url="https://provider.invalid/v1", fast_model="fast-model",
                flag_ship_model="flag-ship-model", protocol="responses",
            )
            worktree = config.worktree("same-task")
            with swe_test_runner.task_execution_lock(config, "same-task"):
                with self.assertRaises(swe_test_runner.SWETestRunnerInfrastructureError) as raised:
                    with swe_test_runner.task_execution_lock(config, "same-task"):
                        pass
                self.assertEqual(raised.exception.reason_code, "task_already_running")
                self.assertFalse(worktree.exists(), "锁冲突不得触碰 disposable worktree")
            with swe_test_runner.task_execution_lock(config, "same-task"):
                pass

    def test_run_contract_contains_identity_without_test_or_stop_policy(self):
        now = dt.datetime(2026, 9, 9, tzinfo=dt.timezone.utc)
        contract = swe_test_runner.build_run_contract("test-task", now)
        self.assertEqual(contract["schema"], "agentgo.run-contract/v3")
        self.assertEqual(set(contract), {"schema", "run_id", "created_at", "budget_profile"})
        self.assertEqual(contract["created_at"], swe_test_runner.format_time(now))
        self.assertNotEqual(contract["run_id"], swe_test_runner.build_run_contract("test-task", now)["run_id"])

    def test_test_paths_are_argv_and_reject_escape(self):
        self.assertEqual(swe_test_runner.test_path_arguments(("tests/test space.py", "tests/it's_ok.py")),
                         ["tests/test space.py", "tests/it's_ok.py"])
        for path in ("../tests/x.py", "tests/../src/x.py", "/tests/x.py", "tests/x\ny.py"):
            with self.assertRaises(ValueError):
                swe_test_runner.test_path_arguments((path,))

    def test_probe_entry_does_not_launch_observation_subprocess(self):
        config = SimpleNamespace()
        with mock.patch.object(swe_test_runner.SWETestRunnerConfig, "from_env", return_value=config), \
                mock.patch.object(swe_test_runner, "preflight_probe") as probe, \
                mock.patch.object(subprocess, "run", side_effect=AssertionError("不应启动旧控制探针")):
            self.assertEqual(swe_test_runner.command_probe(SimpleNamespace(timeout=31)), 0)
            probe.assert_called_once_with(config, 31)

    def test_probe_requires_typed_auto_singleton_call(self):
        self.assertEqual(swe_test_runner.validate_probe_response(probe_response()), (True, "ok"))
        for payload in (
            {"choices": [{"finish_reason": "stop", "message": {"content": "ok"}}]},
            probe_response(name="wrong"),
            probe_response(arguments={"unexpected": "x"}),
            probe_response(finish="stop"),
        ):
            self.assertFalse(swe_test_runner.validate_probe_response(payload)[0])

    def test_preflight_probes_each_distinct_role_model_once(self):
        config = SimpleNamespace(
            base_url="https://provider.invalid/v1",
            protocol="responses",
            probe_models=lambda: [
                ("fast-model", ("fast",)),
                ("flag-ship-model", ("flag_ship",)),
            ],
        )
        with mock.patch.dict(os.environ, {"SWE_API_KEY": "secret"}, clear=True), \
                mock.patch.object(swe_test_runner, "run_provider_probe") as probe:
            swe_test_runner.preflight_probe(config, 37)
        self.assertEqual(
            [(call.args[2], call.args[3], call.args[4]) for call in probe.call_args_list],
            [("fast-model", "responses", 37), ("flag-ship-model", "responses", 37)],
        )

    def test_config_deduplicates_probe_when_capability_models_match(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = swe_test_runner.SWETestRunnerConfig(
                agentgo_root=root, agentgo_bin=root / "agentgo",
                testbed=root / "testbed", tasks_file=root / "tasks.csv",
                prompt_dir=root / "prompts", flask_repo=root / "flask",
                base_url="https://provider.invalid/v1", fast_model="same-model",
                flag_ship_model="same-model", protocol="responses",
            )
        self.assertEqual(config.probe_models(), [(
            "same-model", ("fast", "flag_ship"),
        )])

    def test_provider_probe_rejects_text_and_transport_errors(self):
        text_only = {"choices": [{"finish_reason": "stop", "message": {"content": "pong"}}]}
        with self.assertRaises(RuntimeError):
            swe_test_runner.run_provider_probe(
                "https://provider.invalid/v1", "secret", "model", attempts=1, sleep_sec=0,
                protocol="chat_completions", transport=lambda *_: (200, text_only),
            )
        with self.assertRaises(RuntimeError):
            swe_test_runner.run_provider_probe(
                "https://provider.invalid/v1", "secret", "model", attempts=1, sleep_sec=0,
                protocol="chat_completions",
                transport=lambda *_: (_ for _ in ()).throw(RuntimeError("offline")),
            )

    def test_provider_probe_402_is_typed_and_not_retried(self):
        calls = 0

        def transport(*_args):
            nonlocal calls
            calls += 1
            return 402, {"error": {
                "message": "Insufficient Balance", "code": "invalid_request_error",
            }}

        with self.assertRaises(swe_test_runner.SWETestRunnerInfrastructureError) as raised:
            swe_test_runner.run_provider_probe(
                "https://provider.invalid/v1", "secret", "model",
                attempts=3, sleep_sec=0, transport=transport,
            )
        self.assertEqual(calls, 1)
        self.assertEqual(raised.exception.reason_code, "provider_quota_exhausted")
        self.assertEqual(raised.exception.stage, "provider_preflight")

    def test_provider_probe_accepts_auto_singleton_transport_result(self):
        def transport(_endpoint, _key, body, _timeout):
            self.assertEqual(body["tool_choice"], "auto")
            self.assertEqual(body["reasoning_effort"], "low")
            name = body["tools"][0]["function"]["name"]
            nonce = body["tools"][0]["function"]["parameters"]["properties"]["nonce"]["const"]
            return 200, probe_response(name=name, arguments={"nonce": nonce})

        swe_test_runner.run_provider_probe(
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

        swe_test_runner.run_provider_probe(
            "https://provider.invalid/v1", "secret", "model", protocol="responses",
            attempts=1, sleep_sec=0, transport=transport,
        )

        text_only = {"id": "resp-2", "status": "completed", "output": [{
            "type": "message", "content": [{"type": "output_text", "text": "call tool"}],
        }]}
        with self.assertRaises(RuntimeError):
            swe_test_runner.run_provider_probe(
                "https://provider.invalid/v1", "secret", "model", protocol="responses",
                attempts=1, sleep_sec=0, transport=lambda *_: (200, text_only),
            )

    def test_probe_accepts_provider_auto_singleton_fanout(self):
        chat = probe_response()
        chat["choices"][0]["message"]["tool_calls"].append({
            "id": "probe-call-2",
            "function": {
                "name": swe_test_runner.PROBE_NAME,
                "arguments": json.dumps({"nonce": swe_test_runner.PROBE_NONCE}),
            },
        })
        self.assertEqual(swe_test_runner.validate_probe_response(chat), (True, "ok"))

        responses = {
            "status": "completed",
            "output": [
                {
                    "type": "function_call", "call_id": f"call-{index}",
                    "name": swe_test_runner.PROBE_NAME,
                    "arguments": json.dumps({"nonce": swe_test_runner.PROBE_NONCE}),
                }
                for index in range(2)
            ],
        }
        self.assertEqual(
            swe_test_runner.validate_probe_response(responses, protocol="responses"),
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
            projected = swe_test_runner.project_snapshot(snapshot, "run-1")
            self.assertTrue(projected["graph_terminal"])
            self.assertTrue(projected["tasks_terminal"])
            self.assertEqual(projected["graph_outcomes"], [outcome])

    def test_pending_task_is_not_no_graph_terminal(self):
        projected = swe_test_runner.project_snapshot({
            "tasks": [{"run_id": "run-1", "status": "pending"}],
            "graphs": [],
            "pending_interactions": [],
        }, "run-1")
        self.assertFalse(projected["tasks_terminal"])
        self.assertEqual(projected["active_tasks"], 1)

    def test_monitor_distinguishes_process_exit_and_hard_kill(self):
        exited_process = subprocess.Popen(
            [sys.executable, "-c", "pass"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        exited_process.wait(timeout=10)
        exited = swe_test_runner.monitor_run(
            "http://127.0.0.1:1", "token", exited_process, "run-1",
            time.time(), 10, os.devnull, poll_sec=0, terminal_grace_sec=0,
        )
        self.assertEqual(exited["process_terminal"], "process_exited")

        running_process = subprocess.Popen(
            [sys.executable, "-c", "import time; time.sleep(30)"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            start_new_session=(os.name == "posix"),
        )
        try:
            killed = swe_test_runner.monitor_run(
                "http://127.0.0.1:1", "token", running_process, "run-1",
                time.time() - 2, 1, os.devnull, poll_sec=0, terminal_grace_sec=0,
            )
            self.assertEqual(killed["process_terminal"], "external_hard_kill")
            self.assertTrue(killed["external_hard_kill"])
            self.assertIsNone(running_process.poll(), "监控探测不得终止仍在运行的子进程")
        finally:
            swe_test_runner.terminate_process(running_process)
        self.assertIsNotNone(running_process.poll())

    def test_terminate_process_escalates_and_waits(self):
        process = mock.Mock()
        process.poll.return_value = None
        process.wait.side_effect = [subprocess.TimeoutExpired("child", 2), 0]
        with mock.patch.object(swe_test_runner.os, "killpg", create=True) as killpg:
            swe_test_runner.terminate_process(process)
        self.assertEqual(process.wait.call_count, 2, "强制结束后也必须等待进程回收")
        if os.name == "posix":
            self.assertEqual(killpg.call_count, 2)
        else:
            process.terminate.assert_called_once()
            process.kill.assert_called_once()

    def test_terminate_process_reports_cleanup_failure(self):
        process = mock.Mock()
        process.poll.return_value = None
        process.wait.side_effect = subprocess.TimeoutExpired("child", 2)
        with mock.patch.object(swe_test_runner.os, "killpg", create=True):
            with self.assertRaises(subprocess.TimeoutExpired):
                swe_test_runner.terminate_process(process)

    def test_terminate_process_accepts_concurrent_exit(self):
        process = mock.Mock()
        process.poll.side_effect = [None, 0]
        process.terminate.side_effect = ProcessLookupError()
        with mock.patch.object(swe_test_runner.os, "killpg", create=True,
                               side_effect=ProcessLookupError()):
            swe_test_runner.terminate_process(process)
        process.kill.assert_not_called()

    def test_monitor_graph_and_no_graph_terminals_do_not_use_quiet(self):
        graph_snapshot = {
            "tasks": [
                {"run_id": "run-1", "status": "completed", "graph_id": "graph-1"},
                {"run_id": "run-1", "status": "completed", "final_report_graph_id": "graph-1"},
            ],
            "graphs": [{"run_id": "run-1", "graph_id": "graph-1", "status": "completed", "outcome": "success"}],
            "pending_interactions": [],
        }
        running_process = SimpleNamespace(poll=lambda: None)
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch.object(swe_test_runner, "http_json", return_value=(200, graph_snapshot)):
            result = swe_test_runner.monitor_run("http://local", "token", running_process, "run-1", time.time(), 10,
                                         str(Path(directory) / "snapshot.json"), poll_sec=0,
                                         terminal_grace_sec=0)
        self.assertEqual(result["process_terminal"], "graph_terminal")
        self.assertTrue(result["graph_lifecycle_terminal"])
        self.assertEqual(result["final_report_statuses"], ["completed"])

        missing_final_report = {
            "tasks": [{"run_id": "run-1", "status": "completed", "graph_id": "graph-1"}],
            "graphs": [{"run_id": "run-1", "graph_id": "graph-1", "status": "completed", "outcome": "success"}],
            "pending_interactions": [],
        }
        incomplete_process = SimpleNamespace(poll=mock.Mock(side_effect=[None, 0]))
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch.object(swe_test_runner, "http_json", return_value=(200, missing_final_report)), \
                mock.patch.object(swe_test_runner.time, "sleep", return_value=None):
            result = swe_test_runner.monitor_run("http://local", "token", incomplete_process, "run-1", time.time(), 10,
                                         str(Path(directory) / "snapshot.json"), poll_sec=0,
                                         terminal_grace_sec=0)
        self.assertEqual(result["process_terminal"], "process_exited")

        processing_intervention = {
            "tasks": [
                {"run_id": "run-1", "status": "completed", "graph_id": "graph-1"},
                {"run_id": "run-1", "status": "completed", "final_report_graph_id": "graph-1"},
                {"run_id": "run-1", "status": "processing", "event_type": "__scheduler__"},
            ],
            "graphs": [{"run_id": "run-1", "graph_id": "graph-1", "status": "failed", "outcome": "failed"}],
            "pending_interactions": [],
        }
        intervention_process = SimpleNamespace(poll=mock.Mock(side_effect=[None, 0]))
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch.object(swe_test_runner, "http_json", return_value=(200, processing_intervention)), \
                mock.patch.object(swe_test_runner.time, "sleep", return_value=None):
            result = swe_test_runner.monitor_run("http://local", "token", intervention_process, "run-1", time.time(), 10,
                                         str(Path(directory) / "snapshot.json"), poll_sec=0,
                                         terminal_grace_sec=0)
        self.assertEqual(result["process_terminal"], "process_exited")

        no_graph_snapshot = {
            "tasks": [{"run_id": "run-1", "status": "blocked"}],
            "graphs": [], "pending_interactions": [],
        }
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch.object(swe_test_runner, "http_json", return_value=(200, no_graph_snapshot)):
            result = swe_test_runner.monitor_run("http://local", "token", running_process, "run-1", time.time(), 10,
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
        exiting_process = SimpleNamespace(poll=mock.Mock(side_effect=[None, 0]))
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch.object(swe_test_runner, "http_json", return_value=(200, snapshot)), \
                mock.patch.object(swe_test_runner.time, "sleep", return_value=None):
            result = swe_test_runner.monitor_run("http://local", "token", exiting_process, "run-1", time.time(), 10,
                                         str(Path(directory) / "snapshot.json"), poll_sec=0,
                                         terminal_grace_sec=0)
        self.assertEqual(result["process_terminal"], "process_exited")

    def test_model_contract_gate_has_exit_code_four(self):
        self.assertEqual(swe_test_runner.final_exit_code(runtime_result_fixture(**{
            "model_contract_compatible":False, "architecture_ok":True, "task_resolved":True,
        })), swe_test_runner.EXIT_MODEL_CONTRACT_FAILURE)
        self.assertEqual(swe_test_runner.batch_exit_code([runtime_result_fixture(**{
            "run_state":"completed", "model_contract_compatible":False,
            "architecture_ok":True, "task_resolved":True, "stale":False,
        })], 1), swe_test_runner.EXIT_MODEL_CONTRACT_FAILURE)

    def test_finalize_keeps_architecture_and_task_verdict_separate(self):
        with tempfile.TemporaryDirectory() as directory:
            result_path = Path(directory) / "result.json"
            judge_path = Path(directory) / "judge.json"
            swe_test_runner.atomic_json(result_path, runtime_result_fixture(graph_outcomes=["success"]))
            swe_test_runner.atomic_json(judge_path, judge_fixture(**{"verdict": "failed", "patch_lines": 5, "tampered": False}))
            result = swe_test_runner.finalize_result(str(result_path), str(judge_path))
            self.assertTrue(result["architecture_ok"])
            self.assertFalse(result["task_resolved"])

    def test_test_baseline_manifest_handles_crlf_changes_deletes_and_invalid(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "tests").mkdir()
            target = root / "tests" / "test_a.py"
            target.write_bytes(b"line1\r\nline2\r\n")
            files = ("tests/test_a.py", "tests/test_deleted.py")
            manifest = swe_test_runner.build_test_baseline_manifest(root, files)
            self.assertEqual(swe_test_runner.compare_test_baseline_manifest(root, files, manifest), [])
            target.write_bytes(b"changed\r\n")
            self.assertEqual(swe_test_runner.compare_test_baseline_manifest(root, files, manifest), ["tests/test_a.py"])
            target.unlink()
            (root / "tests" / "test_deleted.py").mkdir()
            self.assertEqual(set(swe_test_runner.compare_test_baseline_manifest(root, files, manifest)),
                             {"tests/test_a.py(被删除)", "tests/test_deleted.py(应已删除)"})
            for bad in ({}, {"schema": "bad", "files": {}},
                        {"schema": "agentgo.swe-test-baseline/v1", "files": {"x": {"exists": True}}},
                        {"schema": "agentgo.swe-test-baseline/v1", "files": dict(manifest["files"], **{"tests/test_a.py": {"exists": True, "sha256": "bad"}})},
                        {"schema": "agentgo.swe-test-baseline/v1", "files": dict(manifest["files"], **{"tests/test_deleted.py": {"exists": False, "sha256": "0"}})}):
                with self.assertRaises(ValueError):
                    swe_test_runner.compare_test_baseline_manifest(root, files, bad)

if __name__ == "__main__":
    unittest.main()
