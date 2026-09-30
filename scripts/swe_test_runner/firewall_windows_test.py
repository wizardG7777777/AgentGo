import base64
import json
import os
from pathlib import Path
import sys
import subprocess
import tempfile
import threading
import time
import unittest
from unittest import mock

import firewall_windows as firewall
import storage
from storage_test import small_policy


class FirewallIntegrationTest(unittest.TestCase):
    def test_broker_error_retains_bounded_diagnostics_without_arbitrary_text(self):
        value = firewall.Session.__new__(firewall.Session)
        value.data = {"pipe": "agentgo-swe-" + "a" * 32}
        value._request_lock = threading.Lock()
        process = mock.Mock(returncode=0)
        process.communicate.return_value = (json.dumps({"ok": False, "error": "firewall_operation_failed",
            "diagnostic": {"hresult": -2147024809, "line": 150, "message": "do-not-emit"}}).encode(), b"")
        with mock.patch.object(firewall.subprocess, "CREATE_NO_WINDOW", 0, create=True), \
             mock.patch.object(firewall.subprocess, "Popen", return_value=process):
            with self.assertRaises(storage.StorageError) as caught:
                value.request("acquire")
        self.assertIn("firewall_operation_failed", str(caught.exception))
        self.assertIn("-2147024809", str(caught.exception))
        self.assertNotIn("do-not-emit", str(caught.exception))

    def test_helper_environment_does_not_forward_provider_credentials(self):
        with mock.patch.dict(os.environ, {"SWE_API_KEY": "fixture-secret", "OPENAI_API_KEY": "fixture-secret",
                                         "UNRELATED_TOKEN": "fixture-secret", "SystemRoot": "fixture-windows"}, clear=True):
            self.assertEqual({k.upper(): v for k, v in firewall.helper_environment().items()}, {"SYSTEMROOT": "fixture-windows"})

    def test_authorization_wait_has_a_deadline_before_readiness(self):
        process = mock.Mock()
        process.poll.return_value = None
        path = mock.Mock()
        path.is_file.return_value = False
        with self.assertRaisesRegex(storage.StorageError, "firewall_broker_start_timeout"):
            firewall.wait_ready(process, path, time.monotonic() - 1)
        process.poll.assert_not_called()

    def test_declined_authorization_stops_instead_of_retrying(self):
        process, path = mock.Mock(), mock.Mock()
        process.poll.return_value = 1223
        path.is_file.return_value = False
        with self.assertRaisesRegex(storage.StorageError, "firewall_authorization_cancelled"):
            firewall.wait_ready(process, path, time.monotonic() + 1)
        self.assertEqual(process.poll.call_count, 1)

    def test_launcher_wait_failure_is_distinct_from_timeout(self):
        process, path = mock.Mock(), mock.Mock()
        process.poll.return_value = 1
        path.is_file.return_value = False
        with self.assertRaisesRegex(storage.StorageError, "firewall_bootstrap_failed"):
            firewall.wait_ready(process, path, time.monotonic() + 1)

    def test_ready_helper_does_not_require_launcher_to_exit(self):
        process, path = mock.Mock(), mock.Mock()
        path.is_file.return_value = True
        firewall.wait_ready(process, path, time.monotonic() + 1)
        process.poll.assert_not_called()

    def test_stop_launcher_uses_popen_lifecycle(self):
        process = mock.Mock()
        process.poll.return_value = None
        firewall.stop_launcher(process)
        process.terminate.assert_called_once()
        process.wait.assert_called_once_with(timeout=5)

    def owner(self, directory):
        value = firewall.Session.__new__(firewall.Session)
        value.owns_helper, value._closed, value._close_attempted = True, False, False
        value.data = {"session_id": "fixture"}
        value.path = Path(directory) / "protected" / "session.json"
        value.control_dir = Path(directory) / "control"
        value.control_dir.mkdir()
        storage.atomic_json(value.control_dir / "state.json", {"status": "ready"})
        value.process, value.guard = mock.Mock(returncode=0), mock.Mock()
        return value

    def test_close_exports_receipt_before_ack_and_checks_directory_removed(self):
        with tempfile.TemporaryDirectory() as directory:
            value = self.owner(directory)
            def request(action):
                if action == "shutdown":
                    return {"receipt": {"session_id": "fixture", "complete": True}}
                self.assertTrue((value.control_dir / "receipt.json").is_file())
                return {"ok": True}
            value.request = mock.Mock(side_effect=request)
            value.close()
            self.assertEqual(storage.read_json(value.control_dir / "state.json")["status"], "closed")
            value.guard.close.assert_called_once()
            value.close()
            self.assertEqual(value.request.call_count, 2)

    def test_failed_rule_cleanup_is_retained_and_cancels_helper(self):
        with tempfile.TemporaryDirectory() as directory:
            value = self.owner(directory)
            value.request = mock.Mock(return_value={"receipt": {"session_id": "fixture", "complete": False}})
            with mock.patch.object(firewall, "stop_launcher") as stop:
                with self.assertRaisesRegex(storage.StorageError, "firewall_rules_not_released"):
                    value.close()
            stop.assert_called_once_with(value.process)
            self.assertTrue((value.control_dir / "cancelled").exists())
            self.assertEqual(storage.read_json(value.control_dir / "state.json")["status"], "cleanup_pending")
            self.assertFalse(value._closed)

    def test_attached_child_cannot_close_the_shared_owner(self):
        value = firewall.Session.__new__(firewall.Session)
        value.owns_helper = False
        value.request = mock.Mock()
        value.close()
        value.request.assert_not_called()

    @unittest.skipUnless(os.name == "nt", "Windows kernel mutex")
    def test_second_authorization_owner_is_rejected_before_uac(self):
        import uuid
        guard = firewall.StartupGuard("fixture-" + uuid.uuid4().hex)
        try:
            with self.assertRaisesRegex(storage.StorageError, "firewall_session_busy"):
                firewall.StartupGuard(guard.name.split("AgentGoSWEFirewall-", 1)[1])
        finally:
            guard.close()

    def test_blocking_rpc_is_killed_and_never_accepted_as_permission(self):
        import subprocess
        value = firewall.Session.__new__(firewall.Session)
        value.data = {"pipe": "agentgo-swe-" + "a" * 32}
        value._request_lock = threading.Lock()
        process = mock.Mock()
        process.communicate.side_effect = [subprocess.TimeoutExpired("rpc", 17), (b"", b"")]
        with mock.patch.object(firewall.subprocess, "CREATE_NO_WINDOW", 0, create=True), \
             mock.patch.object(firewall.subprocess, "Popen", return_value=process), \
             mock.patch.object(firewall, "stop_launcher") as stop:
            with self.assertRaisesRegex(storage.StorageError, "firewall_broker_timeout"):
                value.request("acquire", program="fixture.exe")
        stop.assert_called_once_with(process)

    def test_bootstrap_timeout_removes_staging_and_keeps_failure_receipt(self):
        with tempfile.TemporaryDirectory() as directory:
            store = storage.StorageRoot(directory, small_policy())
            control = store.path / "batches" / store.batch({}) / "firewall"
            control.mkdir()
            process = mock.Mock()
            process.pid = 12345
            process.poll.return_value = None
            guard = mock.Mock(name="guard")
            guard.name = "fixture-guard"
            with mock.patch.dict(os.environ, {"ProgramFiles": str(store.path / "programs")}), \
                 mock.patch.object(firewall.subprocess, "CREATE_NO_WINDOW", 0, create=True), \
                 mock.patch.object(firewall.subprocess, "check_output", return_value="123"), \
                 mock.patch.object(firewall.subprocess, "Popen", return_value=process) as launch, \
                 mock.patch.object(firewall, "wait_ready", side_effect=storage.StorageError("firewall_broker_start_timeout")):
                with self.assertRaisesRegex(storage.StorageError, "firewall_broker_start_timeout"):
                    firewall._start_guarded(store, control, [], 1, "S-1-5-123", guard, {})
            self.assertEqual(launch.call_count, 1)
            self.assertFalse((control / "stage").exists())
            self.assertTrue((control / "cancelled").exists())
            self.assertEqual(storage.read_json(control / "state.json")["status"], "startup_failed")

    def test_dialog_guard_records_only_new_visible_windows(self):
        import firewall_dialogs
        watch = firewall_dialogs.DialogWatch.__new__(firewall_dialogs.DialogWatch)
        watch.baseline, watch.detected = {1: "existing"}, {}
        with mock.patch.object(firewall_dialogs, "visible_windows", return_value={1: "existing"}):
            watch.inspect()
        with mock.patch.object(firewall_dialogs, "visible_windows", return_value={1: "existing", 2: "Windows Security"}):
            with self.assertRaisesRegex(storage.StorageError, "firewall_prompt_detected"):
                watch.inspect()
        self.assertEqual(watch.detected, {2: "Windows Security"})

    def test_offline_firewall_command_does_not_load_provider_configuration(self):
        import runner
        from types import SimpleNamespace
        helper = mock.Mock()
        with mock.patch.object(runner, "required_environment_values", side_effect=AssertionError("provider read")), \
             mock.patch.object(firewall, "start", return_value=helper), \
             mock.patch("firewall_smoke.run", return_value={"passed": True}):
            if os.name == "nt":
                self.assertEqual(runner.command_firewall_check(SimpleNamespace(testbed="fixture")), 0)
                helper.close.assert_called_once()
    def test_hook_is_not_injected_without_an_explicit_session(self):
        with tempfile.TemporaryDirectory() as directory:
            root = storage.StorageRoot(directory, small_policy())
            a = storage.Attempt(root, root.batch({}), "fixture", "test")
            env = a.environment({})
            self.assertNotIn(firewall.ENV_SESSION, env)
            self.assertNotIn("-exec", env["GOFLAGS"])

    @unittest.skipUnless(os.name == "nt", "Windows environment branch")
    def test_managed_environment_covers_model_and_judge_commands(self):
        with tempfile.TemporaryDirectory() as directory:
            root = storage.StorageRoot(directory, small_policy())
            root.firewall = mock.Mock(path=Path(directory) / "session.json")
            a = storage.Attempt(root, root.batch({}), "fixture", "test")
            for credentials in (False, True):
                env = a.environment({"GOFLAGS": "-p=99"}, credentials=credentials)
                self.assertEqual(env[firewall.ENV_RUN], a.attempt_id)
                self.assertEqual(env[firewall.ENV_SESSION], str(root.firewall.path))
                self.assertIn("-p=2", env["GOFLAGS"])
                self.assertIn("-exec=", env["GOFLAGS"])
                self.assertIn("network_exec.py", env["CARGO_TARGET_X86_64_PC_WINDOWS_GNU_RUNNER"])

    def test_missing_broker_is_not_silently_treated_as_permission(self):
        with mock.patch.dict(os.environ, {}, clear=True):
            self.assertIsNone(firewall.from_environment())

    def test_go_flag_quoting_preserves_wrapper_arguments(self):
        self.assertEqual(firewall.go_quote("plain"), "plain")
        self.assertEqual(firewall.go_quote("C:/Program Files/tool.exe"), '"C:/Program Files/tool.exe"')
        self.assertEqual(firewall.go_quote('value "nested"'), '\'value "nested"\'')
        with self.assertRaisesRegex(storage.StorageError, "unquotable"):
            firewall.go_quote('both " and \'')


