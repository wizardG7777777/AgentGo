"""Ordinary-user Cargo test executable hook with independently owned scratch."""
from __future__ import annotations

import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import threading
from types import SimpleNamespace
import uuid

import process_identity
import scratch_scope
import storage


def hook_command():
    return shlex.join([sys.executable, '-B', str(Path(__file__).resolve())])


def parent_scope(path):
    path = Path(path).resolve(strict=True)
    marker_path = next((p / 'storage-root.json' for p in path.parents if (p / 'storage-root.json').is_file()), None)
    if marker_path is None:
        raise storage.StorageError('unowned_exec_scope')
    root = marker_path.parent
    storage.contained(root, path, exists=True)
    marker, state = storage.read_json(marker_path), storage.read_json(path / 'attempt.json')
    if (marker['identity'] != storage.file_identity(root) or state['identity'] != storage.file_identity(path)
            or state['root_id'] != marker['root_id'] or state['path'] != str(path)):
        raise storage.StorageError('exec_scope_identity_changed')
    if state.get('status') not in {'allocated', 'running'} or state.get('quiescent') is True:
        raise storage.StorageError('exec_scope_parent_not_active')
    paths = storage.physical_paths(root, state)
    return SimpleNamespace(path=path, scratch=paths['scratch'], attempt_id=state['attempt_id'],
        policy=storage.StoragePolicy.from_record(state['policy']),
        storage=SimpleNamespace(path=root, root_id=marker['root_id']))


def ensure_stopped(attempt, *, recover=False):
    """Nested exec scopes remain part of archive/cleanup quiescence checks."""
    import psutil
    for path in sorted((attempt.path / 'exec-scopes').glob('*/exec.json')):
        record = storage.read_json(path)
        if record.get('schema') != 'agentgo.swe-scoped-exec/v1':
            raise storage.StorageError('scoped_exec_receipt_invalid')
        for key in ('identity', 'guardian_identity'):
            identity = record.get(key)
            if identity and process_identity.alive(identity, psutil):
                raise storage.StorageError('scoped_exec_process_still_alive')
        if record.get('released'):
            receipt = storage.read_json(path.with_name('guardian.json'))
            if not receipt.get('quiescent') or receipt.get('unknown') or receipt.get('remaining_pids'):
                raise storage.StorageError('scoped_exec_quiescence_unknown')
            recovery = path.with_name('scratch-permissions.json')
            if not recovery.exists() or storage.read_json(recovery).get('complete') is not True:
                if not recover:
                    raise storage.StorageError('scoped_exec_recovery_incomplete')
                scratch_scope.recover(path.with_name('scratch-scope.json'), receipt)


def execute(argv):
    if os.name != 'posix' or not argv:
        raise storage.StorageError('scoped_exec_platform_or_command_invalid')
    import psutil
    attempt = parent_scope(os.environ[scratch_scope.ENV_ATTEMPT])
    control = storage.contained(attempt.storage.path, attempt.path / 'exec-scopes' / uuid.uuid4().hex)
    control.mkdir(parents=True)
    env = {k: v for k, v in os.environ.items()
        if not k.startswith(('SWE_', 'AGENTGO_', 'OPENAI_', 'ANTHROPIC_'))}
    scope, env = scratch_scope.create(attempt, control, env)
    read_fd, write_fd = os.pipe()
    process = guardian = None
    try:
        # Separate argv barrier preserves the caller's stdin and streams outputs
        # directly to their existing owner; no duplicate raw output files.
        process = subprocess.Popen([sys.executable, '-I', '-S', '-B',
            str(Path(__file__).with_name('process_launcher.py')), '--argv-fd', str(read_fd)],
            stdin=0, stdout=1, stderr=2, env=env, pass_fds=(read_fd,), start_new_session=True)
        os.close(read_fd)
        read_fd = None
        identity = process_identity.capture(psutil.Process(process.pid))
        record = {'schema': 'agentgo.swe-scoped-exec/v1',
            'identity': identity, 'scope': str(scope), 'program': str(argv[0]),
            'output_owner': 'caller', 'status': 'starting', 'released': False}
        storage.atomic_json(control / 'exec.json', record)
        guardian = subprocess.Popen([sys.executable, '-B', str(Path(__file__).with_name('process_guardian.py')),
            json.dumps(identity), str(control / 'guardian.json'), str(scope)],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=2, env=env, start_new_session=True)
        record['guardian_identity'] = process_identity.capture(psutil.Process(guardian.pid))
        storage.atomic_json(control / 'exec.json', record)
        ready = []
        thread = threading.Thread(target=lambda: ready.append(guardian.stdout.readline()), daemon=True)
        thread.start()
        thread.join(5)
        if not ready or ready[0].strip() != b'ready':
            raise storage.StorageError('scoped_exec_guardian_not_ready')
        record.update(status='running', released=True)
        storage.atomic_json(control / 'exec.json', record)
        with os.fdopen(write_fd, 'wb') as barrier:
            write_fd = None
            barrier.write(json.dumps(argv).encode('utf-8') + b'\n')
        code = process.wait()
        guardian.stdin.write(b'stop\n')
        guardian.stdin.close()
        guardian.wait(timeout=attempt.policy.max_scan_seconds + 15)
        receipt = storage.read_json(control / 'guardian.json')
        if guardian.returncode or not receipt.get('quiescent') or receipt.get('scratch_error'):
            raise storage.StorageError('scoped_exec_cleanup_unknown')
        storage.atomic_json(control / 'exec.json', dict(record, status='completed', exit_code=code, quiescent=True))
        return code
    finally:
        for fd in (read_fd, write_fd):
            if fd is not None:
                os.close(fd)
        if guardian is not None:
            if guardian.stdin and not guardian.stdin.closed:
                guardian.stdin.close()
            if guardian.poll() is None:
                try:
                    guardian.wait(timeout=attempt.policy.max_scan_seconds + 15)
                except subprocess.TimeoutExpired:
                    guardian.kill()
                    guardian.wait()
            guardian.stdout.close()
        if process is not None:
            if process.poll() is None:
                process.kill()
            process.wait()


if __name__ == '__main__':
    try:
        raise SystemExit(execute(sys.argv[1:]))
    except Exception as error:
        print('SWE_SCOPED_EXEC_ERROR:' + getattr(error, 'code', type(error).__name__), file=sys.stderr, flush=True)
        raise SystemExit(125)
