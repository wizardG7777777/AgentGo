"""Windows-only, least-privilege firewall leases for native SWE executables."""
from __future__ import annotations
import base64
import atexit
import ctypes
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import threading
import time
import uuid

import storage
from storage import StorageError

ENV_SESSION = "SWE_WINDOWS_FIREWALL_SESSION"
ENV_RUN = "SWE_WINDOWS_FIREWALL_RUN"
SCHEMA = "agentgo.swe-firewall-session/v2"


class ProgramLeases:
    """All pre-execution permissions owned by one managed command tree."""
    def __init__(self, session, run_id):
        self.session, self.run_id = session, run_id
        self.entries = []
        self.programs = set()
        self.acquisition_complete = False

    def prepare(self, programs):
        self.acquisition_complete = False
        for program in programs:
            for path in self.session.program_paths(program):
                key = str(path).casefold()
                if key in self.programs:
                    continue
                lease = self.session.acquire(path, self.run_id)
                self.entries.append({'lease_id': lease, 'program': str(path), 'released': False})
                self.programs.add(key)
        self.acquisition_complete = True

    def release(self, *, quiescent):
        if not quiescent:
            raise StorageError('process_quiescence_unknown', 'firewall permissions still held')
        failure = None
        for entry in reversed(self.entries):
            if entry['released']:
                continue
            try:
                self.session.release(entry['lease_id'], self.run_id)
                entry['released'] = True
            except Exception as error:
                # Do not retry an uncertain RPC here. Attempt-level release_run
                # remains the final cleanup fallback after process quiescence.
                entry['release_error'] = getattr(error, 'code', type(error).__name__)
                failure = failure or error
        if failure is not None:
            raise StorageError('firewall_release_failed', type(failure).__name__) from failure

    def record(self):
        return {'scope': 'command_tree', 'leases': [dict(entry) for entry in self.entries],
                'acquisition_complete': self.acquisition_complete,
                'complete': self.acquisition_complete and all(entry['released'] for entry in self.entries)}


def helper_environment():
    allowed = {"PATH", "SYSTEMROOT", "WINDIR", "SYSTEMDRIVE", "TEMP", "TMP", "USERPROFILE",
               "LOCALAPPDATA", "APPDATA", "PROGRAMFILES", "PROGRAMDATA", "COMSPEC", "PATHEXT"}
    return {key: value for key, value in os.environ.items() if key.upper() in allowed}


class StartupGuard:
    """One owner per Windows user, across test roots and concurrent launchers."""
    def __init__(self, sid):
        self.name = "Local\\AgentGoSWEFirewall-" + sid
        self.kernel = ctypes.WinDLL("kernel32", use_last_error=True)
        self.kernel.CreateMutexW.argtypes = [ctypes.c_void_p, ctypes.c_int, ctypes.c_wchar_p]
        self.kernel.CreateMutexW.restype = ctypes.c_void_p
        self.kernel.CloseHandle.argtypes = [ctypes.c_void_p]
        self.handle = self.kernel.CreateMutexW(None, False, self.name)
        error = ctypes.get_last_error()
        if not self.handle:
            raise StorageError("firewall_start_guard_failed")
        if error == 183:
            self.close()
            raise StorageError("firewall_session_busy", "another authorization or broker is active")

    def close(self):
        if self.handle:
            self.kernel.CloseHandle(self.handle)
            self.handle = None


def stop_launcher(process):
    if process is not None and process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)


def wait_ready(process, installed, deadline):
    while time.monotonic() < deadline:
        if installed.is_file():
            return
        code = process.poll()
        if code is not None:
            raise StorageError("firewall_authorization_cancelled" if code == 1223 else "firewall_bootstrap_failed")
        time.sleep(0.1)
    raise StorageError("firewall_broker_start_timeout", "authorization and startup deadline exceeded")


def go_quote(value):
    if not any(c.isspace() or c in "\"'" for c in value):
        return value
    for quote in ('"', "'"):
        if quote not in value:
            return quote + value + quote
    raise StorageError("firewall_hook_path_unquotable")


