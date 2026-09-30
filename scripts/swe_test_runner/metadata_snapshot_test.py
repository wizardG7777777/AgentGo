"""Ownership snapshots and independent process-vs-filesystem completion facts."""
import errno
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import unittest
from unittest import mock

import processes
import scratch_scope
import storage
import storage_inventory
from storage_test import small_policy


class MetadataSnapshotTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = storage.StorageRoot(self.temp.name, small_policy())
        self.attempt = storage.Attempt(self.root, self.root.batch({}), 'fixture', 'metadata')
        self.addCleanup(self.attempt.finish_observation)

    def test_snapshot_is_detached_and_omits_unneeded_private_state(self):
        self.attempt.state['private_fixture_field'] = 'must-not-forward'
        snapshot = self.attempt.observation_snapshot()
        self.assertNotIn('must-not-forward', json.dumps(snapshot))
        snapshot['state']['identity']['inode'] += 1
        self.assertNotEqual(snapshot['state']['identity'], self.attempt.state['identity'])
        self.assertLess(len(json.dumps(snapshot).encode()), 65536)

    def test_live_collect_does_not_read_changing_attempt_file(self):
        snapshot = self.attempt.observation_snapshot()
        original = storage.read_json
        def read(path):
            if Path(path) == self.attempt.path / 'attempt.json':
                raise PermissionError(errno.EACCES, 'fixture concurrent replacement', str(path))
            return original(path)
        with mock.patch.object(storage, 'read_json', side_effect=read):
            value = storage_inventory.collect(self.root.path, self.attempt.path, 5, snapshot=snapshot)
            offline = storage_inventory.collect(self.root.path, self.attempt.path, 5)
        self.assertTrue(value['ok'], value)
        self.assertEqual(value['state_source'], 'supervisor_snapshot')
        self.assertFalse(offline['ok'])
        self.assertEqual(offline['error_operation'], 'read_attempt_metadata')
        self.assertEqual(offline['error_errno'], errno.EACCES)

    def test_snapshot_rejects_forged_identity_paths_and_policy(self):
        edits = [lambda x: x.update(schema='unknown'),
                 lambda x: x['state'].update(root_id='different-owner'),
                 lambda x: x['state']['identity'].update(inode=0),
                 lambda x: x['state'].update(attempt_id='a' * 32),
                 lambda x: x['state'].update(path=str(self.root.path / 'other-attempt')),
                 lambda x: x['state'].update(scratch=str(self.root.path.parent)),
                 lambda x: x['state']['policy'].update(cache_max_bytes=0)]
        for edit in edits:
            snapshot = self.attempt.observation_snapshot(); edit(snapshot)
            value = storage_inventory.collect(self.root.path, self.attempt.path, 5, snapshot=snapshot)
            self.assertFalse(value['ok'], value)

    def test_replaced_attempt_directory_is_rejected_even_with_valid_snapshot(self):
        snapshot = self.attempt.observation_snapshot()
        original = self.attempt.path
        moved = original.with_name(original.name + '-old')
        self.assertTrue(original.resolve().is_relative_to(self.root.path))
        self.assertTrue(moved.resolve().is_relative_to(self.root.path))
        original.rename(moved)
        original.mkdir()
        value = storage_inventory.collect(self.root.path, original, 5, snapshot=snapshot)
        self.assertFalse(value['ok'])
        self.assertEqual(value['error_code'], 'inventory_owner_mismatch')

    def test_snapshot_does_not_skip_root_marker_validation(self):
        marker = self.root.path / 'storage-root.json'
        data = storage.read_json(marker); data['root_id'] = 'different'
        storage.atomic_json(marker, data)
        value = storage_inventory.collect(self.root.path, self.attempt.path, 5,
                                          snapshot=self.attempt.observation_snapshot())
        self.assertFalse(value['ok'])
        self.assertEqual(value['error_code'], 'inventory_owner_mismatch')

    def test_real_worker_uses_request_snapshot_not_mutable_disk_json(self):
        original = subprocess.Popen
        path = self.attempt.path / 'attempt.json'
        def launch(argv, **kwargs):
            process = original(argv, **kwargs)
            communicate = process.communicate
            def send(input=None, **options):
                if input is None:
                    return communicate(input, **options)
                self.assertIn('snapshot', json.loads(input))
                saved = path.read_bytes()
                path.write_text('{fixture unfinished metadata', encoding='utf-8')
                try:
                    return communicate(input, **options)
                finally:
                    path.write_bytes(saved)
            process.communicate = send
            return process
        with mock.patch.object(storage_inventory.subprocess, 'Popen', side_effect=launch):
            value = storage_inventory.run_inventory(self.attempt)
        self.assertTrue(value['ok'], value)
        self.assertEqual(value['state_source'], 'supervisor_snapshot')
        self.assertTrue(self.attempt.state['inventory_scanner']['quiescent'])

    def test_real_worker_survives_concurrent_metadata_publication(self):
        stop = threading.Event(); errors = []; writes = []
        def writer():
            while not stop.wait(0.002):
                try:
                    self.attempt.persist(); writes.append(True)
                except BaseException as error:
                    errors.append(type(error).__name__); break
        thread = threading.Thread(target=writer); thread.start()
        try:
            for _ in range(12):
                value = storage_inventory.run_inventory(self.attempt, exact=False)
                self.assertTrue(value['ok'], value)
        finally:
            stop.set(); thread.join(10)
        self.assertFalse(thread.is_alive())
        self.assertFalse(errors, errors)
        self.assertTrue(writes)

    def test_live_worker_rejects_missing_or_null_snapshot(self):
        request = {'root': str(self.root.path), 'attempt': str(self.attempt.path), 'max_seconds': 5}
        for value in (request, dict(request, snapshot=None)):
            result = subprocess.run([getattr(sys, '_base_executable', sys.executable), '-I', '-S', '-B',
                                     str(Path(storage_inventory.__file__).resolve())],
                input=json.dumps(value).encode(), capture_output=True, check=True, timeout=10,
                **({'creationflags': subprocess.CREATE_NO_WINDOW} if os.name == 'nt' else {}))
            report = json.loads(result.stdout)
            self.assertFalse(report['ok'])
            self.assertEqual(report['blocked_section'], 'request')

    @unittest.skipUnless(os.name == 'nt', 'Windows recovery path')
    def test_scratch_failure_does_not_erase_proven_process_exit(self):
        failure = PermissionError(errno.EACCES, 'fixture metadata denied', str(self.attempt.path / 'attempt.json'))
        with self.attempt.activate(), mock.patch.object(scratch_scope, 'recover', side_effect=failure):
            with self.assertRaisesRegex(storage.StorageError, 'scratch_permission_recovery_failed'):
                processes.run([sys.executable, '-c', 'pass'], timeout=10)
        record = self.attempt.processes[-1].record
        self.assertEqual(record['exit_code'], 0)
        self.assertTrue(record['quiescent'])
        self.assertFalse(record['scratch_recovered'])
        self.assertFalse(record['output_complete'])
        self.assertEqual(record['scratch_recovery_error']['operation'], 'scratch.recover')
        self.assertEqual(record['scratch_recovery_error']['error_type'], 'PermissionError')
        self.assertEqual(record['scratch_recovery_error']['errno'], errno.EACCES)
        self.assertIsNone(record['scratch_recovery_error']['winerror'])
        self.assertNotIn('process_cleanup_error', record)

    @unittest.skipUnless(os.name == 'nt', 'Windows recovery path')
    def test_recovery_reads_are_synchronized_with_attempt_writer(self):
        original = scratch_scope.recover
        def recover(*args, **kwargs):
            # Another thread cannot enter the same publication lock while this
            # recovery path reads attempt.json. RLock reentrancy is not proof.
            acquired = []
            def contender():
                held = self.attempt._mutex.acquire(blocking=False)
                acquired.append(held)
                if held: self.attempt._mutex.release()
            thread = threading.Thread(target=contender); thread.start(); thread.join(5)
            self.assertEqual(acquired, [False])
            return original(*args, **kwargs)
        with self.attempt.activate(), mock.patch.object(scratch_scope, 'recover', side_effect=recover):
            processes.run([sys.executable, '-c', 'pass'], timeout=10)
        self.assertTrue(self.attempt.processes[-1].record['scratch_recovered'])

    @unittest.skipUnless(os.name == 'nt', 'Windows Job evidence')
    def test_unknown_job_state_still_stops_and_preserves_original_details(self):
        with self.attempt.activate():
            process = processes.ManagedProcess(self.attempt, [sys.executable, '-c', 'pass'], cwd=self.attempt.worktree)
            process.popen.wait(timeout=10)
            job = process.job
            failure = storage.StorageError('process_quiescence_unknown', 'fixture native query')
            failure.operation, failure.winerror = 'QueryInformationJobObject', 5
            failure.__cause__ = PermissionError(errno.EACCES, 'fixture cause')
            try:
                with mock.patch.object(job, 'active', side_effect=failure), mock.patch.object(scratch_scope, 'recover') as recover:
                    process.finish()
                recover.assert_not_called()
                with self.assertRaisesRegex(storage.StorageError, 'process_quiescence_unknown'):
                    process.raise_if_failed()
                record = process.record
                self.assertFalse(record['quiescent'])
                self.assertEqual(record['process_cleanup_error']['operation'], 'QueryInformationJobObject')
                self.assertEqual(record['process_cleanup_error']['winerror'], 5)
                self.assertEqual(record['process_cleanup_error']['causes'][0]['error_type'], 'PermissionError')
            finally:
                job.close(); process.job = None

    @unittest.skipUnless(os.name == 'nt', 'Windows native error capture')
    def test_native_job_failure_retains_winerror(self):
        import ctypes
        import process_windows
        job = process_windows.Job.__new__(process_windows.Job)
        job.handle = 1
        job.api = mock.Mock()
        job.api.QueryInformationJobObject.return_value = 0
        job.api.TerminateJobObject.return_value = 0
        for action, name in ((job.active, 'QueryInformationJobObject'), (job.terminate, 'TerminateJobObject')):
            ctypes.set_last_error(5)
            with self.assertRaises(storage.StorageError) as caught: action()
            self.assertEqual(caught.exception.operation, name)
            self.assertEqual(caught.exception.winerror, 5)


if __name__ == '__main__': unittest.main()
