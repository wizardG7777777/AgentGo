#!/usr/bin/env python3

import datetime as dt
import json
import os
from pathlib import Path
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
    return payload


class SWETestRunnerContractTest(unittest.TestCase):
    def test_required_environment_reports_every_missing_or_blank_name_without_values(self):
        environment = {
            name: f"value-for-{name.lower()}"
            for name in swe_test_runner.REQUIRED_ENV_VARS
        }
        environment["SWE_API_KEY"] = "secret-that-must-not-be-rendered"
        environment["SWE_MODEL"] = " \t"
        del environment["SWE_BASE_URL"]
        with mock.patch.dict(os.environ, environment, clear=True):
            with self.assertRaises(RuntimeError) as raised:
                swe_test_runner.required_environment_values()
        message = str(raised.exception)
        self.assertIn("SWE_MODEL", message)
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

    def test_config_requires_explicit_environment_and_trims_values(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            environment = {
                "SWE_API_KEY": " secret ",
                "SWE_BASE_URL": " https://provider.invalid/v1 ",
                "SWE_MODEL": " model-name ",
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
            self.assertEqual(config.model, "model-name")
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
            "SWE_MODEL": "model-name",
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
            "SWE_MODEL": "model-name",
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
        self.assertEqual(counter.result(8), {
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

            def fake_run(command, **kwargs):
                self.assertIn("-p", command)
                self.assertIn(swe_test_runner.PYTEST_REPORTER_MODULE, command)
                environment = kwargs["env"]
                self.assertIn(str(Path(swe_test_runner.__file__).resolve().parent), environment["PYTHONPATH"])
                report_path = Path(environment[swe_test_runner.PYTEST_REPORT_ENV])
                self.assertEqual(report_path, root / "judge.pytest.json")
                report_path.write_text(json.dumps(pytest_payload(
                    collected=2, passed=1, failed=1, errors=1, skipped=0,
                    phase_errors={"collection": 0, "setup": 0, "teardown": 1},
                )), encoding="utf-8")
                junit.write_text(
                    '<testsuite tests="2" failures="1" errors="1" skipped="0" />',
                    encoding="utf-8",
                )
                return SimpleNamespace(returncode=1, stdout=b"1 failed, 1 passed, 1 error\n", stderr=b"")

            with mock.patch.object(swe_test_runner, "venv_python", return_value=root / "python"), \
                    mock.patch.object(swe_test_runner, "run_command", side_effect=fake_run):
                result = swe_test_runner.run_pytest(root, junit, log)
            self.assertEqual(result["tests"], 2)
            self.assertEqual(result["passed"], 1)
            self.assertEqual(result["errors"], 1)
            self.assertEqual(result["exit_code"], 1)

    def test_setting_renderer_replaces_all_markers_without_sed(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "setting.swe-flask.yaml").write_text(
                'root: "__PROJECT_ROOT__"\nport: __PORT__\ntoken: "__TOKEN__"\n'
                'agentgo: "__AGENTGO_ROOT__"\nbase: "__BASE_URL__"\nmodel: "__MODEL__"\n'
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
                model='model-"quoted',
                protocol="responses",
            )
            run_dir = root / "run"
            run_dir.mkdir()
            rendered = swe_test_runner.render_setting(config, root / "worktree", run_dir, 8123, "nonce")
            content = rendered.read_text(encoding="utf-8")
            self.assertNotRegex(content, r"__[A-Z0-9_]+__")
            self.assertIn('model: "model-\\"quoted"', content)
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

    def test_batch_exit_code_fails_when_any_gate_is_not_satisfied(self):
        good = {"stale": False, "architecture_ok": True, "task_resolved": True}
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
            swe_test_runner.atomic_json(done / "result.json", {
                "architecture_ok": True, "task_resolved": True,
                "process_terminal": "graph_terminal", "graph_outcomes": ["success"],
                "metrics": {"model_calls": 3},
            })
            swe_test_runner.atomic_json(done / "judge.json", {
                "verdict": "resolved", "patch_lines": 2,
            })
            later = runs / "later"
            later.mkdir()
            swe_test_runner.atomic_json(later / "result.json", {
                "architecture_ok": True, "task_resolved": True,
            })
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
                base_url="https://provider.invalid/v1", model="model", protocol="responses",
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
                swe_test_runner.atomic_json(run_dir / "result.json", {
                    "architecture_ok": True, "task_resolved": True,
                    "process_terminal": "graph_terminal", "graph_outcomes": ["success"],
                    "metrics": {"model_calls": 2},
                })
                swe_test_runner.atomic_json(run_dir / "judge.json", {
                    "verdict": "resolved", "patch_lines": 1,
                })
                return {"architecture_ok": True, "task_resolved": True}

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
                base_url="https://provider.invalid/v1", model="model", protocol="responses",
            )
            tasks = [
                swe_test_runner.TaskSpec("quota", "a" * 40, (), "quota"),
                swe_test_runner.TaskSpec("later", "b" * 40, (), "later"),
            ]

            def fake_execute(_config, task, _timeout):
                self.assertEqual(task.task_id, "quota")
                run_dir = config.run_dir(task.task_id)
                run_dir.mkdir(parents=True, exist_ok=True)
                result = {
                    "architecture_ok": True, "task_resolved": False,
                    "infrastructure_ok": False,
                    "infrastructure_conditions": {"provider_quota_exhausted": 1},
                    "process_terminal": "graph_terminal", "graph_outcomes": ["blocked"],
                    "metrics": {"model_calls": 4},
                }
                swe_test_runner.atomic_json(run_dir / "result.json", result)
                swe_test_runner.atomic_json(run_dir / "judge.json", {
                    "verdict": "resolved", "patch_lines": 2,
                })
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

    def test_run_contract_leaves_external_and_phase_reserves(self):
        now = dt.datetime(2026, 8, 22, 1, 2, 3, tzinfo=dt.timezone.utc)
        contract = swe_test_runner.build_run_contract("automatic-options", 1200, now)
        self.assertEqual(contract["schema"], swe_test_runner.RUN_SCHEMA)
        self.assertEqual(contract["budget_profile"], "swe/v3")
        self.assertEqual(contract["recovery_reserve"], 90 * swe_test_runner.NANOSECOND)
        self.assertEqual(contract["finalization_reserve"], 30 * swe_test_runner.NANOSECOND)
        deadline = dt.datetime.fromisoformat(contract["deadline_at"].replace("Z", "+00:00"))
        self.assertEqual((deadline - now).total_seconds(), 1140)
        with self.assertRaises(ValueError):
            swe_test_runner.build_run_contract("too-short", 239, now)

    def test_run_budget_metrics_keep_execution_and_control_usage_separate(self):
        with tempfile.TemporaryDirectory() as temp:
            state = Path(temp)
            path = state / "run-budgets" / "run-budgets.jsonl"
            path.parent.mkdir(parents=True)
            records = [
                {"kind": "initialize", "run_id": "run-1", "limit": {"model_calls": 1}},
                {"kind": "reserve", "run_id": "run-1", "reservation": {
                    "reservation_id": "execution", "phase": "execution", "max_charge": {"model_calls": 1},
                }},
                {"kind": "settle", "run_id": "run-1", "settlement": {
                    "reservation_id": "execution", "usage": {"model_calls": 1},
                }},
                {"kind": "reserve", "run_id": "run-1", "reservation": {
                    "reservation_id": "recovery", "phase": "recovery", "max_charge": {"model_calls": 1},
                }},
                {"kind": "settle", "run_id": "run-1", "settlement": {
                    "reservation_id": "recovery", "usage": {"model_calls": 1},
                }},
            ]
            path.write_text("\n".join(json.dumps(record) for record in records) + "\n", encoding="utf-8")
            metrics = swe_test_runner.run_budget_metrics(state, "run-1")
            self.assertTrue(metrics["present"])
            self.assertEqual(metrics["settled"]["model_calls"], 2)
            self.assertEqual(metrics["phase_settled"]["execution"]["model_calls"], 1)
            self.assertEqual(metrics["phase_settled"]["recovery"]["model_calls"], 1)
            self.assertEqual(metrics["active_reservations"], 0)

    def test_probe_requires_typed_auto_singleton_call(self):
        self.assertEqual(swe_test_runner.validate_probe_response(probe_response()), (True, "ok"))
        for payload in (
            {"choices": [{"finish_reason": "stop", "message": {"content": "ok"}}]},
            probe_response(name="wrong"),
            probe_response(arguments={"unexpected": "x"}),
            probe_response(finish="stop"),
        ):
            self.assertFalse(swe_test_runner.validate_probe_response(payload)[0])

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

    def test_graph_terminal_task_scope_excludes_control_plane_scheduler(self):
        tasks = [
            {"id": "origin", "graph_id": "", "status": "completed"},
            {"id": "work", "graph_id": "graph-1", "status": "completed"},
            {"id": "acceptance", "graph_id": "graph-1", "status": "completed"},
            {"id": "final-report", "graph_id": "", "status": "processing"},
        ]
        scoped = swe_test_runner.terminal_task_scope(tasks, [{"graph_id": "graph-1"}])
        self.assertEqual([task["id"] for task in scoped], ["work", "acceptance"])
        self.assertEqual(swe_test_runner.terminal_task_scope(tasks, []), tasks)

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
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch.object(swe_test_runner, "http_json", return_value=(200, missing_final_report)):
            result = swe_test_runner.monitor_run("http://local", "token", running_process, "run-1", time.time(), 10,
                                         str(Path(directory) / "snapshot.json"), poll_sec=0,
                                         terminal_grace_sec=0)
        self.assertEqual(result["process_terminal"], "graph_terminal_incomplete_final_report")

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
                            "fulfillment": {"workspace_revision_ref": "workspace:sha256:x",
                                             "check_refs": ["check:sha256:y"]},
                        },
                    },
                },
                {"kind": "delivery_ack", "ack_ref": "outcome:1", "record": {}},
            ]
            journal.write_text("\n".join(json.dumps(entry) for entry in entries) + "\n", encoding="utf-8")
            outcomes, pending = swe_test_runner.safe_outcomes(state, "run-1")
            self.assertEqual(pending, 0)
            self.assertTrue(outcomes[0]["delivery_acked"])
            self.assertTrue(outcomes[0]["fulfillment_present"])
            self.assertEqual(outcomes[0]["fulfillment_check_count"], 1)
            serialized = json.dumps(outcomes)
            self.assertNotIn("must-not-leak", serialized)
            self.assertNotIn("result", serialized)

    def test_loop_intervention_requires_graph_recovery_outcome(self):
        source = {
            "graph_id": "g-1", "node_id": "work", "activation_id": "work@1",
            "reason_code": "loop_intervention_required", "status": "blocked",
        }
        self.assertEqual(
            swe_test_runner.missing_loop_recovery_sources([source], set()),
            ["g-1/work@1/unknown"],
        )
        recovery = {
            "graph_id": "g-1", "node_id": "recovery", "activation_id": "recovery@1",
            "task_id": "recovery-task", "status": "completed",
        }
        source["task_id"] = "work-task"
        self.assertEqual(swe_test_runner.missing_loop_recovery_sources([source, recovery], {"work-task"}), [])
        self.assertEqual(swe_test_runner.missing_loop_recovery_sources([source, recovery], {"other-task"}),
                         ["g-1/work@1/work-task"])
        hard_blocked = {**source, "graph_id": "g-2", "reason_code": "no_progress_budget_exhausted"}
        self.assertEqual(swe_test_runner.missing_loop_recovery_sources([hard_blocked], set()),
                         ["g-2/work@1/work-task"])
        observation_stalled = {**source, "graph_id": "g-3", "reason_code": "observation_state_stalled"}
        self.assertEqual(swe_test_runner.missing_loop_recovery_sources([observation_stalled], set()),
                         ["g-3/work@1/work-task"])
        recovery_intervention = {
            **source, "task_id": "recovery-task", "node_id": "recovery",
            "activation_id": "recovery@2",
        }
        self.assertEqual(
            swe_test_runner.missing_loop_recovery_sources(
                [recovery_intervention], set(), {"recovery-task"},
            ), [],
        )

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
            metrics, _ = swe_test_runner.trace_metrics(root, "run-1", {"scheduler"})
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
            metrics, _ = swe_test_runner.trace_metrics(root, "run-1", {"scheduler"})
            self.assertFalse(metrics["known_incidents"]["scheduler_tool_batch_exceeded"])

    def test_trace_metrics_marks_final_report_result_scope_rejection_as_architecture_incident(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            log = root / ".agentgo" / "sessions" / "s1" / "logs" / "trace.jsonl"
            log.parent.mkdir(parents=True)
            events = [
                {"ts": "1", "kind": "context_manifest_built", "run_id": "run-1",
                 "task_id": "scheduler", "turn_id": "final",
                 "description": json.dumps([{"source_ref": "prompt-phase:scheduler:final-report"}])},
                {"ts": "2", "kind": "tool_result", "run_id": "run-1", "task_id": "scheduler",
                 "turn_id": "final", "tool": "get_task_result",
                 "error": "get_task_result 被拒绝：Graph 任务 work 不属于当前 legacy Scheduler scope"},
            ]
            log.write_text("\n".join(json.dumps(event) for event in events) + "\n", encoding="utf-8")
            metrics, _ = swe_test_runner.trace_metrics(root, "run-1", {"scheduler"})
            self.assertTrue(metrics["known_incidents"]["final_report_result_scope_failure"])

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
            metrics, _ = swe_test_runner.trace_metrics(root, "run-1", {"scheduler"})
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
            metrics, _ = swe_test_runner.trace_metrics(root, "run-1", {"scheduler"})
            self.assertTrue(metrics["known_incidents"]["invocation_output_limit_exceeded"])

    def test_trace_metrics_marks_recovery_delta_rejection_as_architecture_incident(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            log = root / ".agentgo" / "sessions" / "s1" / "logs" / "trace.jsonl"
            log.parent.mkdir(parents=True)
            event = {"ts": "1", "kind": "tool_result", "run_id": "run-1",
                     "task_id": "recovery", "tool": "submit_recovery_decision",
                     "error": "graph: recovery_delta changed_dimensions 含非法值"}
            log.write_text(json.dumps(event) + "\n", encoding="utf-8")
            metrics, _ = swe_test_runner.trace_metrics(root, "run-1", {"scheduler"})
            self.assertTrue(metrics["known_incidents"]["recovery_contract_rejection"])

    def test_trace_metrics_marks_observation_retry_storm_and_reasoning_replay_break(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            log = root / ".agentgo" / "sessions" / "s1" / "logs" / "trace.jsonl"
            log.parent.mkdir(parents=True)
            manifest = json.dumps([{"source_ref": "prompt-phase:agent:observation-checkpoint"}])
            events = []
            for index in range(3):
                turn_id = f"turn-{index}"
                events.extend([
                    {"ts": f"{index}.0", "kind": "context_manifest_built", "run_id": "run-1",
                     "task_id": "worker", "turn_id": turn_id, "description": manifest},
                    {"ts": f"{index}.1", "kind": "llm_call_end", "run_id": "run-1",
                     "task_id": "worker", "turn_id": turn_id, "failure_kind": "action_contract_rejected",
                     "error": "Observation tool call 未授权"},
                ])
            events.append({
                "ts": "4", "kind": "llm_call_end", "run_id": "run-1", "task_id": "worker",
                "turn_id": "turn-4", "failure_kind": "invalid_request",
                "error": "The `reasoning_text` in the thinking mode must be passed back to the API.",
            })
            log.write_text("\n".join(json.dumps(event) for event in events) + "\n", encoding="utf-8")
            metrics, _ = swe_test_runner.trace_metrics(root, "run-1", {"scheduler"})
            self.assertTrue(metrics["known_incidents"]["observation_checkpoint_retry_storm"])
            self.assertTrue(metrics["known_incidents"]["observation_checkpoint_attempt_limit_exceeded"])
            self.assertTrue(metrics["known_incidents"]["reasoning_mode_replay_break"])

    def test_trace_metrics_marks_nonterminal_observation_control_preflight_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            log = root / ".agentgo" / "sessions" / "s1" / "logs" / "trace.jsonl"
            log.parent.mkdir(parents=True)
            events = [
                {"ts": "1", "kind": "observation_checkpoint_failed", "run_id": "run-1",
                 "task_id": "acceptance", "turn_id": "turn-9",
                 "reason": "control_invocation_preflight_failed"},
                {"ts": "2", "kind": "llm_call_end", "run_id": "run-1",
                 "task_id": "acceptance", "turn_id": "turn-11"},
            ]
            log.write_text("\n".join(json.dumps(event) for event in events) + "\n", encoding="utf-8")
            metrics, _ = swe_test_runner.trace_metrics(root, "run-1", {"scheduler"})
            self.assertTrue(metrics["known_incidents"]["control_checkpoint_unavailable"])
            self.assertEqual(
                metrics["observation_checkpoint_failures"],
                {"control_invocation_preflight_failed": 1},
            )

    def test_trace_metrics_does_not_merge_failures_across_observation_cycles(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            log = root / ".agentgo" / "sessions" / "s1" / "logs" / "trace.jsonl"
            log.parent.mkdir(parents=True)
            events = []
            index = 0
            for cycle in range(5):
                for attempt in range(2):
                    turn_id = f"cycle-{cycle}-attempt-{attempt}"
                    events.extend([
                        {"ts": f"{index:04d}.0", "kind": "context_manifest_built", "run_id": "run-1",
                         "task_id": "worker", "turn_id": turn_id,
                         "description": json.dumps([{"source_ref": "prompt-phase:agent:observation-checkpoint"}])},
                        {"ts": f"{index:04d}.1", "kind": "llm_call_end", "run_id": "run-1",
                         "task_id": "worker", "turn_id": turn_id, "failure_kind": "action_contract_rejected",
                         "error": "Observation tool call 未授权"},
                    ])
                    index += 1
                normal_turn = f"cycle-{cycle}-normal"
                events.extend([
                    {"ts": f"{index:04d}.0", "kind": "context_manifest_built", "run_id": "run-1",
                     "task_id": "worker", "turn_id": normal_turn,
                     "description": json.dumps([{"source_ref": "prompt-phase:default"}])},
                    {"ts": f"{index:04d}.1", "kind": "llm_call_end", "run_id": "run-1",
                     "task_id": "worker", "turn_id": normal_turn},
                ])
                index += 1
            log.write_text("\n".join(json.dumps(event) for event in events) + "\n", encoding="utf-8")
            metrics, _ = swe_test_runner.trace_metrics(root, "run-1", {"scheduler"})
            self.assertFalse(metrics["known_incidents"]["observation_checkpoint_retry_storm"])
            self.assertFalse(metrics["known_incidents"]["observation_checkpoint_attempt_limit_exceeded"])

    def test_finalize_keeps_architecture_and_task_verdict_separate(self):
        with tempfile.TemporaryDirectory() as directory:
            result_path = Path(directory) / "result.json"
            judge_path = Path(directory) / "judge.json"
            swe_test_runner.atomic_json(result_path, {"architecture_ok": True, "graph_outcomes": ["success"]})
            swe_test_runner.atomic_json(judge_path, {"verdict": "failed", "patch_lines": 5, "tampered": False})
            result = swe_test_runner.finalize_result(str(result_path), str(judge_path))
            self.assertTrue(result["architecture_ok"])
            self.assertFalse(result["task_resolved"])

    def test_collect_result_marks_recovery_retry_activation_unstartable(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            snapshot_path = root / "snapshot.json"
            monitor_path = root / "monitor.json"
            swe_test_runner.atomic_json(snapshot_path, {
                "tasks": [],
                "graphs": [{
                    "run_id": "run-1", "graph_id": "g", "status": "failed", "outcome": "failed",
                    "nodes": [{
                        "node_id": "work", "activation_id": "work@2", "status": "failed",
                        "reason": "Task x RunContract phase=execution 的剩余时间窗已耗尽",
                    }],
                }],
            })
            swe_test_runner.atomic_json(monitor_path, {
                "process_terminal": "graph_terminal", "graph_lifecycle_terminal": True,
                "run_identity_visible": True,
            })
            result = swe_test_runner.collect_result(
                str(snapshot_path), str(monitor_path), str(root), "run-1", True,
            )
            self.assertTrue(result["known_incidents"]["recovery_retry_activation_unstartable"])
            self.assertFalse(result["architecture_ok"])

    def test_collect_result_separates_provider_quota_from_architecture_incidents(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            snapshot_path = root / "snapshot.json"
            monitor_path = root / "monitor.json"
            swe_test_runner.atomic_json(snapshot_path, {"tasks": [], "graphs": []})
            swe_test_runner.atomic_json(monitor_path, {
                "process_terminal": "process_exited", "graph_lifecycle_terminal": False,
                "run_identity_visible": True,
            })
            log = root / ".agentgo" / "sessions" / "s1" / "logs" / "trace.jsonl"
            log.parent.mkdir(parents=True)
            log.write_text(json.dumps({
                "ts": "1", "kind": "llm_call_end", "run_id": "run-quota",
                "task_id": "acceptance", "failure_kind": "provider_quota_exhausted",
                "http_status": 402,
            }) + "\n", encoding="utf-8")
            result = swe_test_runner.collect_result(
                str(snapshot_path), str(monitor_path), str(root), "run-quota", True,
            )
            self.assertFalse(result["infrastructure_ok"])
            self.assertEqual(result["infrastructure_conditions"]["provider_quota_exhausted"], 1)
            self.assertNotIn("provider_quota_exhausted", result["known_incidents"])

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
