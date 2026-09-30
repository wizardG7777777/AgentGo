"""One native command path: startup barrier, bounded output, and owned processes."""
from __future__ import annotations
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import threading
import time
import uuid

from storage import StorageError, active, atomic_json, contained, read_json
import process_identity


def process_library():
    try:
        import psutil
    except ImportError:
        raise StorageError("runner_dependency_missing", "install requirements-runner.txt in the Runner environment") from None
    return psutil


class CommandResult:
    def __init__(self, command):
        self.args, self.returncode = command.argv, command.returncode
        self.stdout_path, self.stderr_path = command.stdout_path, command.stderr_path
        self.command_ref = str(command.directory / "command.json")

    def bytes(self, path, limit=4 * 1024 * 1024):
        if path.stat().st_size > limit:
            raise StorageError("output_requires_streaming", path.name)
        return path.read_bytes()

    @property
    def stdout(self):
        return self.bytes(self.stdout_path)

    @property
    def stderr(self):
        return self.bytes(self.stderr_path)

    def stdout_lines(self):
        with self.stdout_path.open("rb") as stream:
            yield from stream

    def text(self, stream="stdout"):
        path = self.stdout_path if stream == "stdout" else self.stderr_path
        with path.open(encoding="utf-8", errors="replace") as source:
            yield from source


def stdout_source(result):
    return result.stdout_path if isinstance(result, CommandResult) else result.stdout


def output_summary(result, limit=65536):
    """A bounded display/prompt summary; judgement uses the original machine data."""
    if not isinstance(result, CommandResult):
        return (result.stdout + result.stderr)[-limit:]
    tail = b""
    for path in (result.stdout_path, result.stderr_path):
        with path.open("rb") as stream:
            stream.seek(max(0, path.stat().st_size - limit))
            tail = (tail + stream.read(limit))[-limit:]
    return tail


def save_output_reference(result, path, *, stderr=False):
    """Managed raw output has one owner; legacy consumers retain their file layout."""
    path = Path(path)
    if isinstance(result, CommandResult):
        source = result.stderr_path if stderr else result.stdout_path
        atomic_json(path.with_name(path.name + ".ref.json"),
                    {"schema": "agentgo.swe-command-output-ref/v1", "path": str(source), "command_ref": result.command_ref})
    else:
        path.write_bytes(result.stderr if stderr else result.stdout)


