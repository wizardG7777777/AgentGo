import hashlib
import json
import os
from pathlib import Path
import tempfile
import subprocess
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch
import zipfile

import native_harness
import rejudge
import runner


class NativePatchTest(unittest.TestCase):
    def test_large_literal_selection_exports_only_production_changes(self):
        with tempfile.TemporaryDirectory() as directory:
            root, out = Path(directory) / 'repo', Path(directory) / 'reports'
            root.mkdir()
            out.mkdir()
            def git(*args):
                return runner.run_command(['git', *args], cwd=root)
            git('init', '-q')
            paths = ['source_' + 'long_name_' * 7 + f'{number:04d}.py' for number in range(500)]
            paths += ['with space.py', 'literal[ab].py', 'literala.py', 'literalb.py', 'unicode_测试.py']
            for name in paths:
                (root / name).write_bytes(b'before\n')
            (root / 'tests').mkdir()
            (root / 'tests/test_source.py').write_bytes(b'protected\n')
            payload = b''.join(name.encode('utf-8') + b'\0' for name in paths + ['tests/test_source.py'])
            runner.run_command(['git', '--literal-pathspecs', 'add', '--pathspec-from-file=-', '--pathspec-file-nul'],
                               cwd=root, input_data=payload)
            git('-c', 'user.name=fixture', '-c', 'user.email=fixture@invalid.local', 'commit', '-qm', 'baseline')
            for name in ('with space.py', 'literal[ab].py', 'unicode_测试.py'):
                (root / name).write_bytes(b'after\n')
            (root / paths[0]).unlink()
            (root / 'new.py').write_bytes(b'new\n')
            (root / 'tests/test_source.py').write_bytes(b'changed test\n')
            git('add', '--', 'tests/test_source.py')
            old_command = ['git', 'add', '-A', '--', *paths]
            self.assertGreater(len(subprocess.list2cmdline(old_command)), 32767)
            if os.name == 'nt':
                with self.assertRaises(OSError) as old_failure:
                    runner.run_command(old_command, cwd=root)
                self.assertEqual(old_failure.exception.winerror, 206)
            patch_data, ignored = native_harness.capture_source_patch(root, out, runner)
            staged = set(git('diff', '--cached', '--name-only', '-z').stdout.decode('utf-8').split('\0')) - {''}
            self.assertEqual(staged, {paths[0], 'with space.py', 'literal[ab].py', 'unicode_测试.py', 'new.py'})
            self.assertNotIn(b'tests/test_source.py', patch_data)
            self.assertFalse(ignored)

    def test_replay_dependency_lock_precedes_installation_and_is_separate_from_normal_qualification(self):
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            repo = base / 'upstream'
            repo.mkdir()
            def git(*args):
                return runner.run_command(['git', *args], cwd=repo)
            git('init', '-q')
            (repo / 'Cargo.toml').write_text('[package]\nname="fixture"\nversion="0.1.0"\n')
            git('add', '--', 'Cargo.toml')
            git('-c', 'user.name=fixture', '-c', 'user.email=fixture@invalid.local', 'commit', '-qm', 'baseline')
            head = git('rev-parse', 'HEAD').stdout.decode().strip()
            task = SimpleNamespace(task_id='task', metadata={'base_sha': head})
            root = base / 'worktree'
            config = SimpleNamespace(repo=repo, testbed=base / 'testbed',
                suite=SimpleNamespace(options={'driver': 'cargo'}), worktree=lambda _: root,
                _frozen_dependency_files={'Cargo.lock': b'original lock\n'})
            normal = SimpleNamespace(testbed=config.testbed)
            self.assertNotEqual(native_harness.qualification_dir(config, task),
                native_harness.qualification_dir(normal, task))
            prepared = []
            driver = SimpleNamespace(prepare=lambda: prepared.append((root / 'Cargo.lock').read_bytes()), identity=lambda: {})
            api = SimpleNamespace(run_command=runner.run_command,
                safe_remove_worktree=lambda _config, path: self.assertFalse(path.exists()))
            with patch.object(native_harness.native_drivers, 'make', return_value=driver):
                native_harness.snapshot(config, task, api)
            self.assertEqual(prepared, [b'original lock\n'])
            cargo = object.__new__(native_harness.native_drivers.CargoDriver)
            cargo.config = config
            cargo.prepare_existing = Mock()
            cargo.command = Mock()
            cargo.identity = Mock()
            cargo.prepare()
            self.assertEqual(cargo.command.call_args.args[0], ['cargo', 'fetch', '--locked'])

    def test_ignored_scratch_does_not_break_patch_or_disappear_from_evidence(self):
        with tempfile.TemporaryDirectory() as directory:
            root, out = Path(directory) / 'repo', Path(directory) / 'reports'
            root.mkdir()
            out.mkdir()
            def git(*args):
                return runner.run_command(['git', *args], cwd=root)
            git('init', '-q')
            (root / '.gitignore').write_text('target/\n')
            (root / 'keep.rs').write_text('before\n')
            (root / 'delete.rs').write_text('delete\n')
            (root / 'tests').mkdir()
            (root / 'tests/test.rs').write_text('frozen\n')
            git('add', '--', '.gitignore', 'keep.rs', 'delete.rs', 'tests/test.rs')
            git('-c', 'user.name=fixture', '-c', 'user.email=fixture@invalid.local', 'commit', '-qm', 'baseline')
            (root / 'keep.rs').write_text('after\n')
            (root / 'delete.rs').unlink()
            (root / 'new.rs').write_text('added\n')
            (root / 'tests/test.rs').write_text('modified test\n')
            (root / 'target').mkdir()
            artifact = root / 'target/_scratch.rs'
            artifact.write_text('scratch evidence\n')
            before = native_harness.identity(root)
            patch, ignored = native_harness.capture_source_patch(root, out, runner)
            self.assertEqual(before, native_harness.identity(root))
            text = patch.decode()
            for name in ('keep.rs', 'delete.rs', 'new.rs'):
                self.assertIn(name, text)
            self.assertNotIn('_scratch.rs', text)
            self.assertNotIn('tests/test.rs', text)
            self.assertEqual(ignored, {'target/_scratch.rs': hashlib.sha256(artifact.read_bytes()).hexdigest()})
            staged = git('diff', '--cached', '--name-only').stdout.decode().splitlines()
            self.assertEqual(set(staged), {'keep.rs', 'delete.rs', 'new.rs'})
            self.assertTrue(json.loads((out / 'ignored_source_artifacts.json').read_text())['retained_in_input_identity'])

            archive = out / 'evidence.zip'
            with zipfile.ZipFile(archive, 'w') as zipped:
                zipped.writestr('worktree/target/_scratch.rs', artifact.read_bytes())
            evidence = {'ignored_sources': json.loads((out / 'ignored_source_artifacts.json').read_text()),
                'manifest_path': out / 'manifest.json', 'manifest': {'archive': archive.name,
                    'files': [{'path': 'worktree/target/_scratch.rs', 'kind': 'file', 'sha256': ignored['target/_scratch.rs']}]}}
            artifact.unlink()
            self.assertEqual(rejudge.restore_ignored_source_inputs(root, evidence, runner), ['target/_scratch.rs'])
            self.assertEqual(hashlib.sha256(artifact.read_bytes()).hexdigest(), ignored['target/_scratch.rs'])
            evidence['ignored_sources']['files'] = {'tests/test.rs': 'forged'}
            with self.assertRaisesRegex(ValueError, 'protected'):
                rejudge.restore_ignored_source_inputs(root, evidence, runner)


if __name__ == '__main__':
    unittest.main()
