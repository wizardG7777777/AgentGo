"""Keep Windows Node dependency links relocatable without copying their targets."""
from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
import stat
import uuid

from storage import StorageError


def _identity(path):
    info = path.lstat()
    return info.st_dev, info.st_ino, getattr(info, 'st_reparse_tag', None), os.readlink(path)


def normalize_node_junctions(root):
    """Validate the complete link plan before changing owned dependency entries.

    Junction targets are never traversed by the inventory or copied. Relative
    symbolic links continue to resolve inside a subsequently copied worktree.
    """
    if os.name != 'nt':
        return {'mode': 'native', 'converted_junctions': 0}
    root = Path(root).resolve(strict=True)
    pending, changes, mappings = [root], [], []
    while pending:
        directory = pending.pop()
        with os.scandir(directory) as entries:
            for entry in entries:
                if entry.name in {'.git', '.agentgo', '.swe-build'}:
                    continue
                path = Path(entry.path)
                info = entry.stat(follow_symlinks=False)
                junction = getattr(info, 'st_reparse_tag', None) == stat.IO_REPARSE_TAG_MOUNT_POINT
                symbolic = stat.S_ISLNK(info.st_mode)
                if junction or symbolic:
                    relative = path.relative_to(root)
                    if 'node_modules' not in relative.parts:
                        if junction:
                            raise StorageError('windows_dependency_link_outside_node_modules', relative.as_posix())
                        continue
                    try:
                        target = path.resolve(strict=True)
                    except (OSError, RuntimeError):
                        raise StorageError('windows_dependency_link_unresolved', relative.as_posix()) from None
                    if not target.is_relative_to(root):
                        raise StorageError('windows_dependency_link_target_outside_worktree', relative.as_posix())
                    if junction and not target.is_dir():
                        raise StorageError('windows_dependency_link_target_not_directory', relative.as_posix())
                    target_text = os.path.relpath(target, path.parent)
                    if symbolic and Path(os.readlink(path)).is_absolute():
                        raise StorageError('windows_dependency_absolute_symlink', relative.as_posix())
                    if junction:
                        changes.append((path, target_text, target, _identity(path)))
                    mappings.append({'path': relative.as_posix(), 'target': target.relative_to(root).as_posix()})
                    continue
                if stat.S_ISDIR(info.st_mode):
                    pending.append(path)
                elif getattr(info, 'st_file_attributes', 0) & stat.FILE_ATTRIBUTE_REPARSE_POINT:
                    raise StorageError('windows_dependency_unknown_reparse_point', path.relative_to(root).as_posix())
    # Validate symlink capability before removing any junction. No elevation or
    # global policy changes are attempted when this capability is unavailable.
    if changes:
        probe = root / ('.swe-link-probe-' + uuid.uuid4().hex)
        try:
            probe.symlink_to(os.path.relpath(changes[0][2], root), target_is_directory=True)
        except OSError:
            raise StorageError('windows_dependency_symlink_permission_unavailable') from None
        finally:
            if probe.is_symlink():
                probe.unlink()
    for path, target_text, target, identity in changes:
        if not path.is_junction() or _identity(path) != identity:
            raise StorageError('windows_dependency_link_identity_changed', path.relative_to(root).as_posix())
        stage = path.with_name('.swe-link-' + uuid.uuid4().hex)
        try:
            stage.symlink_to(target_text, target_is_directory=True)
            # Recheck immediately before removing only the junction entry.
            if not path.is_junction() or _identity(path) != identity:
                raise StorageError('windows_dependency_link_identity_changed', path.relative_to(root).as_posix())
            os.rmdir(path)
            stage.rename(path)  # Windows rename refuses an existing destination.
            if not path.is_symlink() or path.is_junction() or path.resolve(strict=True) != target:
                raise StorageError('windows_dependency_link_conversion_invalid', path.relative_to(root).as_posix())
        except OSError:
            raise StorageError('windows_dependency_link_conversion_failed', path.relative_to(root).as_posix()) from None
        finally:
            if stage.is_symlink():
                stage.unlink()
    mappings.sort(key=lambda item: item['path'])
    return {'mode': 'relative-symbolic-links', 'converted_junctions': len(changes),
            'links': len(mappings), 'mapping_sha256': hashlib.sha256(
                json.dumps(mappings, sort_keys=True, separators=(',', ':')).encode()).hexdigest()}
