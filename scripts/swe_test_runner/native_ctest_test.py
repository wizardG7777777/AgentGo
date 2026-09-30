import os
import json
import subprocess
from pathlib import Path
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import native_drivers
import native_harness


class CTestSanitizerTest(unittest.TestCase):
    @unittest.skipUnless(os.name == 'nt', 'Windows CTest permission scope')
    def test_selected_child_programs_belong_to_ctest_command_not_whole_attempt(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            driver = self.fixture(root, None)
            driver.prepare_existing()
            driver.task.metadata['ctest_targets'] = ['selected']
            driver.cmake_build.mkdir()
            (driver.cmake_build / 'CMakeCache.txt').write_text('CMAKE_HOME_DIRECTORY:INTERNAL=' + str(root), encoding='utf-8')
            selected, other = root / 'selected.exe', root / 'other.exe'
            selected.touch(); other.touch()
            output = root / 'output'; output.mkdir()
            captured = []
            def command(argv, **kwargs):
                label = kwargs.get('label')
                if label == 'ctest-network-inventory':
                    return subprocess.CompletedProcess(argv, 0, json.dumps({'tests': [
                        {'name': 'selected', 'command': [str(selected)]},
                        {'name': 'other', 'command': [str(other)]}]}).encode(), b'')
                if label == 'ctest':
                    captured.extend(kwargs['firewall_programs'])
                    (output / 'ctest.xml').write_text('<testsuite tests="1"><testcase name="selected"/></testsuite>', encoding='utf-8')
                return subprocess.CompletedProcess(argv, 0, b'', b'')
            attempt = SimpleNamespace(policy=SimpleNamespace(max_build_jobs=2), storage=SimpleNamespace(firewall=object()))
            with patch.object(native_drivers.resource_storage, 'active', return_value=attempt), patch.object(driver, 'command', side_effect=command):
                driver.test(True, output)
            self.assertEqual(captured, [str(selected)])

    def fixture(self, root, sanitizer):
        config = SimpleNamespace(suite=SimpleNamespace(options={"driver": "ctest"}))
        task = SimpleNamespace(metadata={"environment": {"sanitizer": sanitizer}})
        return native_drivers.CTestDriver(config, task, root)

    def test_missing_sanitizer_compiler_stops_instead_of_using_plain_build(self):
        with tempfile.TemporaryDirectory() as directory:
            driver = self.fixture(Path(directory), "address")
            with patch.object(native_drivers.shutil, "which", return_value=None):
                with self.assertRaisesRegex(RuntimeError, "native compiler"):
                    driver.prepare_existing()

    def test_unknown_sanitizer_is_not_silently_ignored(self):
        with tempfile.TemporaryDirectory() as directory:
            driver = self.fixture(Path(directory), "adress")
            with self.assertRaisesRegex(ValueError, "unsupported CTest sanitizer"):
                driver.prepare_existing()

    def test_task_instrumentation_overrides_inherited_disabling_flags(self):
        with tempfile.TemporaryDirectory() as directory:
            driver = self.fixture(Path(directory), "address")
            driver.env.update(CXXFLAGS="-fno-sanitize=all", ASAN_OPTIONS="halt_on_error=0",
                              CL="/RTC1", LINK="/INCREMENTAL")
            with patch.object(native_drivers.shutil, "which", return_value=sys.executable):
                driver.prepare_existing()
            self.assertNotIn("fno-sanitize", driver.env["CXXFLAGS"])
            self.assertEqual(driver.env["ASAN_OPTIONS"], "halt_on_error=1:abort_on_error=0:exitcode=1")
            self.assertEqual(driver.cmake_definitions["CMAKE_CXX_FLAGS"], driver.env["CXXFLAGS"])
            if os.name == "nt":
                self.assertIn("/EHsc", driver.env["CXXFLAGS"])
                self.assertNotIn("CL", driver.env)
                self.assertNotIn("LINK", driver.env)
                self.assertNotIn("/RTC", driver.cmake_definitions["CMAKE_CXX_FLAGS_DEBUG"])

    def test_enabling_instrumentation_invalidates_task_qualification_identity(self):
        config = SimpleNamespace(suite=SimpleNamespace(options={"driver": "ctest"}))
        task = SimpleNamespace(metadata={"environment": {}})
        before = native_harness.definition_digest(config, task)
        task.metadata["environment"]["sanitizer"] = "address"
        self.assertNotEqual(before, native_harness.definition_digest(config, task))


if __name__ == "__main__":
    unittest.main()