def hook_command():
    return " ".join(go_quote(value) for value in
        (Path(getattr(sys, "_base_executable", sys.executable)).as_posix(), "-I", "-S", "-B",
         Path(__file__).with_name("network_exec.py").as_posix()))


def go_flags(jobs):
    return f"-p={jobs} " + go_quote("-exec=" + hook_command())


class Session:
    def __init__(self, path):
        if os.name != "nt":
            raise StorageError("windows_firewall_wrong_platform")
        self.path = Path(path).resolve(strict=True)
        self.data = json.loads(self.path.read_text(encoding="utf-8-sig"))
        if self.data.get("schema") != SCHEMA or not self.data.get("ready"):
            raise StorageError("firewall_session_invalid")
        if not isinstance(self.data.get("pipe"), str) or not self.data["pipe"].startswith("agentgo-swe-"):
            raise StorageError("firewall_session_invalid")
        self.process = self.guard = self.control_dir = None
        self.owns_helper = False
        self._closed = False
        self._close_attempted = False
        self._request_lock = threading.Lock()
        self._dialog_watch = None

    def request(self, action, **fields):
        payload = json.dumps({"pipe": self.data["pipe"], "timeout": 15,
                              "payload": dict(action=action, **fields)}, ensure_ascii=True).encode("utf-8")
        if len(payload) > 16384:
            raise StorageError("firewall_request_too_large")
        executable = str(Path(getattr(sys, "_base_executable", sys.executable)).resolve())
        with self._request_lock:
            process = subprocess.Popen([executable, "-I", "-S", "-B", str(Path(__file__).with_name("firewall_rpc.py"))],
                stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                env=helper_environment(), creationflags=subprocess.CREATE_NO_WINDOW)
            try:
                response, _ = process.communicate(payload, timeout=17)
            except subprocess.TimeoutExpired:
                stop_launcher(process)
                process.communicate(timeout=5)
                raise StorageError("firewall_broker_timeout") from None
            except BaseException:
                stop_launcher(process)
                process.communicate(timeout=5)
                raise
        if process.returncode or len(response) > 65536:
            raise StorageError("firewall_broker_unavailable")
        value = json.loads(response)
        if not value.get("ok"):
            code = value.get("error", "unknown")
            if not isinstance(code, str) or not re.fullmatch(r"[a-z_]{1,100}", code):
                code = "invalid_broker_error"
            diagnostic = value.get("diagnostic", {})
            details = {key: diagnostic[key] for key in ("hresult", "line")
                       if isinstance(diagnostic.get(key), int)} if isinstance(diagnostic, dict) else {}
            raise StorageError("firewall_permission_failed", code + (" " + json.dumps(details) if details else ""))
        return value

    def check(self, run_id=""):
        if self._dialog_watch is None:
            from firewall_dialogs import DialogWatch
            self._dialog_watch = DialogWatch()
        self.check_dialogs()
        value = self.request("ping", run_id=run_id)
        if value.get("session_id") != self.data["session_id"]:
            raise StorageError("firewall_session_mismatch")
        if value.get("fault"):
            raise StorageError("firewall_permission_failed", value["fault"])
        if value.get("draining"):
            raise StorageError("firewall_session_draining")

    def check_dialogs(self):
        if self._dialog_watch is not None:
            self._dialog_watch.check()

    def contains(self, program):
        program = Path(program).resolve()
        return (any(str(program).casefold() == str(Path(p)).casefold() for p in self.data["programs"])
                or any(program.is_relative_to(Path(root)) for root in self.data["roots"]))

    def program_paths(self, program):
        program = Path(program).resolve(strict=True)
        if program.suffix.lower() != ".exe" or not self.contains(program):
            return []
        if program.stem.lower() in {"go", "gofmt", "compile", "asm", "link", "cgo", "vet", "gcc", "g++", "cc1", "cc1plus", "cargo", "rustc", "rustup", "cmake", "ninja"}:
            return []
        programs = []
        # Python venv redirectors run the base interpreter, which owns sockets.
        if program.name.lower() == "python.exe":
            cfg = program.parent.parent / "pyvenv.cfg"
            if cfg.is_file():
                home = next((line.partition("=")[2].strip() for line in cfg.read_text().splitlines() if line.startswith("home =")), None)
                base = Path(home) / "python.exe" if home else None
                if base and base.is_file():
                    programs.append(base.resolve(strict=True))
        programs.append(program)
        return programs

    def acquire(self, program, run_id):
        program = Path(program).resolve(strict=True)
        value = self.request("acquire", program=str(program), run_id=run_id, lease_id=uuid.uuid4().hex)
        self.last_acquire_receipt = value
        return value["lease_id"]

    def release(self, lease_id, run_id):
        self.request("release", lease_id=lease_id, run_id=run_id)

    def release_run(self, run_id):
        return self.request("release_run", run_id=run_id)

    def close(self):
        watcher = getattr(self, "_dialog_watch", None)
        if watcher is not None:
            watcher.close()
            if self.control_dir is not None:
                storage.atomic_json(self.control_dir / "dialogs.json", {
                    "monitor": "win32-visible-top-level", "detected": list(watcher.detected.values()),
                    "error_code": getattr(watcher.error, "code", None)})
            self._dialog_watch = None
        if not self.owns_helper or self._closed:
            return
        state_path = self.control_dir / "state.json"
        self._close_attempted = True
        state = storage.read_json(state_path)
        try:
            receipt = self.request("shutdown")["receipt"]
            if receipt.get("session_id") != self.data["session_id"] or not receipt.get("complete"):
                raise StorageError("firewall_rules_not_released")
            storage.atomic_json(self.control_dir / "receipt.json", receipt)
            self.request("ack_shutdown")
            self.process.wait(timeout=15)
            if self.process.returncode or self.path.parent.exists():
                raise StorageError("firewall_helper_cleanup_incomplete")
            state.update(status="closed", rules_released=True, protected_directory_removed=True)
            self._closed = True
        except BaseException as error:
            state.update(status="cleanup_pending", error_code=getattr(error, "code", type(error).__name__))
            (self.control_dir / "cancelled").touch()
            stop_launcher(self.process)
            raise
        finally:
            storage.atomic_json(state_path, state)
            if self._closed and self.guard:
                self.guard.close()

    def _exit_cleanup(self):
        if self._close_attempted:
            return
        try:
            self.close()
        except Exception:
            print("SWE_FIREWALL_CLEANUP_PENDING: inspect the batch firewall/state.json", file=sys.stderr)