class ManagedProcess:
    def __init__(self, attempt, argv, *, cwd, env=None, input_data=None, label="command", credentials=False, log_path=None, firewall_programs=()):
        self.attempt, self.psutil = attempt, process_library()
        self.argv = [str(x) for x in argv]
        self.cwd = str(Path(cwd).resolve(strict=True))
        environment = attempt.environment(env, credentials=credentials)
        executable = shutil.which(self.argv[0], path=environment.get("PATH"))
        if not executable:
            raise StorageError("required_executable_missing", Path(self.argv[0]).name)
        command = [executable, *self.argv[1:]]
        attempt.check()
        firewall = getattr(attempt.storage, "firewall", None)
        self.directory = attempt.path / "commands" / uuid.uuid4().hex
        self.directory.mkdir()
        import scratch_scope
        self.scratch_scope, environment = scratch_scope.create(attempt, self.directory, environment)
        self.stdout_path = contained(attempt.storage.path, log_path) if log_path else self.directory / "stdout.log"
        self.stderr_path = self.directory / "stderr.log"
        self.stdout_path.parent.mkdir(parents=True, exist_ok=True)
        self.record = {"schema": "agentgo.swe-command/v1", "command_id": self.directory.name,
                       "phase": attempt.phase_name, "label": label, "argv": self.argv, "cwd": self.cwd,
                       "started_at": time.time(), "status": "starting", "stdout": str(self.stdout_path),
                       "stderr": str(self.stderr_path), "output_complete": False, "quiescent": False,
                       "scratch": environment["TMPDIR"], "output_bytes": 0}
        self.record["supervisor_identity"] = process_identity.capture(self.psutil.Process(os.getpid()))
        atomic_json(self.directory / "command.json", self.record)
        self.job = self.guardian = None
        self.error = None
        self._mutex, self._stop_lock = threading.RLock(), threading.RLock()
        self._finished, self._readers = False, []
        self._known = {}
        self._output_bytes = 0
        self._output_error = None
        self._done = threading.Event()
        self._firewall_scope = None
        self.popen = None
        options = {"creationflags": subprocess.CREATE_NO_WINDOW} if os.name == "nt" else {"start_new_session": True}
        launcher_python = str(Path(getattr(sys, "_base_executable", sys.executable)).resolve(strict=True))
        # The stdlib-only barrier must itself be the process placed in the Job;
        # bypass Windows venv redirectors and user site startup code.
        attempt.processes.append(self)
        try:
            if os.name == 'nt' and firewall:
                from firewall_windows import ProgramLeases
                self._firewall_scope = ProgramLeases(firewall, attempt.attempt_id)
                self._firewall_scope.prepare([executable, *firewall_programs])
                self.record['firewall_permissions'] = self._firewall_scope.record()
                atomic_json(self.directory / 'command.json', self.record)
            self.popen = subprocess.Popen([launcher_python, "-I", "-S", "-B", str(Path(__file__).with_name("process_launcher.py"))],
                            cwd=self.cwd, env=environment, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT if log_path else subprocess.PIPE, **options)
            self.pid = self.popen.pid
            original = self.psutil.Process(self.pid)
            self._known[self.pid] = original
            self.record.update(pid=self.pid, birth=original.create_time())
            self.record["process_identity"] = process_identity.capture(original)
            self.record["observed_processes"] = [self.record["process_identity"]]
            if os.name == "nt":
                from process_windows import Job
                self.job = Job()
                self.job.assign(self.pid)
                self.record["coverage"] = "windows_job"
                self.record["kill_on_supervisor_exit"] = True
            else:
                self.record["coverage"] = "observed_tree"
                self._start_guardian(self.record["process_identity"], environment)
            self.record["status"] = "running"
            atomic_json(self.directory / "command.json", self.record)
            for pipe, path in ((self.popen.stdout, self.stdout_path), (self.popen.stderr, self.stderr_path)):
                if pipe is None:
                    path.touch()
                    continue
                thread = threading.Thread(target=self._read, args=(pipe, path), daemon=True)
                thread.start()
                self._readers.append(thread)
            self._monitor = threading.Thread(target=self._watch, name="swe-command-monitor-" + self.directory.name, daemon=True)
            self._monitor.start()

            def feed():
                try:
                    self.popen.stdin.write(json.dumps(command).encode("utf-8") + b"\n")
                    if input_data:
                        self.popen.stdin.write(input_data)
                    self.popen.stdin.close()
                except (BrokenPipeError, OSError):
                    pass
            self._feeder = threading.Thread(target=feed, daemon=True)
            self._feeder.start()
        except BaseException as failure:
            self._done.set()
            if self.popen is not None and self.popen.poll() is None:
                self.popen.kill()
            if self.popen is not None:
                self.popen.wait()
            tree_quiet = True
            if self.job:
                self.job.terminate()
                deadline = time.monotonic() + 5
                while self.job.active() and time.monotonic() < deadline:
                    time.sleep(0.05)
                tree_quiet = self.job.active() == 0
                self.job.close()
                self.job = None
            if self.guardian:
                if self.guardian.poll() is None:
                    self.guardian.kill()
                self.guardian.wait()
                for pipe in (self.guardian.stdin, self.guardian.stdout):
                    pipe.close()
            monitor = getattr(self, "_monitor", None)
            if monitor:
                monitor.join(self.attempt.policy.max_monitor_stop_seconds)
            for thread in self._readers:
                thread.join(5)
            for pipe in ((self.popen.stdin, self.popen.stdout, self.popen.stderr) if self.popen is not None else ()):
                if pipe and not pipe.closed:
                    pipe.close()
            self._finished = True
            quiet = tree_quiet and not any(thread.is_alive() for thread in self._readers) and not (monitor and monitor.is_alive())
            self._release_firewall(quiet)
            self.record.update(status="startup_failed", quiescent=quiet, finished_at=time.time(),
                               error_code=getattr(failure, "code", type(failure).__name__))
            atomic_json(self.directory / "command.json", self.record)
            raise

    def _start_guardian(self, identity, environment):
        env = {k: v for k, v in environment.items() if not k.startswith(("SWE_", "AGENTGO_", "OPENAI_", "ANTHROPIC_"))}
        self.guardian = subprocess.Popen([sys.executable, str(Path(__file__).with_name("process_guardian.py")),
                         json.dumps(identity), str(self.directory / "guardian.json"), str(self.scratch_scope)],
                         stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                         env=env, start_new_session=True)
        diagnostics = threading.Thread(target=self._read, args=(self.guardian.stderr, self.directory / "guardian.stderr.log"), daemon=True)
        diagnostics.start()
        self._readers.append(diagnostics)
        ready = []
        thread = threading.Thread(target=lambda: ready.append(self.guardian.stdout.readline()), daemon=True)
        thread.start()
        thread.join(5)
        if not ready or ready[0].strip() != b"ready":
            self.record["guardian_ready_timeout"] = thread.is_alive()
            if self.guardian.poll() is None:
                self.guardian.kill()
            self.guardian.wait()
            thread.join(2)
            self.record["guardian_exit_code"] = self.guardian.returncode
            raise StorageError("process_isolation_unavailable", "guardian startup")
        self.record["guardian_identity"] = process_identity.capture(self.psutil.Process(self.guardian.pid))

    def _read(self, pipe, path):
        try:
            with pipe, path.open("wb") as destination:
                while data := os.read(pipe.fileno(), 65536):
                    with self._mutex:
                        remaining = max(0, self.attempt.policy.max_command_output_bytes - self._output_bytes)
                        allowed = self.attempt.charge_output(min(len(data), remaining))
                        if allowed < len(data):
                            self._output_error = StorageError("output_limit_exceeded", "command")
                        destination.write(data[:allowed])
                        destination.flush()
                        self._output_bytes += allowed
                destination.flush()
                os.fsync(destination.fileno())
        except BaseException as error:
            self._output_error = error if isinstance(error, StorageError) else StorageError("command_output_write_failed")

    def _watch(self):
        while not self._done.wait(0.1):
            observation = "descendants"
            try:
                changed = False
                for process in list(self._known.values()):
                    try:
                        for child in process.children(recursive=True):
                            if child.pid not in self._known:
                                try:
                                    observation = "child_identity"
                                    identity = process_identity.capture(child)
                                except (FileNotFoundError, ProcessLookupError, self.psutil.NoSuchProcess):
                                    continue
                                self._known[child.pid] = child
                                self.record["observed_processes"].append(identity)
                                changed = True
                    except (FileNotFoundError, ProcessLookupError, self.psutil.NoSuchProcess, self.psutil.ZombieProcess):
                        pass
                if changed:
                    observation = "command_record"
                    with self._mutex:
                        atomic_json(self.directory / "command.json", self.record)
                if self._output_error:
                    raise self._output_error
                observation = "resource_check"
                self.attempt.check()
            except BaseException as error:
                self.record["monitor_failure"] = {"observation": observation,
                    "error_type": type(error).__name__, "errno": getattr(error, "errno", None)}
                self.error = error if isinstance(error, StorageError) else StorageError("process_quiescence_unknown")
                try:
                    self._terminate_tree()
                except BaseException:
                    self.error = StorageError("process_quiescence_unknown")
                return

    @property
    def returncode(self):
        return self.popen.returncode if self.popen is not None else None

    def poll(self):
        return self.popen.poll()

    def _terminate_tree(self):
        with self._stop_lock:
            if self.job:
                self.job.terminate()
            else:
                alive = []
                for process in list(self._known.values()):
                    try:
                        if process.is_running() and process.status() != self.psutil.STATUS_ZOMBIE:
                            process.terminate()
                            alive.append(process)
                    except (self.psutil.NoSuchProcess, self.psutil.ZombieProcess):
                        pass
                _, alive = self.psutil.wait_procs(alive, timeout=0.5)
                for process in alive:
                    try:
                        process.kill()
                    except self.psutil.NoSuchProcess:
                        pass
            if self.popen.poll() is None:
                self.popen.kill()

    def stop(self):
        if self._finished:
            return
        with self._mutex:
            self.record["stop_requested"] = True
        self._terminate_tree()
        self.popen.wait(timeout=10)
        self.finish()

    def wait(self, timeout=None):
        try:
            self.popen.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            self.error = StorageError("command_timeout")
            self.stop()
            raise self.error
        self.finish()
        self.raise_if_failed()
        return self.returncode

    def finish(self):
        if self._finished:
            return
        self._done.set()
        self._monitor.join(timeout=self.attempt.policy.max_monitor_stop_seconds)
        if self._monitor.is_alive():
            self.error = StorageError("process_quiescence_unknown", "monitor did not stop")
            self._record_cleanup_failure('process_cleanup_error', 'monitor.join', self.error)
        # A parent that exits leaving a writer behind does not finish the command.
        windows_job = self.job is not None
        operation = 'job.query' if windows_job else 'guardian.stop'
        try:
            if self.job:
                remaining = self.job.active()
                if remaining:
                    self.record["descendants_cancelled"] = remaining
                    operation = 'job.terminate'
                    self.job.terminate()
                deadline = time.monotonic() + 5
                operation = 'job.wait_for_exit'
                while self.job.active() and time.monotonic() < deadline:
                    time.sleep(0.05)
                self.record['job_active_after_wait'] = self.job.active()
                quiescent = self.record['job_active_after_wait'] == 0
                operation = 'job.close'
                self.job.close()
                self.job = None
            else:
                if self.guardian and self.guardian.poll() is None:
                    self.guardian.stdin.write(b"stop\n")
                    self.guardian.stdin.close()
                if self.guardian:
                    self.guardian.wait(timeout=8)
                    self.guardian.stdout.close()
                operation = 'guardian.read_receipt'
                fact = read_json(self.directory / "guardian.json")
                quiescent = fact["quiescent"]
                if fact.get("scratch_error"):
                    self.error = self.error or StorageError("scratch_permission_recovery_failed", fact["scratch_error"])
            operation = 'output_readers.join'
            for thread in self._readers:
                thread.join(5)
            self.record['output_readers_stopped'] = not any(thread.is_alive() for thread in self._readers)
            if not self.record['output_readers_stopped']:
                quiescent = False
            if not quiescent:
                self.error = StorageError("process_quiescence_unknown")
                self._record_cleanup_failure('process_cleanup_error', 'process_tree_or_output_not_quiescent', self.error)
        except BaseException as error:
            quiescent = False
            self.error = StorageError("process_quiescence_unknown")
            self._record_cleanup_failure('process_cleanup_error', operation, error)
        if self._output_error:
            self.error = self._output_error
        self.record['monitor_stopped'] = not self._monitor.is_alive()
        if not self.record['monitor_stopped']:
            quiescent = False
        # Filesystem recovery is a separate fact from a proven process exit.
        # Windows recovery reads attempt.json, whose atomic updates use this
        # same mutex. Never race those reads against the inventory publisher.
        if windows_job and quiescent:
            try:
                import scratch_scope
                with self.attempt._mutex:
                    scratch_scope.recover(self.scratch_scope, {"quiescent": True, "remaining_pids": [],
                        "unknown": False, "coverage": "windows_job"})
                self.record['scratch_recovered'] = True
            except BaseException as error:
                self.record['scratch_recovered'] = False
                self._record_cleanup_failure('scratch_recovery_error', 'scratch.recover', error)
                self.error = self.error or (error if isinstance(error, StorageError)
                                            else StorageError('scratch_permission_recovery_failed'))
        self._release_firewall(quiescent)
        self._finished = True
        self.record.update(status="failed" if self.error else "completed", exit_code=self.returncode,
                           finished_at=time.time(), output_complete=self.error is None,
                           output_bytes=self._output_bytes, quiescent=quiescent,
                           error_code=getattr(self.error, "code", None))
        atomic_json(self.directory / "command.json", self.record)

    def _record_cleanup_failure(self, field, operation, error):
        def detail(value):
            result = {'error_type': type(value).__name__, 'error_code': getattr(value, 'code', None),
                      'errno': getattr(value, 'errno', None), 'winerror': getattr(value, 'winerror', None)}
            filename = getattr(value, 'filename', None)
            if filename:
                try:
                    result['path'] = Path(os.path.abspath(os.fsdecode(filename))).relative_to(self.attempt.storage.path).as_posix()[:512]
                except (TypeError, ValueError, OSError):
                    result['path'] = '<outside-owned-root>'
            return result
        record = {'operation': getattr(error, 'operation', operation), **detail(error)}
        causes, seen, current = [], {id(error)}, error
        for _ in range(3):
            current = current.__cause__ or current.__context__
            if current is None or id(current) in seen:
                break
            seen.add(id(current)); causes.append(detail(current))
        if causes:
            record['causes'] = causes
        self.record[field] = record

    def _release_firewall(self, quiescent):
        if self._firewall_scope is None:
            return
        if quiescent:
            try:
                self._firewall_scope.release(quiescent=True)
            except StorageError as error:
                self.error = self.error or error
        self.record['firewall_permissions'] = self._firewall_scope.record()
        self.record['firewall_permissions']['process_tree_quiescent'] = quiescent

    def raise_if_failed(self):
        if self.error:
            raise self.error


def run(argv, *, cwd=None, env=None, input_data=None, timeout=None, check=True, label="command", credentials=False, firewall_programs=()):
    attempt = active()
    if attempt is None:
        return subprocess.run([str(x) for x in argv], cwd=cwd, env=env, input=input_data,
                              capture_output=True, timeout=timeout, check=check)
    process = ManagedProcess(attempt, argv, cwd=cwd or attempt.worktree, env=env,
                             input_data=input_data, label=label, credentials=credentials, firewall_programs=firewall_programs)
    try:
        process.wait(timeout=timeout)
    except BaseException:
        if not process._finished:
            process.stop()
        raise
    result = CommandResult(process)
    if check and result.returncode:
        raise StorageError("command_failed", f"{Path(str(argv[0])).name} exit={result.returncode}; command={process.directory.name}")
    return result
