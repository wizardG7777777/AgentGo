import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import native_harness
import native_reports as reports
import runner
from suite_config import Suite


class NativeReportTest(unittest.TestCase):
    def test_vitest_new_file_allowance_is_only_given_to_unpatched_baselines(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = SimpleNamespace(suite=SimpleNamespace(options={'driver': 'vitest'}), repo=root / 'upstream')
            task = SimpleNamespace(task_id='task', metadata={'base_sha': 'a' * 40},
                fix_sha='b' * 40, test_files=('added.test.ts',))
            for stage, targeted, expected in (('clean', False, ['added.test.ts']),
                    ('clean-baseline', False, ['added.test.ts']), ('red', True, None),
                    ('green', False, None), ('judge', False, None), ('judge-targeted', True, None)):
                driver = SimpleNamespace(commands=[], build=root / 'build', identity=Mock(return_value={}),
                    command=Mock(return_value=SimpleNamespace(stdout=b'added.test.ts\0source.ts\0')),
                    test=Mock(return_value=(reports.report({'existing': 'pass'}, engine='vitest', exit_code=0), 'log')))
                with self.subTest(stage=stage), patch.object(native_harness, 'identity', return_value={'files': {}, 'digest': 'input'}), \
                        patch.object(native_harness, 'definition_digest', return_value='definition'):
                    native_harness.execute_tests(driver, config, task, root, root / 'reports', stage, targeted)
                self.assertEqual(driver.test.call_args.kwargs,
                    {'baseline_added_files': expected} if expected is not None else {})
                self.assertEqual(driver.command.call_count, int(expected is not None))

    def test_vitest_new_test_files_are_absent_only_before_the_patch(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            existing = root / 'existing.test.ts'
            existing.write_text('existing')
            selectors = ['existing.test.ts', 'added.test.ts']
            selected = reports.vitest_paths(root, selectors, baseline_added_files=['added.test.ts'])
            self.assertEqual(selected, [(existing.resolve(), False)])
            with self.assertRaises(FileNotFoundError):
                reports.vitest_paths(root, selectors)
            with self.assertRaises(FileNotFoundError):
                reports.vitest_paths(root, ['typo.test.ts'], baseline_added_files=['added.test.ts'])
            (root / 'added.test.ts').write_text('new regression')
            self.assertEqual(len(reports.vitest_paths(root, selectors)), 2)

    def test_new_test_scope_has_explicit_zero_baseline_without_claiming_test_passes(self):
        with tempfile.TemporaryDirectory() as directory:
            self.assertEqual(reports.vitest_paths(Path(directory), ['added.test.ts'],
                baseline_added_files=['added.test.ts']), [])
            with self.assertRaises(ValueError):
                reports.vitest_paths(Path(directory), [], baseline_added_files=['added.test.ts'])
        value = reports.report({}, engine='vitest', exit_code=0, baseline_added_files=['added.test.ts'])
        self.assertEqual(value['passed'], 0)
        self.assertEqual(value['collected'], 0)
        self.assertTrue(value['empty_before_test_patch'])
        self.assertFalse(value['tests_executed'])
        with self.assertRaises(ValueError):
            reports.report({}, engine='vitest', exit_code=0)
        with self.assertRaises(ValueError):
            reports.report({'missing': 'run'}, engine='vitest', exit_code=0,
                baseline_added_files=['added.test.ts'])

    def test_new_test_baseline_exception_does_not_allow_framework_or_parent_paths(self):
        with tempfile.TemporaryDirectory() as directory:
            for name in ('.agentgo/added.test.ts', '../added.test.ts'):
                with self.subTest(name=name), self.assertRaises(ValueError):
                    reports.vitest_paths(Path(directory), [name], baseline_added_files=[name])

    def test_typescript_build_cache_is_not_a_configuration_file(self):
        for name in ('temp/tsconfig.tsbuildinfo', 'temp/tsconfig.test.tsbuildinfo'):
            self.assertFalse(native_harness.test_path(name))
        for name in ('tsconfig.json', 'tsconfig.build.json', 'tsconfig.base.jsonc',
                     'tests/tsconfig.tsbuildinfo', 'testdata/build.tsbuildinfo', 'vitest.config.ts'):
            self.assertTrue(native_harness.test_path(name))

    def test_pytest_collection_failure_is_not_an_environment_exit(self):
        for code in (2, 4):
            with self.subTest(exit_code=code):
                value = reports.report({}, engine="pytest", exit_code=code,
                                       errors=[{"nodeid": "tests/test_import.py", "phase": "collection"}])
                self.assertEqual(value["errors"], 1)
                self.assertEqual(value["collected"], 0)
                self.assertEqual(value["exit_code"], code)
        for code, errors in [(2, []), (4, []), (4, [{"nodeid": "missing", "phase": "setup"}]),
                             (3, [{"nodeid": "tests/test_import.py", "phase": "collection"}])]:
            with self.assertRaises(ValueError):
                reports.report({}, engine="pytest", exit_code=code, errors=errors)

    def test_skipping_a_previously_passing_test_cannot_pass(self):
        clean = reports.report({"a": "pass", "b": "skip"}, engine="fixture", exit_code=0)
        current = reports.report({"a": "skip", "b": "pass"}, engine="fixture", exit_code=0)
        okay, comparison = reports.compare(clean, ["a", "b"], current, ["b"])
        self.assertFalse(okay)
        self.assertEqual(comparison["lost_passes"], ["a"])

    def test_new_failures_cannot_replace_baseline_failures(self):
        clean = reports.report({"a": "fail", "b": "pass"}, engine="fixture", exit_code=1)
        current = reports.report({"a": "pass", "b": "fail"}, engine="fixture", exit_code=1)
        self.assertFalse(reports.compare(clean, ["a", "b"], current, ["a"])[0])

    def test_empty_success_missing_results_and_unknown_states_are_rejected(self):
        for states, expected in [({}, None), ({"a": "pass"}, ["a", "missing"]), ({"a": "mystery"}, None)]:
            with self.assertRaises(ValueError):
                reports.report(states, engine="fixture", exit_code=0, expected=expected)

    def test_libtest_requires_named_outcomes_and_matching_terminal_counts(self):
        text = "running 2 tests\ntest a ... ok\ntest b ... ignored\ntest result: ok. 1 passed; 0 failed; 1 ignored; 0 measured; 0 filtered out; finished in 0.01s\n"
        result = reports.libtest(text, ["a", "b"], "unit", 0)
        self.assertEqual(result["passed"], 1)
        self.assertEqual(result["skipped"], 1)
        for invalid in [text.replace("1 passed", "2 passed"), text.split("test result:")[0], text.replace("test b", "test unknown")]:
            with self.assertRaises(ValueError):
                reports.libtest(invalid, ["a", "b"], "unit", 0)

    def test_vitest_rejects_truncated_count_and_foreign_subject_files(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            data = {"numTotalTests": 1, "numPassedTests": 1, "numFailedTests": 0,
                    "testResults": [{"name": str(root / "a.test.ts"), "status": "passed", "assertionResults": [{"fullName": "case", "status": "passed"}]}]}
            path = root / "report.json"
            path.write_text(json.dumps(data))
            self.assertEqual(reports.vitest(path, root, 0)["passed"], 1)
            data["numTotalTests"] = 10
            path.write_text(json.dumps(data))
            with self.assertRaises(ValueError):
                reports.vitest(path, root, 0)
            data["numTotalTests"] = 1
            data["testResults"][0]["name"] = str(root.parent / "other.test.ts")
            path.write_text(json.dumps(data))
            with self.assertRaises(ValueError):
                reports.vitest(path, root, 0)

    def test_ctest_junit_cannot_claim_missing_cases(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "ctest.xml"
            path.write_text('<testsuite tests="2"><testcase name="only-one"/></testsuite>')
            with self.assertRaises(ValueError):
                reports.junit(path, engine="ctest", exit_code=0)

    def test_vitest_rejects_framework_candidate_tests_even_inside_checkout(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            data = {"numTotalTests": 1, "numPassedTests": 1, "numFailedTests": 0,
                    "testResults": [{"name": str(root / ".agentgo/candidates-v1/fixture/tree/packages/lib/a.test.ts"),
                        "status": "passed", "assertionResults": [{"fullName": "case", "status": "passed"}]}]}
            path = root / "report.json"
            path.write_text(json.dumps(data))
            with self.assertRaisesRegex(ValueError, "framework"):
                reports.vitest(path, root, 0)

    def test_vitest_scope_accepts_declared_source_and_rejects_similar_paths(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            folder = root / "packages/lib/tests"
            folder.mkdir(parents=True)
            source = folder / "case.test.ts"
            source.write_text("fixture")
            data = {"numTotalTests": 1, "numPassedTests": 1, "numFailedTests": 0,
                    "testResults": [{"name": str(source), "status": "passed",
                        "assertionResults": [{"fullName": "case", "status": "passed"}]}]}
            path = root / "report.json"
            path.write_text(json.dumps(data))
            selected = reports.vitest_paths(root, ["packages/lib/tests"])
            self.assertEqual(reports.vitest(path, root, 0, selected=selected)["passed"], 1)
            for outside in (root / "packages/lib/tests-other/case.test.ts",
                            root / ".agentgo/candidates-v1/copy/tree/packages/lib/tests/case.test.ts"):
                data["testResults"][0]["name"] = str(outside)
                path.write_text(json.dumps(data))
                with self.assertRaises(ValueError):
                    reports.vitest(path, root, 0, selected=selected)
            data["testResults"][0]["name"] = str(folder / "case.test.ts.backup.test.ts")
            path.write_text(json.dumps(data))
            with self.assertRaisesRegex(ValueError, "declared selection"):
                reports.vitest(path, root, 0, selected=reports.vitest_paths(root, ["packages/lib/tests/case.test.ts"]))

    def test_vitest_scope_rejects_missing_and_framework_selectors(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / ".agentgo/tests").mkdir(parents=True)
            with self.assertRaises(ValueError):
                reports.vitest_paths(root, [])
            with self.assertRaises(ValueError):
                reports.vitest_paths(root, [".agentgo/tests"])
            with self.assertRaises((ValueError, FileNotFoundError)):
                reports.vitest_paths(root, ["missing"])

    def test_vitest_scope_does_not_hide_nested_user_fixtures(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            folder = root / "tests/.agentgo"
            folder.mkdir(parents=True)
            source = folder / "fixture.test.ts"
            source.write_text("user fixture")
            path = root / "report.json"
            path.write_text(json.dumps({"numTotalTests": 1, "numPassedTests": 1, "numFailedTests": 0,
                "testResults": [{"name": str(source), "status": "passed",
                    "assertionResults": [{"fullName": "case", "status": "passed"}]}]}))
            self.assertEqual(reports.vitest(path, root, 0, selected=reports.vitest_paths(root, ["tests"]))["passed"], 1)

    def test_core_catalog_has_twelve_distinct_eight_task_suites(self):
        directory = Path(__file__).parent / "suites"
        group = Suite.load(directory / "core-96")
        self.assertEqual(len(group.options["members"]), 12)
        all_tasks = []
        for member in group.options["members"]:
            suite = Suite.load(directory / member)
            tasks = native_harness.load_native_tasks(directory / member / "tasks.json", runner.TaskSpec)
            self.assertEqual(len(tasks), 8)
            self.assertEqual(suite.engine, "native")
            for task in tasks:
                self.assertTrue((directory / member / "prompts" / (task.task_id + ".md")).is_file())
            all_tasks.extend(task.task_id for task in tasks)
        self.assertEqual(len(set(all_tasks)), 96)


if __name__ == "__main__":
    unittest.main()