def from_environment():
    path = os.environ.get(ENV_SESSION)
    return Session(path) if os.name == "nt" and path else None


def start(testbed, programs=(), *, control_dir=None, timeout=120):
    """One UAC request; the protected broker follows this process's lifetime."""
    if os.name != "nt":
        return None
    if timeout <= 0:
        raise StorageError("firewall_start_timeout_invalid")
    if ctypes.WinDLL("shell32").IsUserAnAdmin():
        raise StorageError("runner_must_not_be_elevated", "only the firewall helper may run as administrator")
    store = storage.StorageRoot(testbed)
    if control_dir is None:
        control_dir = store.path / "batches" / store.batch({"mode": "firewall-owner"}) / "firewall"
    control_dir = storage.contained(store.path, Path(control_dir))
    control_dir.mkdir(parents=True, exist_ok=False)
    sid = subprocess.check_output(["whoami", "/user", "/fo", "csv", "/nh"], text=True,
                                  env=helper_environment(), timeout=10).strip().split(',')[-1].strip('"')
    if not re.fullmatch(r"S-[0-9-]+", sid):
        raise StorageError("firewall_user_identity_invalid")
    try:
        guard = StartupGuard(sid)
    except BaseException as error:
        storage.atomic_json(control_dir / "state.json", {"root_id": store.root_id, "status": "startup_failed",
            "error_code": getattr(error, "code", type(error).__name__)})
        raise
    context = {}
    try:
        return _start_guarded(store, control_dir, programs, timeout, sid, guard, context)
    except BaseException as error:
        (control_dir / "cancelled").touch()
        cleanup_error = None
        if context.get("session") is not None:
            try:
                context["session"].close()
            except Exception as failure:
                cleanup_error = getattr(failure, "code", type(failure).__name__)
        stop_launcher(context.get("process"))
        if context.get("stage_identity"):
            try:
                storage.remove_owned(store.path, context["stage"], context["stage_identity"])
            except Exception as failure:
                cleanup_error = getattr(failure, "code", type(failure).__name__)
        guard.close()
        path = control_dir / "state.json"
        value = storage.read_json(path) if path.is_file() else {"root_id": store.root_id}
        value.update(status="startup_failed", error_code=getattr(error, "code", type(error).__name__))
        if cleanup_error:
            value["cleanup_error"] = cleanup_error
        storage.atomic_json(path, value)
        raise


