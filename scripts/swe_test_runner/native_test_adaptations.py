"""Explicit, content-pinned platform adaptations of upstream test fixtures."""
from __future__ import annotations

import hashlib
from pathlib import Path
import re
import sys

from suite_config import project_path


def validate(metadata):
    adaptations = metadata.get('test_adaptations', [])
    required = metadata.get('required_regression_tests', [])
    if (not isinstance(required, list)
            or any(not isinstance(name, str) or not name.strip() for name in required)
            or len(set(required)) != len(required)):
        raise ValueError('invalid required native regression identities')
    if not isinstance(adaptations, list):
        raise ValueError('invalid native test adaptations')
    seen = set()
    for item in adaptations:
        if not isinstance(item, dict) or set(item) != {
                'platform', 'path', 'upstream_sha256', 'adapted_sha256', 'replacements'}:
            raise ValueError('invalid native test adaptation fields')
        name = project_path(item['path'])
        identity = (item['platform'], name)
        if (item['platform'] not in {'win32', 'linux', 'darwin'} or identity in seen
                or name not in metadata['test_files'] or name in metadata['source_files']):
            raise ValueError('test adaptation must identify a distinct declared test on a known platform')
        seen.add(identity)
        for key in ('upstream_sha256', 'adapted_sha256'):
            if not re.fullmatch('[0-9a-f]{64}', item[key]):
                raise ValueError('test adaptation requires immutable content identities')
        edits = item['replacements']
        if not isinstance(edits, list) or not edits:
            raise ValueError('test adaptation requires exact replacements')
        for edit in edits:
            if (not isinstance(edit, dict) or set(edit) != {'old', 'new'}
                    or not isinstance(edit['old'], str) or not edit['old']
                    or not isinstance(edit['new'], str) or edit['old'] == edit['new']):
                raise ValueError('invalid test adaptation replacement')


def apply(root, task, platform=None):
    validate(task.metadata)
    root = Path(root).resolve(strict=True)
    planned, receipts = [], []
    for item in task.metadata.get('test_adaptations', []):
        if item['platform'] != (platform or sys.platform):
            continue
        path = root / item['path']
        # Reject links in every component, including links within the workspace.
        current = path
        while current != root:
            if current.is_symlink():
                raise ValueError('test adaptation path cannot contain links')
            current = current.parent
        if not path.resolve(strict=True).is_relative_to(root):
            raise ValueError('test adaptation escaped its workspace')
        raw = path.read_bytes()
        # Git's checkout conversion may use CRLF. Pin the canonical Git text,
        # retain the physical-input hash in receipts, and publish LF as required.
        before = raw.replace(b'\r\n', b'\n')
        if hashlib.sha256(before).hexdigest() != item['upstream_sha256']:
            raise ValueError('test adaptation upstream content changed: ' + item['path'])
        text = before.decode('utf-8')
        for edit in item['replacements']:
            if text.count(edit['old']) != 1:
                raise ValueError('test adaptation replacement must match exactly once')
            text = text.replace(edit['old'], edit['new'], 1)
        after = text.encode('utf-8')
        if hashlib.sha256(after).hexdigest() != item['adapted_sha256']:
            raise ValueError('test adaptation output identity changed')
        planned.append((path, after))
        receipts.append({key: item[key] for key in ('platform', 'path', 'upstream_sha256', 'adapted_sha256')} | {
            'physical_input_sha256': hashlib.sha256(raw).hexdigest(),
            'line_endings': 'crlf-to-lf' if raw != before else 'lf'})
    # Validate every selected file before changing any file.
    for path, data in planned:
        path.write_bytes(data)
    return receipts


def check_collection(task, report, stage):
    if stage in {'clean', 'clean-baseline'}:
        return
    required = task.metadata.get('required_regression_tests', [])
    missing = sorted(set(required) - set(report['collected_nodeids']))
    if missing:
        raise ValueError('required native regression not collected on ' + sys.platform + ': ' + ', '.join(missing))
    unexecuted = [name for name in required if report['case_outcomes'].get(name) not in {'pass', 'fail'}]
    if unexecuted:
        raise ValueError('required native regression not executed: ' + ', '.join(unexecuted))
    if stage in {'red', 'targeted-baseline'}:
        not_failed = [name for name in required if report['case_outcomes'][name] != 'fail']
        if not_failed:
            raise ValueError('required native regression did not fail before source fix: ' + ', '.join(not_failed))
