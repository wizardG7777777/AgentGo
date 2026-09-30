"""Small fixtures for concurrent metadata reads; no provider, ACL edits or UAC."""
import ctypes
import errno
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import storage
import storage_inventory
from storage_test import small_policy


def denied(path, code=5):
    error = PermissionError(errno.EACCES, 'fixture access denied', str(path))
    error.winerror = code
    return error


class InventoryReadTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        # Exercise native Windows classification deterministically on both OSes.
        patch = mock.patch.object(storage.InventoryReader, 'transient_error',
            side_effect=lambda error: getattr(error, 'winerror', None) in {5, 32, 33})
        patch.start()
        self.addCleanup(patch.stop)

    def reader(self, **kwargs):
        return storage.InventoryReader(self.root, retry_transient=True, **kwargs)

    def test_deleted_directory_is_rechecked_without_retrying_missing_data(self):
        child = self.root / 'child'
        child.mkdir()
        original = os.scandir
        def read(path):
            if Path(path) == child:
                child.rmdir()
                raise denied(child)
            return original(path)
        reader = self.reader()
        with mock.patch.object(storage.os, 'scandir', side_effect=read):
            result = storage.measure_tree(self.root, _reader=reader)
        self.assertTrue(result['complete'])
        self.assertEqual(result['files'], 0)
        event = reader.diagnostics['events'][0]
        self.assertEqual((event['operation'], event['outcome'], event['retries']),
                         ('scandir_open', 'disappeared', 0))

    def test_partial_enumeration_is_closed_and_discarded_before_retry(self):
        (self.root / 'a').write_bytes(b'abc')
        original = os.scandir
        closed = []
        class BrokenScan:
            def __enter__(this): return this
            def __exit__(this, *args): closed.append(True)
            def __iter__(this):
                with original(self.root) as entries:
                    yield next(entries)
                raise denied(self.root, 32)
        calls = []
        def read(path):
            calls.append(path)
            if len(calls) == 1: return BrokenScan()
            self.assertEqual(closed, [True])
            return original(path)
        reader = self.reader()
        with mock.patch.object(storage.os, 'scandir', side_effect=read):
            result = storage.measure_tree(self.root, _reader=reader)
        self.assertEqual((result['files'], result['logical_bytes']), (1, 3))
        self.assertEqual(reader.diagnostics['events'][0]['operation'], 'scandir_iterate')
        self.assertEqual(reader.diagnostics['events'][0]['outcome'], 'recovered')

    def test_directory_replaced_with_link_during_error_is_not_followed(self):
        owned, outside = self.root / 'owned', self.root / 'outside'
        owned.mkdir(); outside.mkdir()
        (outside / 'large').write_bytes(b'x' * 4096)
        child = owned / 'child'
        child.mkdir()
        # Verify this host supports user symlinks before the fault is injected.
        check = self.root / 'check'
        try: check.symlink_to(outside, target_is_directory=True)
        except OSError: self.skipTest('user symlinks unavailable')
        check.unlink()
        original = os.scandir
        def read(path):
            if Path(path) == child:
                child.rmdir()
                child.symlink_to(outside, target_is_directory=True)
                raise denied(child)
            return original(path)
        reader = self.reader()
        with mock.patch.object(storage.os, 'scandir', side_effect=read):
            result = storage.measure_tree(owned, _reader=reader)
        self.assertEqual((result['files'], result['links']), (0, 1))
        self.assertEqual(reader.diagnostics['events'][0]['path_state']['kind'], 'leaf_link')

    def test_ancestor_replacement_rejects_retry(self):
        parent, outside = self.root / 'parent', self.root / 'outside'
        parent.mkdir(); outside.mkdir()
        target = parent / 'data'
        target.write_bytes(b'a')
        (outside / 'data').write_bytes(b'b' * 100)
        link = self.root / 'link'
        try: link.symlink_to(outside, target_is_directory=True)
        except OSError: self.skipTest('user symlinks unavailable')
        error = denied(target)
        def read():
            parent.rename(self.root / 'old-parent')
            link.rename(parent)
            raise error
        retry = mock.Mock(side_effect=AssertionError('must not read outside subtree'))
        with self.assertRaises(PermissionError) as caught:
            self.reader().read(target, 'entry_stat', read, retry_action=retry)
        self.assertIs(caught.exception, error)
        self.assertEqual(error.inventory_read_detail['path_state']['kind'], 'ancestor_link')
        retry.assert_not_called()

    def test_retry_uses_fresh_metadata_and_accounts_replacement(self):
        target = self.root / 'file'
        target.write_bytes(b'old')
        class Entry:
            path = str(target)
            def stat(this, **kwargs):
                target.write_bytes(b'new content')
                raise denied(target, 33)
        class Scan:
            def __enter__(this): return this
            def __exit__(this, *args): pass
            def __iter__(this): return iter([Entry()])
        reader = self.reader()
        with mock.patch.object(storage.os, 'scandir', return_value=Scan()):
            result = storage.measure_tree(self.root, _reader=reader)
        self.assertEqual(result['logical_bytes'], len(b'new content'))
        self.assertEqual(reader.diagnostics['events'][0]['retries'], 1)

    def test_persistent_denial_preserves_original_error_and_bounded_retries(self):
        error = denied(self.root)
        action = mock.Mock(side_effect=error)
        reader = self.reader()
        with mock.patch.object(storage.time, 'sleep') as sleep:
            with self.assertRaises(PermissionError) as caught:
                reader.read(self.root, 'scandir_open', action)
        self.assertIs(caught.exception, error)
        self.assertEqual(action.call_count, 5)
        self.assertEqual(sleep.call_count, 4)
        self.assertEqual(error.inventory_operation, 'scandir_open')
        self.assertEqual(error.inventory_read_detail['outcome'], 'failed')

    def test_unknown_error_and_generic_permission_are_not_retried(self):
        for error in (OSError(errno.EIO, 'fixture I/O', str(self.root)),
                      denied(self.root, 206), PermissionError(errno.EACCES, 'fixture')):
            action = mock.Mock(side_effect=error)
            with self.assertRaises(OSError) as caught:
                self.reader().read(self.root, 'scandir_open', action)
            self.assertIs(caught.exception, error)
            self.assertEqual(action.call_count, 1)

    def test_retries_share_sample_budget_and_error_records_are_bounded(self):
        reader = self.reader()
        with mock.patch.object(storage.time, 'sleep'):
            for _ in range(30):
                with self.assertRaises(PermissionError):
                    reader.read(self.root, 'scandir_open', mock.Mock(side_effect=denied(self.root)))
        self.assertLessEqual(reader.diagnostics['retry_wait_seconds'], 1.0)
        self.assertEqual(len(reader.diagnostics['events']), 4)
        self.assertEqual(reader.diagnostics['omitted_events'], 26)

    def test_scan_deadline_prevents_another_retry(self):
        reader = self.reader(max_seconds=0.001)
        with mock.patch.object(storage.time, 'sleep') as sleep:
            with self.assertRaises(storage.InventoryTimeout):
                reader.read(self.root, 'scandir_open', mock.Mock(side_effect=denied(self.root)))
        sleep.assert_not_called()

    def test_error_diagnostics_survive_worker_result_and_attempt_history(self):
        root = storage.StorageRoot(self.root, small_policy())
        attempt = storage.Attempt(root, root.batch({}), 'fixture', 'read-error')
        original = os.scandir
        def read(path):
            if Path(path) == attempt.worktree: raise denied(path, 32)
            return original(path)
        with mock.patch.object(storage.os, 'scandir', side_effect=read), mock.patch.object(storage.time, 'sleep'):
            result = storage_inventory.collect(root.path, attempt.path, 5)
        self.assertFalse(result['ok'])
        self.assertEqual((result['error_operation'], result['error_errno'], result['error_winerror']),
                         ('scandir_open', errno.EACCES, 32))
        self.assertEqual(result['blocked_section'], 'worktree')
        with mock.patch.object(storage_inventory, 'run_inventory', return_value=result):
            attempt._inventory_scan()
        self.assertTrue(attempt.state['inventory_incomplete'])
        self.assertEqual(attempt.state['inventory_error_winerror'], 32)
        with self.assertRaisesRegex(storage.StorageError, 'storage_observation_unavailable'):
            attempt.check()
        attempt._inventory_scan(final=True)
        # A later successful observation never erases the original stop.
        with self.assertRaisesRegex(storage.StorageError, 'storage_observation_unavailable'):
            attempt.check()
        self.assertIn('"error_winerror": 32', (attempt.reports / 'inventory-samples.jsonl').read_text())


