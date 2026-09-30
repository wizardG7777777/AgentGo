"""Permission lifetimes: deterministic broker and native ordinary-user children."""
import collections
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

import firewall_windows as firewall
import processes
import storage
from storage_test import small_policy


class Broker:
    def __init__(self, root):
        self.active = {}
        self.rules = collections.Counter()
        self.acquired = self.released = self.peak = 0
        self.fail_acquire = None
        self.fail_release = False
        self.on_release = lambda: None
        self.session = firewall.Session.__new__(firewall.Session)
        self.session.data = {'session_id': 'fixture', 'roots': [str(root)],
                             'programs': [sys.executable, getattr(sys, '_base_executable', sys.executable)]}
        self.session.path = Path(root) / 'fixture-session.json'
        self.session.check = mock.Mock()
        self.session.check_dialogs = mock.Mock()
        self.session.request = self.request

    def request(self, action, **values):
        if action == 'acquire':
            if len(self.active) >= 256 or self.acquired + 1 == self.fail_acquire:
                raise storage.StorageError('firewall_permission_failed', 'lease_limit')
            self.active[values['lease_id']] = values['program']
            self.rules[values['program']] += 1
            self.acquired += 1
            self.peak = max(self.peak, len(self.active))
            return {'lease_id': values['lease_id'], 'rule_name': 'fixture'}
        if action == 'release':
            self.on_release()
            if self.fail_release:
                raise storage.StorageError('firewall_broker_timeout')
            program = self.active.pop(values['lease_id'], None)
            if program is not None:
                self.rules[program] -= 1
                if not self.rules[program]: del self.rules[program]
                self.released += 1
            return {'ok': True}
        raise AssertionError(action)


class LeaseScopeTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.broker = Broker(self.root)

    def test_many_distinct_serial_executables_keep_one_active_lease(self):
        for index in range(300):
            exe = self.root / f'fixture-{index}.exe'
            exe.write_bytes(b'fixture, never executed')
            group = firewall.ProgramLeases(self.broker.session, 'run')
            group.prepare([exe, exe])  # Duplicate CTest entries share one command lease.
            self.assertEqual(len(self.broker.active), 1)
            group.release(quiescent=True)
        self.assertEqual((self.broker.acquired, self.broker.released, self.broker.peak), (300, 300, 1))

    def test_python_redirector_and_base_are_both_tracked(self):
        base, venv = self.root / 'base', self.root / 'venv'
        base.mkdir(); (venv / 'Scripts').mkdir(parents=True)
        (base / 'python.exe').touch(); (venv / 'Scripts/python.exe').touch()
        (venv / 'pyvenv.cfg').write_text('home = ' + str(base), encoding='utf-8')
        group = firewall.ProgramLeases(self.broker.session, 'run')
        group.prepare([venv / 'Scripts/python.exe', base / 'python.exe'])
        self.assertEqual(len(group.entries), 2)
        group.release(quiescent=True)
        self.assertFalse(self.broker.active)

    def test_shared_program_rule_survives_until_last_owner_releases(self):
        exe = self.root / 'test.exe'; exe.touch()
        a, b = (firewall.ProgramLeases(self.broker.session, 'run') for _ in range(2))
        a.prepare([exe]); b.prepare([exe])
        a.release(quiescent=True)
        self.assertEqual(len(self.broker.active), 1)
        self.assertEqual(len(self.broker.rules), 1)
        a.release(quiescent=True)
        self.assertEqual(self.broker.released, 1)
        b.release(quiescent=True)
        self.assertFalse(self.broker.rules)

    def test_unknown_quiescence_keeps_permissions(self):
        exe = self.root / 'test.exe'; exe.touch()
        group = firewall.ProgramLeases(self.broker.session, 'run')
        group.prepare([exe])
        with self.assertRaisesRegex(storage.StorageError, 'process_quiescence_unknown'):
            group.release(quiescent=False)
        self.assertEqual(len(self.broker.active), 1)
        self.assertFalse(group.record()['complete'])
        group.release(quiescent=True)

    def test_actual_concurrent_limit_is_still_enforced(self):
        exe = self.root / 'test.exe'; exe.touch()
        groups = []
        try:
            for _ in range(256):
                g = firewall.ProgramLeases(self.broker.session, 'run'); g.prepare([exe]); groups.append(g)
            with self.assertRaisesRegex(storage.StorageError, 'lease_limit'):
                firewall.ProgramLeases(self.broker.session, 'run').prepare([exe])
        finally:
            for g in groups: g.release(quiescent=True)
        self.assertEqual(self.broker.peak, 256)

    def test_partial_acquisition_can_rollback_and_failure_is_not_complete(self):
        a, b = self.root / 'a.exe', self.root / 'b.exe'; a.touch(); b.touch()
        self.broker.fail_acquire = 2
        group = firewall.ProgramLeases(self.broker.session, 'run')
        with self.assertRaises(storage.StorageError): group.prepare([a, b])
        group.release(quiescent=True)
        self.assertFalse(self.broker.active)
        self.assertFalse(group.record()['acquisition_complete'])

    def test_release_failure_remains_explicit_and_is_not_retried(self):
        exe = self.root / 'test.exe'; exe.touch()
        group = firewall.ProgramLeases(self.broker.session, 'run'); group.prepare([exe])
        self.broker.fail_release = True
        with self.assertRaisesRegex(storage.StorageError, 'firewall_release_failed'): group.release(quiescent=True)
        self.assertEqual(len(self.broker.active), 1)
        self.assertFalse(group.record()['complete'])
        self.assertEqual(group.record()['leases'][0]['release_error'], 'firewall_broker_timeout')


