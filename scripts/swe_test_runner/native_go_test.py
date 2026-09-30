"""Deterministic Go adapter checks; no provider or upstream repository required."""
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import native_go
import runner
from suite_config import Suite, project_path


def event(action, test=None, **values):
    result = {"Action": action, "Package": "example/module", **values}
    if test is not None:
        result["Test"] = test
    return result


class GoReportTest(unittest.TestCase):
    def test_parent_and_subtest_failures_do_not_double_count_top_level_tests(self):
        events = [event("start"), event("run", "TestParent"), event("run", "TestParent/child"),
                  event("fail", "TestParent/child"), event("fail", "TestParent"), event("fail")]
        report = native_go.parse_report(events, ["example/module::TestParent"], 1)
        self.assertEqual(report["failed"], 1)
        self.assertEqual(len(report["failure_events"]), 2)
        self.assertEqual(report["not_run"], [])

    def test_panicked_test_and_unexecuted_following_tests_are_explicit(self):
        report = native_go.parse_report([event("start"), event("run", "TestFirst"), event("fail")],
                                       ["example/module::TestFirst", "example/module::TestSecond"], 1)
        self.assertEqual(report["errors"], 1)
        self.assertEqual(report["passed"], 0)
        self.assertEqual(len(report["not_run"]), 2)
        self.assertEqual(report["failure_events"][0]["phase"], "aborted")

    def test_missing_settlement_cache_only_and_incomplete_inventory_cannot_pass(self):
        cases = [[], [event("start"), event("run", "TestA")],
                 [event("start"), event("pass")],
                 [event("start"), event("run", "TestA"), event("pass", "TestA"), event("pass")]]
        for events in cases:
            with self.subTest(events=events), self.assertRaises(ValueError):
                native_go.parse_report(events, ["example/module::TestA", "example/module::TestB"], 0)

    def test_duplicate_terminal_and_uncollected_tests_are_rejected(self):
        with self.assertRaises(ValueError):
            native_go.parse_report([event("start"), event("run", "TestA"), event("pass", "TestA"),
                                    event("fail", "TestA"), event("fail")], ["example/module::TestA"], 1)
        with self.assertRaises(ValueError):
            native_go.parse_report([event("start"), event("run", "TestB"), event("pass", "TestB"), event("pass")],
                                   ["example/module::TestA"], 0)

    def test_build_failure_is_not_a_successful_empty_suite(self):
        report = native_go.parse_report([{"Action": "build-fail", "ImportPath": "example/module"}], None, 1)
        self.assertEqual(report["collected"], 0)
        self.assertEqual(report["errors"], 1)
        self.assertFalse(native_go.green(report))

    def test_enumeration_records_exact_package_and_target_names(self):
        events = [event("output", Output="TestA\n"), event("output", Output="TestB\n"),
                  event("output", Output="not a test name\n")]
        self.assertEqual(native_go.enumeration(events, ("TestB",)), ["example/module::TestB"])

    def test_preexisting_failures_cannot_hide_new_failures_or_missing_tests(self):
        clean = {"collected_nodeids": ["a", "b"], "failure_events": [{"nodeid": "a", "phase": "test"}],
                 "not_run": [], "test_outcomes": {"a": "fail", "b": "pass"}}
        preserved = dict(clean, collected_nodeids=["a", "b", "target"])
        self.assertTrue(native_go.preserves_baseline(clean, preserved)[0])
        replaced = dict(preserved, failure_events=[{"nodeid": "b", "phase": "test"}])
        self.assertFalse(native_go.preserves_baseline(clean, replaced)[0])
        self.assertFalse(native_go.preserves_baseline(clean, dict(preserved, not_run=["b"]))[0])
        self.assertFalse(native_go.preserves_baseline(clean, dict(preserved, collected_nodeids=["a"]))[0])
        self.assertFalse(native_go.preserves_baseline(clean, dict(preserved, test_outcomes={"a": "fail", "b": "skip"}))[0])
        self.assertFalse(native_go.preserves_baseline(clean, preserved, ["a", "b", "target", "missing"])[0])

    def test_target_tests_must_all_pass_without_skips(self):
        events = [event("start"), event("run", "TestA"), event("pass", "TestA"), event("run", "TestB"),
                  event("skip", "TestB"), event("pass")]
        report = native_go.parse_report(events, ["example/module::TestA", "example/module::TestB"], 0)
        self.assertTrue(native_go.green(report))
        self.assertFalse(native_go.target_green(report))


