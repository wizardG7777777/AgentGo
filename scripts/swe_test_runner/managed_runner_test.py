"""End-to-end resource wiring with a tiny local Git fixture and real processes."""
import contextlib
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest import mock

import native_drivers
import native_reports
import runner
import storage
from storage_test import small_policy


class FixtureDriver(native_drivers.Driver):
    def prepare(self):
        (self.build / "venv").mkdir()
        (self.build / "venv" / "rebuildable").write_bytes(b"environment")

    def identity(self):
        return {"driver": "fixture", "subject_root": str(self.root)}

    def test(self, targeted, output):
        code = ("from pathlib import Path; import tempfile; "
                "p=Path(tempfile.gettempdir())/'scratch-database';p.write_bytes(b'x'*1024); "
                "ok=Path('src/value.txt').read_text()==Path('tests/expected.txt').read_text(); "
                "print('pass' if ok else 'fail');raise SystemExit(0 if ok else 1)")
        completed = self.command([sys.executable, "-c", code], check=False, label="fixture-test")
        outcome = "pass" if completed.returncode == 0 else "fail"
        return native_reports.report({"fixture::required": outcome}, engine="fixture", exit_code=completed.returncode), outcome


class ManagedRunnerTest(unittest.TestCase):
    def test_rust_target_uses_actual_platform_and_architecture(self):
        self.assertEqual(native_drivers.rust_target("win32", "AMD64"), "x86_64-pc-windows-gnu")
        self.assertEqual(native_drivers.rust_target("darwin", "arm64"), "aarch64-apple-darwin")
        self.assertEqual(native_drivers.rust_target("linux", "aarch64"), "aarch64-unknown-linux-gnu")
        with self.assertRaises(ValueError):
            native_drivers.rust_target("darwin", "unknown")

    def test_offline_report_never_reads_provider_configuration(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "fixture").write_bytes(b"x")
            with mock.patch.dict(os.environ, {}, clear=True), \
                 mock.patch.object(runner, "required_environment_values", side_effect=AssertionError("provider accessed")), \
                 mock.patch.object(sys, "argv", ["runner.py", "storage", "report", "--testbed", str(root)]), \
                 contextlib.redirect_stdout(io.StringIO()) as stream:
                self.assertEqual(runner.main(), 0)
            self.assertFalse(json.loads(stream.getvalue())["managed"])
            self.assertEqual([p.name for p in root.iterdir()], ["fixture"])

    @unittest.skipUnless(shutil.which("git"), "Git is required for a real local checkout")
    def test_public_qualification_archives_and_cleans_unique_attempt(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            repo = root / "upstream"
            repo.mkdir()
            environment = dict(os.environ, GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_NOSYSTEM="1")
            def git(*args):
                return subprocess.run(["git", *args], cwd=repo, env=environment, capture_output=True, check=True).stdout.decode().strip()
            git("init", "-q")
            git("config", "user.name", "SWE Fixture")
            git("config", "user.email", "fixture@example.invalid")
            (repo / "src").mkdir()
            (repo / "tests").mkdir()
            (repo / "src/value.txt").write_bytes(b"old")
            (repo / "tests/expected.txt").write_bytes(b"old")
            git("add", ".")
            git("commit", "-qm", "base")
            base = git("rev-parse", "HEAD")
            (repo / "src/value.txt").write_bytes(b"fixed")
            (repo / "tests/expected.txt").write_bytes(b"fixed")
            git("add", ".")
            git("commit", "-qm", "fix")
            fix = git("rev-parse", "HEAD")
            suite = SimpleNamespace(suite_id="fixture-8", engine="native", project="fixture", repository=str(repo),
                                    options={"driver": "go", "environment": {}}, role_prompt_directory="prompts/swe")
            config = runner.SWETestRunnerConfig(root, root / "unused-binary", root / "testbed", root / "tasks.json",
                                               root, repo, "https://provider.invalid", "fixture", "fixture", "responses", suite)
            config.managed_storage = True
            task = runner.TaskSpec("fixture-task", fix, ("tests/expected.txt",), "fixture", (),
                                   {"base_sha": base, "source_files": ["src/value.txt"]})
            policy = root / "policy.json"
            storage.atomic_json(policy, small_policy().record())
            args = SimpleNamespace(suite="fixture-8", task_id="fixture-task", storage_policy=str(policy))
            with mock.patch.object(runner.SWETestRunnerConfig, "from_env", return_value=config), \
                 mock.patch.object(runner, "load_tasks", return_value=[task]), \
                 mock.patch.object(native_drivers, "make", side_effect=lambda c,t,r,**kw: FixtureDriver(c,t,r)), \
                 contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(runner.command_verify_candidates(args), 0)
            self.assertEqual((repo / "src/value.txt").read_bytes(), b"fixed")
            report = config.run_dir(task.task_id)
            state = storage.read_json(report / "storage.json")
            self.assertEqual(state["status"], "cleaned")
            self.assertFalse(config.worktree(task.task_id).exists())
            self.assertFalse(Path(state["scratch"]).exists())
            self.assertFalse((report.parent / "commands").exists())
            self.assertTrue(Path(state["archive_ref"]).is_file())
            self.assertTrue(storage.read_json(state["cleanup_ref"])["complete"])
            for stage in ("clean", "red", "green-targeted", "green"):
                execution = storage.read_json(report / stage / "execution.json")
                self.assertEqual(len(execution["commands"]), 1)
                self.assertFalse((report / stage / "commands").exists())
            self.assertEqual(storage.read_json(report / "reference.json")["status"], "qualified")


if __name__ == "__main__":
    unittest.main()
