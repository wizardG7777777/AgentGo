import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import native_windows_links
import storage


class NativeWindowsLinksTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix='swe-links-')
        self.addCleanup(temporary.cleanup)
        self.area = Path(temporary.name)
        self.root = self.area / 'worktree'
        self.root.mkdir()

    def junction(self, path, target):
        path.parent.mkdir(parents=True, exist_ok=True)
        env = os.environ.copy()
        env['SWE_TEST_LINK'] = str(path)
        env['SWE_TEST_LINK_TARGET'] = str(target)
        subprocess.run(['powershell', '-NoProfile', '-NonInteractive', '-Command',
            "$ErrorActionPreference='Stop'; New-Item -ItemType Junction -Path $env:SWE_TEST_LINK -Target $env:SWE_TEST_LINK_TARGET | Out-Null"],
            env=env, capture_output=True, check=True, timeout=10,
            creationflags=getattr(subprocess, 'CREATE_NO_WINDOW', 0))
        self.assertTrue(path.is_junction())

    @unittest.skipUnless(os.name == 'nt', 'Windows directory junctions')
    def test_links_relocate_with_candidate_and_cycles_do_not_copy_data(self):
        package = self.root / 'packages' / 'subject'
        package.mkdir(parents=True)
        (package / 'source.js').write_text('baseline')
        link = self.root / 'node_modules' / 'subject'
        self.junction(link, package)
        self.junction(package / 'node_modules' / 'self', package)
        first = native_windows_links.normalize_node_junctions(self.root)
        self.assertEqual(first['converted_junctions'], 2)
        self.assertTrue(link.is_symlink())
        self.assertFalse(link.is_junction())
        self.assertFalse(Path(os.readlink(link)).is_absolute())
        second = native_windows_links.normalize_node_junctions(self.root)
        self.assertEqual(second['converted_junctions'], 0)
        self.assertEqual(first['mapping_sha256'], second['mapping_sha256'])
        candidate = self.area / 'candidate'
        shutil.copytree(self.root, candidate, symlinks=True)
        (candidate / 'packages' / 'subject' / 'source.js').write_text('candidate')
        self.assertEqual((candidate / 'node_modules' / 'subject' / 'source.js').read_text(), 'candidate')
        self.assertEqual((candidate / 'packages' / 'subject' / 'node_modules' / 'self' / 'source.js').read_text(), 'candidate')
        self.assertEqual((package / 'source.js').read_text(), 'baseline')

    @unittest.skipUnless(os.name == 'nt', 'Windows directory junctions')
    def test_outside_target_rejects_whole_plan_without_modification(self):
        inside, outside = self.root / 'package', self.area / 'external'
        inside.mkdir()
        outside.mkdir()
        (outside / 'evidence').write_text('retain')
        good, bad = self.root / 'node_modules' / 'a', self.root / 'node_modules' / 'z'
        self.junction(good, inside)
        self.junction(bad, outside)
        with self.assertRaisesRegex(storage.StorageError, 'target_outside_worktree'):
            native_windows_links.normalize_node_junctions(self.root)
        self.assertTrue(good.is_junction())
        self.assertTrue(bad.is_junction())
        self.assertEqual((outside / 'evidence').read_text(), 'retain')

    @unittest.skipUnless(os.name == 'nt', 'Windows directory junctions')
    def test_missing_symlink_permission_keeps_original_junction(self):
        target = self.root / 'package'
        target.mkdir()
        link = self.root / 'node_modules' / 'package'
        self.junction(link, target)
        with patch.object(Path, 'symlink_to', side_effect=PermissionError('fixture')):
            with self.assertRaisesRegex(storage.StorageError, 'symlink_permission_unavailable'):
                native_windows_links.normalize_node_junctions(self.root)
        self.assertTrue(link.is_junction())
        self.assertTrue(target.is_dir())

    @unittest.skipIf(os.name == 'nt', 'POSIX unchanged')
    def test_posix_does_not_change_dependency_links(self):
        target = self.root / 'package'
        target.mkdir()
        link = self.root / 'node_modules' / 'package'
        link.parent.mkdir()
        link.symlink_to('../package', target_is_directory=True)
        before = os.readlink(link)
        self.assertEqual(native_windows_links.normalize_node_junctions(self.root),
                         {'mode': 'native', 'converted_junctions': 0})
        self.assertEqual(os.readlink(link), before)


if __name__ == '__main__':
    unittest.main()