@unittest.skipUnless(os.name == "nt", "real Windows PowerShell and COM without rule installation")
class NativeBrokerFunctionsTest(unittest.TestCase):
    def run_functions(self, names, body):
        source = str(Path(firewall.__file__).with_name("firewall_broker.ps1")).replace("'", "''")
        script = f"""$ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue'
$tokens=$null; $errors=$null
$ast=[Management.Automation.Language.Parser]::ParseFile('{source}',[ref]$tokens,[ref]$errors)
if($errors.Count) {{throw 'parse_failed'}}
foreach($name in @({','.join(repr(name) for name in names)})) {{
 $f=$ast.Find({{param($a) $a -is [Management.Automation.Language.FunctionDefinitionAst] -and $a.Name -eq $name}},$true)
 if(-not $f) {{throw 'function_missing'}}
 Invoke-Expression $f.Extent.Text
}}
{body}
"""
        result = subprocess.run(["powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand",
            base64.b64encode(script.encode("utf-16-le")).decode()], capture_output=True, timeout=20,
            env=firewall.helper_environment(), creationflags=subprocess.CREATE_NO_WINDOW)
        self.assertEqual(result.returncode, 0, result.stderr.decode(errors="replace"))
        return result.stdout.decode("utf-8-sig").strip()

    def test_receipt_can_be_replaced_repeatedly_under_windows_powershell(self):
        with tempfile.TemporaryDirectory(prefix="swe-firewall-unit-") as directory:
            path = str(Path(directory) / "receipt.json").replace("'", "''")
            self.run_functions(["Write-JSON"], f"""
Write-JSON '{path}' @{{sequence=1}}
Write-JSON '{path}' @{{sequence=2}}
Write-JSON '{path}' @{{sequence=3}}
""")
            self.assertEqual(json.loads(Path(path).read_text(encoding="utf-8")), {"sequence": 3})
            self.assertEqual([p.name for p in Path(directory).iterdir()], ["receipt.json"])

    def test_real_com_accepts_exact_narrow_program_rule_without_installing_it(self):
        result = self.run_functions(["New-ProgramRule"], """
$group='AgentGo SWE offline-unit'; $policy=@{session_id='fixture'}
$program=Join-Path $env:SystemRoot 'System32/notepad.exe'
$rule=New-ProgramRule 'uninstalled-fixture' $program
if($rule.ApplicationName -ine $program -or $rule.Direction -ne 1 -or $rule.Action -ne 1 -or -not $rule.Enabled -or $rule.Profiles -ne 7 -or $rule.Protocol -ne 256 -or $rule.RemoteAddresses -ne '127.0.0.1/255.255.255.255') {throw 'rule_mismatch'}
Write-Output 'verified'
""")
        self.assertEqual(result, "verified")


if __name__ == "__main__":
    unittest.main()
