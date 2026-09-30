import json
import os
from pathlib import Path
import stat
import sys
import tempfile
import time
import unittest

import artifacts
import cleanup
import processes
import scratch_scope
import scoped_exec
import storage
from storage_test import small_policy


class ScratchScopeTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = storage.StorageRoot(self.temp.name, small_policy())
        self.attempt = storage.Attempt(self.root, self.root.batch({}), 'fixture', 'scratch')

    @unittest.skipUnless(os.name == 'posix', 'POSIX permissions')
    def test_permission_semantics_survive_until_child_exit_then_archive_and_cleanup(self):
        if os.geteuid() == 0:
            self.skipTest('PermissionDenied requires ordinary user')
        code = '''import os,json,tempfile
from pathlib import Path
p=Path(tempfile.mkdtemp())/'baz';p.mkdir();(p/'data').write_text('evidence');p.chmod(0o244)
try: (p/'data').stat()
except PermissionError: denied=True
else: denied=False
print(json.dumps({'denied':denied,'path':str(p)}))
'''
        with self.attempt.activate():
            result = processes.run([sys.executable, '-c', code])
        data = json.loads(result.stdout)
        self.assertTrue(data['denied'])
        path = Path(data['path'])
        self.assertEqual((path / 'data').read_text(), 'evidence')
        record = storage.read_json(Path(result.command_ref).parent / 'scratch-permissions.json')
        self.assertTrue(record['complete'])
        self.assertEqual([(r['old_mode'], r['new_mode']) for r in record['changes']], [(0o244, 0o744)])
        manifest = artifacts.seal(self.attempt)
        self.assertTrue(artifacts.verify(manifest)['complete'])
        self.assertTrue(cleanup.apply(cleanup.plan(self.attempt, manifest))['complete'])
        self.assertFalse(self.attempt.scratch.exists())

    def test_commands_receive_different_owned_scratch(self):
        with self.attempt.activate():
            runs = [processes.run([sys.executable, '-c', 'import os;print(os.environ["TMPDIR"])']) for _ in range(2)]
        paths = [Path(run.stdout.decode().strip()) for run in runs]
        self.assertNotEqual(paths[0], paths[1])
        self.assertTrue(all(path.is_relative_to(self.attempt.scratch) for path in paths))

    @unittest.skipUnless(os.name == 'posix', 'Cargo scoped exec hook')
    def test_nested_test_hook_preserves_stdin_stdout_and_recovers_its_own_scratch(self):
        code = '''import os,sys,tempfile,json
from pathlib import Path
p=Path(tempfile.mkdtemp())/'blocked';p.mkdir();(p/'data').write_text('kept');p.chmod(0)
print(json.dumps({'stdin':sys.stdin.read(),'path':str(p)}))
'''
        with self.attempt.activate():
            result = processes.run([sys.executable, str(Path(scoped_exec.__file__)), sys.executable, '-c', code], input_data=b'input')
        data = json.loads(result.stdout)
        self.assertEqual(data['stdin'], 'input')
        self.assertEqual((Path(data['path']) / 'data').read_text(), 'kept')
        controls = list((self.attempt.path / 'exec-scopes').glob('*/exec.json'))
        self.assertEqual(len(controls), 1)
        record = storage.read_json(controls[0])
        self.assertEqual(record['output_owner'], 'caller')
        self.assertTrue(record['quiescent'])
        self.assertFalse(list(controls[0].parent.glob('*.log')))
        self.assertTrue(storage.read_json(controls[0].parent / 'scratch-permissions.json')['complete'])

    @unittest.skipUnless(os.name == 'posix', 'POSIX permission window')
    def test_live_permissions_are_untouched_and_incomplete_sample_is_not_a_complete_count(self):
        if os.geteuid() == 0:
            self.skipTest('PermissionDenied requires ordinary user')
        ready, release = self.attempt.reports / 'ready.json', self.attempt.reports / 'release'
        code = '''import json,sys,tempfile,time
from pathlib import Path
p=Path(tempfile.mkdtemp())/'blocked';p.mkdir();(p/'data').write_text('data');p.chmod(0o244)
Path(sys.argv[1]).write_text(json.dumps({'path':str(p)}))
while not Path(sys.argv[2]).exists():time.sleep(.02)
'''
        with self.attempt.activate():
            child = processes.ManagedProcess(self.attempt, [sys.executable, '-c', code, str(ready), str(release)], cwd=self.attempt.worktree)
            try:
                deadline = time.monotonic() + 5
                while not ready.exists() and time.monotonic() < deadline:
                    time.sleep(.02)
                self.assertTrue(ready.exists())
                target = Path(storage.read_json(ready)['path'])
                self.assertIsNone(child.poll())
                self.attempt._inventory_scan()
                self.assertTrue(self.attempt.state['inventory_incomplete'])
                self.assertEqual(stat.S_IMODE(target.stat().st_mode), 0o244)
                release.touch()
                child.wait(timeout=5)
                self.assertEqual(stat.S_IMODE(target.stat().st_mode), 0o744)
                self.attempt._inventory_scan(final=True)
                self.assertFalse(self.attempt.state['inventory_incomplete'])
            finally:
                child.stop()

    @unittest.skipUnless(os.name == 'posix', 'POSIX timeout cleanup')
    def test_timeout_recovers_only_after_process_tree_stops(self):
        code = "import tempfile,time;from pathlib import Path;p=Path(tempfile.mkdtemp())/'blocked';p.mkdir();(p/'data').write_text('data');p.chmod(0);time.sleep(30)"
        with self.attempt.activate():
            with self.assertRaisesRegex(storage.StorageError, 'command_timeout'):
                processes.run([sys.executable, '-c', code], timeout=.5)
        process = self.attempt.processes[0]
        self.assertIsNotNone(process.poll())
        self.assertTrue(process.record['quiescent'])
        receipt = storage.read_json(process.directory / 'scratch-permissions.json')
        self.assertTrue(receipt['complete'])
        self.assertTrue(any(change['old_mode'] == 0 and change['new_mode'] == 0o700 for change in receipt['changes']))

    def test_recovery_rejects_unknown_quiescence_and_changed_identity(self):
        control = self.attempt.path / 'fixture-control'
        control.mkdir()
        scope, env = scratch_scope.create(self.attempt, control, {})
        with self.assertRaisesRegex(storage.StorageError, 'quiescence'):
            scratch_scope.recover(scope, {'quiescent': False})
        data = storage.read_json(scope)
        data['identity']['inode'] += 1
        storage.atomic_json(scope, data)
        with self.assertRaisesRegex(storage.StorageError, 'identity_changed'):
            scratch_scope.recover(scope, {'quiescent': True})

    @unittest.skipUnless(os.name == 'posix', 'POSIX link boundaries')
    def test_external_link_untouched_and_unreadable_hardlink_rejected(self):
        control = self.attempt.path / 'fixture-control'
        control.mkdir()
        scope, env = scratch_scope.create(self.attempt, control, {})
        target = Path(env['TMPDIR'])
        external = self.root.path / 'outside'
        external.write_text('outside')
        external.chmod(0o200)
        self.addCleanup(lambda: external.chmod(0o600))
        (target / 'link').symlink_to(external)
        scratch_scope.recover(scope, {'quiescent': True})
        self.assertEqual(stat.S_IMODE(external.stat().st_mode), 0o200)
        os.link(external, target / 'hardlink')
        with self.assertRaisesRegex(storage.StorageError, 'hardlink'):
            scratch_scope.recover(scope, {'quiescent': True})
        self.assertEqual(stat.S_IMODE(external.stat().st_mode), 0o200)


if __name__ == '__main__':
    unittest.main()