def _start_guarded(store, control_dir, programs, timeout, sid, guard, context):
    testbed = store.path
    owner_birth = subprocess.check_output(["powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
        f"[Diagnostics.Process]::GetProcessById({os.getpid()}).StartTime.ToUniversalTime().ToFileTimeUtc()"],
        text=True, env=helper_environment(), timeout=10).strip()
    session_id = uuid.uuid4().hex
    stage = control_dir / "stage"
    stage.mkdir(parents=True)
    stage_identity = storage.file_identity(stage)
    context.update(stage=stage, stage_identity=stage_identity)
    broker = Path(__file__).with_name("firewall_broker.ps1").read_bytes()
    policy = json.dumps({"schema": SCHEMA, "session_id": session_id, "pipe": "agentgo-swe-" + session_id,
        "user_sid": sid, "owner_pid": os.getpid(), "owner_birth": owner_birth,
        "guard_name": guard.name, "startup_expires": time.time() + timeout, "cancel_path": str(control_dir / "cancelled"),
        "roots": [str(Path(testbed).resolve())],
        "programs": sorted({str(Path(p).resolve()) for p in programs if p and Path(p).is_file()})}).encode()
    (stage / "broker.ps1").write_bytes(broker)
    (stage / "policy.json").write_bytes(policy)
    def literal(value):
        return "'" + str(value).replace("'", "''") + "'"
    # The elevated bootstrap checks immutable digests, then writes those exact
    # bytes under protected ACLs. It never executes the writable staged script.
    script = "$ErrorActionPreference='Stop'; "
    script += "$p=[IO.File]::ReadAllText(" + literal(stage / "policy.json") + ")|ConvertFrom-Json;"
    script += "if((Test-Path -LiteralPath $p.cancel_path)-or([DateTimeOffset]::UtcNow.ToUnixTimeSeconds()-gt $p.startup_expires)){exit 124};"
    script += "$o=[Diagnostics.Process]::GetProcessById([int]$p.owner_pid);if($o.StartTime.ToUniversalTime().ToFileTimeUtc().ToString()-ne$p.owner_birth){exit 124};"
    script += "$base=Join-Path ([Environment]::GetFolderPath('ProgramFiles')) 'AgentGoSWEFirewall'; "
    script += "$dest=Join-Path $base " + literal(session_id) + "; "
    script += "if(Test-Path -LiteralPath $dest){throw 'installation_exists'}; "
    script += "$admin=[Security.Principal.SecurityIdentifier]::new('S-1-5-32-544'); $system=[Security.Principal.SecurityIdentifier]::new('S-1-5-18'); $user=[Security.Principal.SecurityIdentifier]::new(" + literal(sid) + "); "
    script += "foreach($d in @($base,$dest)){if(Test-Path -LiteralPath $d){if(([IO.File]::GetAttributes($d)-band[IO.FileAttributes]::ReparsePoint)-ne 0){throw 'reparse_install'}};[IO.Directory]::CreateDirectory($d)|Out-Null;$acl=[Security.AccessControl.DirectorySecurity]::new();$acl.SetAccessRuleProtection($true,$false);$acl.SetOwner($admin);foreach($s in @($admin,$system)){$acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new($s,'FullControl','ContainerInherit,ObjectInherit','None','Allow'))};$reader=if($d -eq $base){[Security.Principal.SecurityIdentifier]::new('S-1-5-32-545')}else{$user};$acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new($reader,'ReadAndExecute','ContainerInherit,ObjectInherit','None','Allow'));[IO.Directory]::SetAccessControl($d,$acl)}; "
    for name, data in (("broker.ps1", broker), ("policy.json", policy)):
        script += "$bytes=[IO.File]::ReadAllBytes(" + literal(stage / name) + ");$hash=[BitConverter]::ToString([Security.Cryptography.SHA256]::Create().ComputeHash($bytes)).Replace('-','').ToLowerInvariant();if($hash -ne " + literal(hashlib.sha256(data).hexdigest()) + "){throw 'staging_changed'};[IO.File]::WriteAllBytes((Join-Path $dest " + literal(name) + "),$bytes); "
    script += "& (Join-Path $dest 'broker.ps1') -PolicyFile (Join-Path $dest 'policy.json')"
    encoded = base64.b64encode(script.encode("utf-16-le")).decode()
    launch = "try {$p=Start-Process -PassThru -ErrorAction Stop -FilePath ([IO.Path]::Combine($env:SystemRoot,'System32','WindowsPowerShell','v1.0','powershell.exe')) -Verb RunAs -WindowStyle Hidden -ArgumentList @('-NoProfile','-NonInteractive','-ExecutionPolicy','Bypass','-EncodedCommand'," + literal(encoded) + ");$p.WaitForExit();exit $p.ExitCode} catch {$e=$_.Exception;while($e.InnerException){$e=$e.InnerException};if($e -is [ComponentModel.Win32Exception]){exit $e.NativeErrorCode};exit 1}"
    installed = Path(os.environ["ProgramFiles"]) / "AgentGoSWEFirewall" / session_id / "session.json"
    state = {"schema": "agentgo.swe-firewall-owner/v2", "session_id": session_id, "status": "authorizing",
             "root_id": store.root_id, "owner_pid": os.getpid(), "owner_birth": owner_birth,
             "staging": str(stage), "protected_directory": str(installed.parent), "started_at": time.time(),
             "broker_sha256": hashlib.sha256(broker).hexdigest(), "policy_sha256": hashlib.sha256(policy).hexdigest(),
             "protected_source_bytes": len(broker) + len(policy)}
    storage.atomic_json(control_dir / "state.json", state)
    process = None
    try:
        deadline = time.monotonic() + timeout
        with (control_dir / "startup.log").open("wb") as log:
            process = subprocess.Popen(["powershell.exe", "-NoProfile", "-NonInteractive", "-Command", launch],
                stdout=log, stderr=subprocess.STDOUT, env=helper_environment(), creationflags=subprocess.CREATE_NO_WINDOW)
        context["process"] = process
        state["launcher_pid"] = process.pid
        storage.atomic_json(control_dir / "state.json", state)
        wait_ready(process, installed, deadline)
        session = Session(installed)
        session.process, session.guard, session.control_dir = process, guard, control_dir
        session.owns_helper = True
        context["session"] = session
        session.check()
        storage.atomic_json(control_dir / "session.json", session.data)
        state.update(status="ready", broker_pid=session.data["pid"], recovered_sessions=session.data.get("recovered_sessions", []))
        storage.atomic_json(control_dir / "state.json", state)
        atexit.register(session._exit_cleanup)
        return session
    except BaseException as error:
        (control_dir / "cancelled").touch()
        stop_launcher(process)
        state.update(status="startup_failed", error_code=getattr(error, "code", type(error).__name__),
                     protected_cleanup_pending=installed.parent.exists())
        storage.atomic_json(control_dir / "state.json", state)
        guard.close()
        raise
    finally:
        storage.remove_owned(store.path, stage, stage_identity)