class GoSuiteTest(unittest.TestCase):
    def test_ignored_test_output_is_separate_from_ignored_compiler_inputs(self):
        import subprocess
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            subprocess.run(["git", "init", "-q", str(root)], check=True)
            (root / ".gitignore").write_text("test\n*.go\n", encoding="utf-8")
            (root / "context.go").write_text("package example\n", encoding="utf-8")
            original = native_go.input_identity(root)
            (root / "test").write_text("generated output", encoding="utf-8")
            self.assertEqual(original, native_go.input_identity(root))
            (root / "context.go").write_text("package changed\n", encoding="utf-8")
            self.assertNotEqual(original, native_go.input_identity(root))

    def test_same_entrypoint_accepts_suite_before_or_after_command(self):
        for argv in (["--suite", "gin-1", "task", "context-copy-state"],
                     ["task", "context-copy-state", "--suite", "gin-1"]):
            parsed = runner.parser().parse_args(argv)
            self.assertEqual(parsed.suite, "gin-1")
            self.assertEqual(parsed.func, runner.command_task)

    def test_gin_suite_is_pinned_and_declares_target_tests(self):
        directory = Path(__file__).parent / "suites" / "gin-1"
        suite = Suite.load(directory)
        tasks = runner.load_tasks(directory / "tasks.csv", suite.engine)
        self.assertEqual(suite.engine, "go")
        self.assertEqual(suite.module, "github.com/gin-gonic/gin")
        self.assertEqual(suite.toolchain, "go1.25.0")
        self.assertEqual(len(tasks), 1)
        self.assertEqual(len(tasks[0].test_selectors), 3)
        self.assertEqual(tasks[0].test_files, ("context_test.go",))

    def test_unknown_schema_engine_and_path_escape_are_rejected(self):
        original = json.loads((Path(__file__).parent / "suites/gin-1/suite.json").read_text())
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "suite.json"
            for key, value in (("schema", "unknown"), ("test_engine", "unknown"), ("role_prompt_directory", "../outside")):
                path.write_text(json.dumps(dict(original, **{key: value})), encoding="utf-8")
                with self.assertRaises(ValueError):
                    Suite.load(path.parent)
        for value in ("../outside", "/outside", "C:/outside", "a\\b", "a/../b", "./file"):
            with self.assertRaises(ValueError):
                project_path(value)

    def test_identity_covers_root_go_source_tests_assets_and_dependencies(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            for name in ("go.mod", "go.sum", "context.go", "context_test.go", "testdata/data.txt"):
                path = root / name
                path.parent.mkdir(exist_ok=True)
                path.write_text("before", encoding="utf-8")
            baseline = native_go.input_identity(root)
            protected = native_go.protected_manifest(root)
            (root / "context.go").write_text("after", encoding="utf-8")
            self.assertNotEqual(baseline, native_go.input_identity(root))
            self.assertEqual(protected, native_go.protected_manifest(root))
            for name in ("context_test.go", "new_test.go", "go.mod", "testdata/data.txt"):
                with self.subTest(name=name):
                    (root / name).write_text("changed", encoding="utf-8")
                    self.assertNotEqual(protected, native_go.protected_manifest(root))

    def test_formal_go_tests_do_not_receive_provider_credentials_or_host_flags(self):
        class Config:
            suite = Suite("gin-1", "gin", "unused", "go", "github.com/gin-gonic/gin", "go1.25.0")
        with mock.patch.dict(os.environ, {"SWE_API_KEY": "fixture", "GOFLAGS": "-run=Nothing", "GOWORK": "outside"}):
            environment = native_go.go_environment(Config(), tests=True)
        self.assertNotIn("SWE_API_KEY", environment)
        self.assertEqual(environment["GOWORK"], "off")
        self.assertEqual(environment["GOFLAGS"], "")
        self.assertEqual(environment["GOTOOLCHAIN"], "go1.25.0")


if __name__ == "__main__":
    unittest.main()
