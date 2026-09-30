"""Owned command scratch; permission recovery only after writer quiescence."""
from __future__ import annotations

import os
from pathlib import Path
import stat
import sys
import time
import uuid

import storage

SCHEMA = 'agentgo.swe-scratch-scope/v1'
ENV_ATTEMPT = 'SWE_RESOURCE_ATTEMPT'


def create(attempt, control, environment):
    parent = storage.contained(attempt.storage.path, attempt.scratch / 'x')
    parent.mkdir(exist_ok=True)
    target = parent / uuid.uuid4().hex[:8]
    if len(os.fsencode(str(target))) > attempt.policy.max_scratch_prefix_bytes:
        raise storage.StorageError('testbed_path_too_long', 'command scratch')
    target.mkdir(mode=0o700)
    value = {'schema': SCHEMA, 'root': str(attempt.storage.path), 'root_id': attempt.storage.root_id,
        'root_identity': storage.file_identity(attempt.storage.path), 'attempt': str(attempt.path),
        'attempt_id': attempt.attempt_id, 'scratch_base': str(attempt.scratch), 'path': str(target),
        'identity': storage.file_identity(target), 'created_at': time.time()}
    scope = Path(control) / 'scratch-scope.json'
    storage.atomic_json(scope, value)
    env = dict(environment)
    for key in ('TEMP', 'TMP', 'TMPDIR'):
        env[key] = str(target)
    (target / 'go').mkdir()
    env['GOTMPDIR'] = str(target / 'go')
    env[ENV_ATTEMPT] = str(attempt.path)
    return scope, env


def _open_flags():
    if hasattr(os, 'O_PATH'):
        access = os.O_PATH
    elif sys.platform == 'darwin' and hasattr(os, 'O_EVTONLY'):
        access = os.O_EVTONLY
    else:
        access = os.O_RDONLY | getattr(os, 'O_NONBLOCK', 0)
    return access | os.O_NOFOLLOW | getattr(os, 'O_CLOEXEC', 0)


def _chmod_fd(fd, mode):
    if hasattr(os, 'O_PATH'):
        os.chmod('/proc/self/fd/' + str(fd), mode)
    else:
        os.fchmod(fd, mode)


def recover(scope_path, proof):
    """Add owner rights needed to archive/remove scratch; retain original modes."""
    if proof.get('quiescent') is not True or proof.get('remaining_pids') or proof.get('unknown'):
        raise storage.StorageError('process_quiescence_unknown', 'scratch recovery')
    scope_path = Path(scope_path)
    scope = storage.read_json(scope_path)
    if scope.get('schema') != SCHEMA:
        raise storage.StorageError('scratch_scope_schema_invalid')
    root, target = Path(scope['root']), Path(scope['path'])
    marker = storage.read_json(root / 'storage-root.json')
    if marker['root_id'] != scope['root_id'] or storage.file_identity(root) != scope['root_identity']:
        raise storage.StorageError('scratch_scope_owner_changed')
    state = storage.read_json(Path(scope['attempt']) / 'attempt.json')
    paths = storage.physical_paths(root, state)
    if state['attempt_id'] != scope['attempt_id'] or str(paths['scratch']) != scope['scratch_base']:
        raise storage.StorageError('scratch_scope_owner_changed')
    if target == paths['scratch'] or not target.is_relative_to(paths['scratch']):
        raise storage.StorageError('scratch_scope_outside_owned_scratch')
    receipt_path = scope_path.with_name('scratch-permissions.json')
    value = {'schema': 'agentgo.swe-scratch-permissions/v1', 'scope': str(scope_path),
        'attempt_id': scope['attempt_id'], 'quiescence': proof, 'changes': [], 'complete': False}
    if receipt_path.exists():
        previous = storage.read_json(receipt_path)
        if previous.get('scope') != str(scope_path) or previous.get('attempt_id') != scope['attempt_id']:
            raise storage.StorageError('scratch_recovery_receipt_changed')
        value['changes'] = previous.get('changes', [])
    storage.atomic_json(receipt_path, value)
    if os.name == 'nt':
        storage.contained(root, target, exists=True)
        if storage.file_identity(target) != scope['identity']:
            raise storage.StorageError('scratch_scope_identity_changed')
        value.update(complete=True, platform_action='windows_readonly_left_to_cleanup', finished_at=time.time())
        storage.atomic_json(receipt_path, value)
        return value

    descriptors = []
    try:
        base_fd = os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        descriptors.append(base_fd)
        info = os.fstat(base_fd)
        if {'device': info.st_dev, 'inode': info.st_ino} != scope['root_identity']:
            raise storage.StorageError('scratch_scope_owner_changed')
        parent_fd = base_fd
        for part in target.relative_to(root).parts:
            fd = os.open(part, _open_flags(), dir_fd=parent_fd)
            descriptors.append(fd)
            if not stat.S_ISDIR(os.fstat(fd).st_mode):
                raise storage.StorageError('scratch_scope_path_is_not_directory')
            parent_fd = fd
        original = os.fstat(parent_fd)
        if {'device': original.st_dev, 'inode': original.st_ino} != scope['identity']:
            raise storage.StorageError('scratch_scope_identity_changed')

        def visit(fd, relative):
            before = os.fstat(fd)
            if stat.S_ISLNK(before.st_mode):
                return
            if before.st_uid != os.geteuid() or before.st_dev != original.st_dev:
                raise storage.StorageError('scratch_permission_owner_mismatch', relative)
            directory = stat.S_ISDIR(before.st_mode)
            if not directory and not stat.S_ISREG(before.st_mode):
                return  # No mode change on sockets, FIFOs, or devices.
            old = stat.S_IMODE(before.st_mode)
            mode = old | (0o700 if directory else 0o400)
            if mode != old:
                if not directory and before.st_nlink != 1:
                    raise storage.StorageError('scratch_permission_hardlink', relative)
                change = {'path': relative, 'device': before.st_dev, 'inode': before.st_ino,
                    'uid': before.st_uid, 'old_mode': old, 'new_mode': mode, 'status': 'planned'}
                value['changes'].append(change)
                storage.atomic_json(receipt_path, value)
                _chmod_fd(fd, mode)
                after = os.fstat(fd)
                if (after.st_dev, after.st_ino, stat.S_IMODE(after.st_mode)) != (before.st_dev, before.st_ino, mode):
                    raise storage.StorageError('scratch_permission_identity_changed', relative)
                change['status'] = 'applied'
                storage.atomic_json(receipt_path, value)
            if directory:
                readable = os.open('.', os.O_RDONLY | os.O_DIRECTORY, dir_fd=fd)
                try:
                    with os.scandir(readable) as entries:
                        names = sorted(entry.name for entry in entries if not entry.is_symlink())
                    for name in names:
                        child = os.open(name, _open_flags(), dir_fd=readable)
                        try:
                            visit(child, (relative + '/' + name).lstrip('/'))
                        finally:
                            os.close(child)
                finally:
                    os.close(readable)
        visit(parent_fd, '')
        value.update(complete=True, finished_at=time.time())
        storage.atomic_json(receipt_path, value)
        return value
    finally:
        for fd in reversed(descriptors):
            os.close(fd)
