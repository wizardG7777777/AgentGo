"""Real tiny child processes test native ownership; no model or upstream runs."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import threading
import unittest
import contextlib
import io
from types import SimpleNamespace
from unittest import mock

import processes
import storage
import process_identity
from storage_test import small_policy


class ProcessesTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = storage.StorageRoot(self.temporary.name, small_policy())
        self.attempt = storage.Attempt(self.root, self.root.batch({}), "fixture", "task")

    def test_output_and_binary_stdin_are_saved_once(self):
        payload = b"one\n\x00\xfftwo"
        with self.attempt.activate():
            result = processes.run([sys.executable, "-c", "import sys;sys.stdout.buffer.write(sys.stdin.buffer.read());sys.stderr.write('error')"], input_data=payload)
        self.assertEqual(result.stdout, payload)
        self.assertEqual(result.stderr, b"error")
        self.assertEqual(len(list((self.attempt.path / "commands").iterdir())), 1)
        record = storage.read_json(result.command_ref)
        self.assertTrue(record["quiescent"])
        self.assertTrue(record["output_complete"])

    @unittest.skipUnless(os.name == "posix", "POSIX guardian")
    def test_guardian_startup_failure_preserves_original_error_and_can_finalize(self):
        with self.attempt.activate(), mock.patch.object(processes.ManagedProcess, "_start_guardian",
                    side_effect=storage.StorageError("process_isolation_unavailable")):
            with self.assertRaisesRegex(storage.StorageError, "process_isolation_unavailable"):
                processes.run([sys.executable, "-c", "raise AssertionError('barrier released')"])
        process = self.attempt.processes[0]
        process.stop()  # Finalization must not access an uninitialized monitor.
        self.assertTrue(process._finished)
        self.assertIsNotNone(process.popen.poll())
        record = storage.read_json(process.directory / "command.json")
        self.assertTrue(record["quiescent"])
        self.assertEqual(record["error_code"], "process_isolation_unavailable")

    @unittest.skipUnless(Path("/proc/sys/kernel/random/boot_id").is_file(), "Linux kernel identity")
    def test_guardian_accepts_kernel_identity_despite_wall_clock_birth_drift(self):
        import psutil
        target = subprocess.Popen([sys.executable, "-c", "import time;time.sleep(60)"])
        try:
            identity = process_identity.capture(psutil.Process(target.pid))
            identity["birth"] += 5
            guardian = subprocess.Popen([sys.executable, str(Path(processes.__file__).with_name("process_guardian.py")),
                            json.dumps(identity), str(self.attempt.reports / "guardian-test.json")],
                            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            try:
                out, err = guardian.communicate(input=b"stop\n", timeout=8)
                self.assertEqual(guardian.returncode, 0, err.decode(errors="replace"))
                self.assertEqual(out.strip(), b"ready")
                self.assertTrue(storage.read_json(self.attempt.reports / "guardian-test.json")["quiescent"])
            finally:
                if guardian.poll() is None:
                    guardian.kill()
                guardian.wait()
                for pipe in (guardian.stdin, guardian.stdout, guardian.stderr):
                    pipe.close()
        finally:
            if target.poll() is None:
                target.kill()
            target.wait()

    def test_child_uses_owned_temp_without_provider_credentials(self):
        code = "import tempfile,os,json;f=tempfile.NamedTemporaryFile(delete=False);f.write(b'fixture');f.close();print(json.dumps({'path':f.name,'key_present':'SWE_API_KEY' in os.environ}))"
        environment = dict(os.environ, SWE_API_KEY="fixture-value")
        with self.attempt.activate():
            result = processes.run([sys.executable, "-c", code], env=environment)
        value = json.loads(result.stdout)
        self.assertFalse(value["key_present"])
        self.assertTrue(Path(value["path"]).is_relative_to(self.attempt.scratch))

    def test_nonzero_exit_preserves_output_for_judgement(self):
        with self.attempt.activate():
            result = processes.run([sys.executable, "-c", "print('failed test');raise SystemExit(7)"], check=False)
        self.assertEqual(result.returncode, 7)
        self.assertIn(b"failed test", result.stdout)

    def test_child_disappearing_during_identity_read_does_not_abort_command(self):
        original_capture = process_identity.capture
        injected = threading.Event()
        def capture(process):
            if threading.current_thread().name.startswith("swe-command-monitor-") and not injected.is_set():
                injected.set()
                raise ProcessLookupError(3, "fixture child exited during proc read")
            return original_capture(process)
        code = "import subprocess,sys;subprocess.run([sys.executable,'-c','import time;time.sleep(0.6)']);print('completed')"
        with self.attempt.activate(), mock.patch.object(process_identity, "capture", side_effect=capture):
            result = processes.run([sys.executable, "-c", code], timeout=10)
        self.assertTrue(injected.is_set(), "fixture did not reach descendant identity observation")
        self.assertEqual(result.returncode, 0)
        self.assertIn(b"completed", result.stdout)
        record = storage.read_json(result.command_ref)
        self.assertTrue(record["quiescent"])
        self.assertTrue(record["output_complete"])
        self.assertNotIn("monitor_failure", record)

    def test_monitor_permission_error_remains_a_failure_with_diagnostics(self):
        original_capture = process_identity.capture
        injected = threading.Event()
        def capture(process):
            if threading.current_thread().name.startswith("swe-command-monitor-") and not injected.is_set():
                injected.set()
                raise PermissionError(13, "fixture permission denied")
            return original_capture(process)
        code = "import subprocess,sys;subprocess.run([sys.executable,'-c','import time;time.sleep(5)'])"
        with self.attempt.activate(), mock.patch.object(process_identity, "capture", side_effect=capture):
            with self.assertRaisesRegex(storage.StorageError, "process_quiescence_unknown"):
                processes.run([sys.executable, "-c", code], timeout=10)
        self.assertTrue(injected.is_set())
        record = storage.read_json(next((self.attempt.path / "commands").glob("*/command.json")))
        self.assertFalse(record["output_complete"])
        self.assertEqual(record["monitor_failure"], {"observation": "child_identity", "error_type": "PermissionError", "errno": 13})

    def test_reporter_environment_survives_credential_filter(self):
        report = self.attempt.reports / "pytest.json"
        env = dict(os.environ, AGENTGO_NATIVE_PYTEST_REPORT=str(report), AGENTGO_NATIVE_PYTHON_SUBJECT="fixture",
                   AGENTGO_PRIVATE_TOKEN="fixture", SWE_API_KEY="fixture")
        code = "import os,json;print(json.dumps({'report':os.environ.get('AGENTGO_NATIVE_PYTEST_REPORT'),'subject':os.environ.get('AGENTGO_NATIVE_PYTHON_SUBJECT'),'private_present':'AGENTGO_PRIVATE_TOKEN' in os.environ or 'SWE_API_KEY' in os.environ}))"
        with self.attempt.activate():
            result = processes.run([sys.executable, "-c", code], env=env)
        value = json.loads(result.stdout)
        self.assertEqual(value["report"], str(report))
        self.assertEqual(value["subject"], "fixture")
        self.assertFalse(value["private_present"])

    def test_timeout_stops_owned_process_and_records_failure(self):
        with self.attempt.activate(), self.assertRaisesRegex(storage.StorageError, "command_timeout"):
            processes.run([sys.executable, "-c", "import time;time.sleep(60)"], timeout=0.3)
        record = storage.read_json(next((self.attempt.path / "commands").glob("*/command.json")))
        self.assertTrue(record["quiescent"])
        self.assertFalse(record["output_complete"])

    def test_log_flood_is_bounded_and_never_reported_complete(self):
        self.attempt.policy = small_policy(max_command_output_bytes=8192)
        with self.attempt.activate(), self.assertRaisesRegex(storage.StorageError, "output_limit_exceeded"):
            processes.run([sys.executable, "-c", "import sys;sys.stdout.buffer.write(b'x'*200000);sys.stdout.flush()"])
        directory = next((self.attempt.path / "commands").iterdir())
        self.assertLessEqual((directory / "stdout.log").stat().st_size, 8192)
        self.assertTrue(storage.read_json(directory / "command.json")["quiescent"])
        self.assertFalse(storage.read_json(directory / "command.json")["output_complete"])

    def test_parent_exit_does_not_leave_child_holding_output(self):
        code = "import subprocess,sys,time; p=subprocess.Popen([sys.executable,'-c','import time;time.sleep(60)']);print(p.pid,flush=True);time.sleep(0.5)"
        with self.attempt.activate():
            result = processes.run([sys.executable, "-c", code], timeout=10)
        pid = int(result.stdout)
        psutil = processes.process_library()
        try:
            process = psutil.Process(pid)
            alive = process.is_running() and process.status() != psutil.STATUS_ZOMBIE
        except psutil.NoSuchProcess:
            alive = False
        self.assertFalse(alive)
        self.assertTrue(storage.read_json(result.command_ref)["quiescent"])

    def test_parent_force_kill_triggers_external_cleanup(self):
        parent_script = self.attempt.worktree / "parent.py"
        pid_file = self.attempt.worktree / "child.pid"
        child_code = f"import os,time;from pathlib import Path;Path({str(pid_file)!r}).write_text(str(os.getpid()));time.sleep(60)"
        parent_script.write_text(
            "import sys,time\n"
            f"sys.path.insert(0,{str(Path(__file__).parent)!r})\n"
            "import storage,processes\nfrom storage_test import small_policy\n"
            f"root=storage.StorageRoot({str(self.root.path / 'nested')!r},small_policy())\n"
            "a=storage.Attempt(root,root.batch({}),'fixture','child')\n"
            "with a.activate():\n"
            f" p=processes.ManagedProcess(a,[sys.executable,'-c',{child_code!r}],cwd=a.worktree)\n"
            " p.wait(timeout=60)\n", encoding="utf-8")
        parent = subprocess.Popen([sys.executable, str(parent_script)], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        try:
            deadline = time.monotonic() + 10
            while not pid_file.exists() and parent.poll() is None and time.monotonic() < deadline:
                time.sleep(0.05)
            if not pid_file.exists():
                self.fail("child did not start: " + (parent.stderr.read().decode() if parent.poll() is not None else "timeout"))
            pid = int(pid_file.read_text())
            parent.kill()
            parent.wait(timeout=5)
            psutil = processes.process_library()
            deadline = time.monotonic() + 10
            alive = True
            while time.monotonic() < deadline:
                try:
                    p = psutil.Process(pid)
                    alive = p.is_running() and p.status() != psutil.STATUS_ZOMBIE
                except psutil.NoSuchProcess:
                    alive = False
                if not alive:
                    break
                time.sleep(0.05)
            self.assertFalse(alive, "owned child survived supervisor death")
            attempt_dir = next((self.root.path / "nested" / "batches").glob("*/attempts/fixture/child/*"))
            command_file = next((attempt_dir / "commands").glob("*/command.json"))
            deadline = time.monotonic() + 5
            while time.monotonic() < deadline:
                command = storage.read_json(command_file)
                guardian = command.get("guardian_identity")
                if guardian is None or not process_identity.alive(guardian, psutil):
                    break
                time.sleep(0.05)
            import managed_runner
            with contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(managed_runner.maintenance(SimpleNamespace(operation="archive-seal", attempt=str(attempt_dir))), 0)
                self.assertEqual(managed_runner.maintenance(SimpleNamespace(operation="cleanup-plan", attempt=str(attempt_dir), release_retained=True)), 0)
            plan = next((self.root.path / "nested" / "cleanup").glob("*/plan.json"))
            with contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(managed_runner.maintenance(SimpleNamespace(operation="cleanup-apply", plan=str(plan))), 0)
            recovered_state = storage.read_json(attempt_dir / "attempt.json")
            self.assertFalse(Path(recovered_state["worktree"]).exists())
            self.assertFalse(Path(recovered_state["scratch"]).exists())
            self.assertFalse((attempt_dir / "reports" / "result.json").exists(), "recovery invented a subject result")
            self.assertEqual(storage.read_json(attempt_dir / "attempt.json")["status"], "cleaned")
        finally:
            if parent.poll() is None:
                parent.kill()
                parent.wait()
            parent.stderr.close()


if __name__ == "__main__":
    unittest.main()