@unittest.skipUnless(os.name == 'nt', 'native Windows command permissions')
class ManagedCommandLeaseTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(); self.addCleanup(self.temp.cleanup)
        self.root = storage.StorageRoot(self.temp.name, small_policy())
        self.attempt = storage.Attempt(self.root, self.root.batch({}), 'fixture', 'leases')
        self.broker = Broker(self.root.path)
        self.root.firewall = self.broker.session

    def assert_balanced(self):
        self.assertFalse(self.broker.active)
        self.assertEqual(self.broker.acquired, self.broker.released)
        for process in self.attempt.processes:
            self.assertTrue(process.record['quiescent'])
            self.assertTrue(process.record['firewall_permissions']['complete'])

    def test_270_real_serial_commands_release_before_the_next_command(self):
        binary = getattr(sys, '_base_executable', sys.executable)
        with self.attempt.activate():
            for _ in range(270):
                result = processes.run([binary, '-I', '-S', '-c', 'pass'], timeout=10)
                self.assertFalse(self.broker.active)
        self.assertEqual(self.broker.peak, 1)
        self.assert_balanced()

    def test_nonzero_and_timeout_release_after_process_exit(self):
        with self.attempt.activate():
            self.assertEqual(processes.run([sys.executable, '-c', 'raise SystemExit(7)'], check=False).returncode, 7)
            with self.assertRaisesRegex(storage.StorageError, 'command_timeout'):
                processes.run([sys.executable, '-c', 'import time;time.sleep(60)'], timeout=0.2)
        self.assert_balanced()

    def test_launch_failure_releases_all_acquired_permissions(self):
        original = subprocess.Popen
        def launch(argv, *args, **kwargs):
            if any(str(x).endswith('process_launcher.py') for x in argv):
                raise OSError('fixture launch failure')
            return original(argv, *args, **kwargs)
        with self.attempt.activate(), mock.patch.object(processes.subprocess, 'Popen', side_effect=launch):
            with self.assertRaisesRegex(OSError, 'fixture launch failure'):
                processes.run([sys.executable, '-c', 'pass'])
        self.assert_balanced()

    def test_partial_permission_failure_does_not_start_a_process(self):
        binary = getattr(sys, '_base_executable', sys.executable)
        extra = self.attempt.worktree / 'extra.exe'; extra.touch()
        self.broker.fail_acquire = 2
        original = subprocess.Popen
        child_starts = []
        def launch(argv, *args, **kwargs):
            if any(str(x).endswith('process_launcher.py') for x in argv):
                child_starts.append(True)
                raise AssertionError('subject started without complete permission')
            return original(argv, *args, **kwargs)
        with self.attempt.activate(), mock.patch.object(processes.subprocess, 'Popen', side_effect=launch):
            with self.assertRaisesRegex(storage.StorageError, 'lease_limit'):
                processes.run([binary, '-c', 'pass'], firewall_programs=[extra])
        self.assertEqual(child_starts, [])
        self.assertFalse(self.broker.active)
        self.assertTrue(self.attempt.processes[0].record['quiescent'])

    def test_overlapping_commands_do_not_remove_each_others_rules(self):
        with self.attempt.activate():
            a = processes.ManagedProcess(self.attempt, [sys.executable, '-c', 'pass'], cwd=self.attempt.worktree)
            b = processes.ManagedProcess(self.attempt, [sys.executable, '-c', 'import time;time.sleep(60)'], cwd=self.attempt.worktree)
            try:
                a.wait(timeout=10)
                self.assertIsNone(b.poll())
                self.assertTrue(self.broker.rules)
            finally:
                b.stop()
        self.assert_balanced()

    def test_descendants_are_stopped_before_releasing_permission(self):
        def release_check():
            for process in self.attempt.processes:
                for child in process._known.values():
                    try:
                        self.assertFalse(child.is_running() and child.status() != 'zombie')
                    except processes.process_library().NoSuchProcess:
                        pass
        self.broker.on_release = release_check
        code = "import subprocess,sys,time;subprocess.Popen([sys.executable,'-c','import time;time.sleep(60)']);time.sleep(0.5)"
        with self.attempt.activate(): processes.run([sys.executable, '-c', code], timeout=10)
        self.assert_balanced()

    def test_release_failure_prevents_successful_command_receipt(self):
        self.broker.fail_release = True
        with self.attempt.activate(), self.assertRaisesRegex(storage.StorageError, 'firewall_release_failed'):
            processes.run([sys.executable, '-c', 'pass'])
        record = self.attempt.processes[0].record
        self.assertTrue(record['quiescent'])
        self.assertFalse(record['output_complete'])
        self.assertFalse(record['firewall_permissions']['complete'])


if __name__ == '__main__': unittest.main()