@unittest.skipUnless(os.name == 'nt', 'native Windows delete-pending handles')
class NativeDeletePendingTest(unittest.TestCase):
    def test_real_delete_pending_is_handled_without_elevation(self):
        from ctypes import wintypes
        api = ctypes.WinDLL('kernel32', use_last_error=True)
        api.CreateFileW.argtypes = [wintypes.LPCWSTR, wintypes.DWORD, wintypes.DWORD,
            ctypes.c_void_p, wintypes.DWORD, wintypes.DWORD, wintypes.HANDLE]
        api.CreateFileW.restype = wintypes.HANDLE
        api.CloseHandle.argtypes = [wintypes.HANDLE]
        api.CloseHandle.restype = wintypes.BOOL
        api.RemoveDirectoryW.argtypes = [wintypes.LPCWSTR]
        api.RemoveDirectoryW.restype = wintypes.BOOL
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            child = root / 'pending'
            child.mkdir()
            handle = api.CreateFileW(str(child), 0, 7, None, 3, 0x02000000, None)
            if handle == ctypes.c_void_p(-1).value: raise ctypes.WinError(ctypes.get_last_error())
            try:
                if not api.RemoveDirectoryW(str(child)):
                    raise ctypes.WinError(ctypes.get_last_error())
                try:
                    with os.scandir(child) as entries: list(entries)
                except OSError as error:
                    native_error = error
                else:
                    self.fail('fixture did not deny enumeration')
                self.assertIn(native_error.winerror, (3, 5))
                reader = storage.InventoryReader(root, max_seconds=5, retry_transient=True)
                original = storage.time.sleep
                def close_on_wait(delay):
                    nonlocal handle
                    if handle is not None:
                        self.assertTrue(api.CloseHandle(handle))
                        handle = None
                    original(delay)
                with mock.patch.object(storage.time, 'sleep', side_effect=close_on_wait):
                    result = storage.measure_tree(child, _reader=reader)
                self.assertTrue(result['complete'])
                self.assertEqual(result['files'], 0)
                if not isinstance(native_error, FileNotFoundError):
                    self.assertEqual(reader.diagnostics['events'][0]['winerror'], native_error.winerror)
                    self.assertEqual(reader.diagnostics['events'][0]['outcome'], 'disappeared')
                self.assertFalse(child.exists())
            finally:
                if handle is not None: api.CloseHandle(handle)


if __name__ == '__main__':
    unittest.main()
