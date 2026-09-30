import copy
import hashlib
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest

import native_test_adaptations as adaptations


class NativeTestAdaptationsTest(unittest.TestCase):
    def fixture(self, root):
        path = root / 'tests' / 'regression.rs'
        path.parent.mkdir()
        path.write_bytes(b'#[cfg(not(windows))]\nassert_original();\n')
        after = b'assert_original();\n'
        item = {'platform': 'win32', 'path': 'tests/regression.rs',
            'upstream_sha256': hashlib.sha256(path.read_bytes()).hexdigest(),
            'adapted_sha256': hashlib.sha256(after).hexdigest(),
            'replacements': [{'old': '#[cfg(not(windows))]\n', 'new': ''}]}
        task = SimpleNamespace(metadata={'test_files': ['tests/regression.rs'],
            'source_files': ['src/lib.rs'], 'test_adaptations': [item],
            'required_regression_tests': ['integration::regression::feature']})
        return task, path, after

    def test_platform_selection_pinned_content_and_repeat_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            task, path, after = self.fixture(root)
            original = path.read_bytes()
            self.assertEqual(adaptations.apply(root, task, 'linux'), [])
            self.assertEqual(path.read_bytes(), original)
            receipt = adaptations.apply(root, task, 'win32')
            self.assertEqual(path.read_bytes(), after)
            self.assertEqual(receipt[0]['adapted_sha256'], hashlib.sha256(after).hexdigest())
            with self.assertRaisesRegex(ValueError, 'upstream content'):
                adaptations.apply(root, task, 'win32')

    def test_changed_content_and_output_hash_rejected_without_writes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            task, path, _ = self.fixture(root)
            original = path.read_bytes()
            task.metadata['test_adaptations'][0]['adapted_sha256'] = '0' * 64
            with self.assertRaisesRegex(ValueError, 'output identity'):
                adaptations.apply(root, task, 'win32')
            self.assertEqual(path.read_bytes(), original)
            path.write_bytes(original + b'changed\n')
            with self.assertRaisesRegex(ValueError, 'upstream content'):
                adaptations.apply(root, task, 'win32')

    def test_crlf_checkout_uses_canonical_pin_and_retains_physical_identity(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            task, path, after = self.fixture(root)
            physical = path.read_bytes().replace(b'\n', b'\r\n')
            path.write_bytes(physical)
            receipt = adaptations.apply(root, task, 'win32')[0]
            self.assertEqual(path.read_bytes(), after)
            self.assertEqual(receipt['physical_input_sha256'], hashlib.sha256(physical).hexdigest())
            self.assertEqual(receipt['line_endings'], 'crlf-to-lf')

    def test_all_files_validated_before_mutation(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            task, path, _ = self.fixture(root)
            original = path.read_bytes()
            other = path.with_name('other.rs')
            other.write_bytes(b'unexpected')
            task.metadata['test_files'].append('tests/other.rs')
            second = copy.deepcopy(task.metadata['test_adaptations'][0])
            second['path'] = 'tests/other.rs'
            task.metadata['test_adaptations'].append(second)
            with self.assertRaisesRegex(ValueError, 'upstream content'):
                adaptations.apply(root, task, 'win32')
            self.assertEqual(path.read_bytes(), original)

    def test_source_paths_unknown_platform_duplicates_and_bad_required_ids_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            task, _, _ = self.fixture(Path(directory))
            for field, value in [('path', 'src/lib.rs'), ('path', '../escape'), ('platform', 'unknown')]:
                bad = copy.deepcopy(task.metadata)
                bad['test_adaptations'][0][field] = value
                with self.assertRaises(ValueError):
                    adaptations.validate(bad)
            bad = copy.deepcopy(task.metadata)
            bad['test_adaptations'] *= 2
            with self.assertRaises(ValueError):
                adaptations.validate(bad)
            for value in ([{}], ['same', 'same'], [''], 'test'):
                bad['required_regression_tests'] = value
                with self.assertRaises(ValueError):
                    adaptations.validate(bad)

    def test_symlink_cannot_redirect_a_declared_test(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            task, path, _ = self.fixture(root)
            original = path.read_bytes()
            real = root / 'real.rs'
            real.write_bytes(original)
            path.unlink()
            try:
                path.symlink_to(real)
            except OSError:
                self.skipTest('symlink unavailable')
            with self.assertRaisesRegex(ValueError, 'links'):
                adaptations.apply(root, task, 'win32')
            self.assertEqual(real.read_bytes(), original)

    def test_absent_skipped_and_unrelated_failures_cannot_qualify(self):
        name = 'integration::regression::feature'
        task = SimpleNamespace(metadata={'required_regression_tests': [name]})
        absent = {'collected_nodeids': ['other'], 'case_outcomes': {'other': 'fail'}}
        adaptations.check_collection(task, absent, 'clean')
        with self.assertRaisesRegex(ValueError, 'not collected'):
            adaptations.check_collection(task, absent, 'red')
        skipped = {'collected_nodeids': [name], 'case_outcomes': {name: 'skip'}}
        with self.assertRaisesRegex(ValueError, 'not executed'):
            adaptations.check_collection(task, skipped, 'judge')
        passed = {'collected_nodeids': [name], 'case_outcomes': {name: 'pass'}}
        with self.assertRaisesRegex(ValueError, 'did not fail'):
            adaptations.check_collection(task, passed, 'red')
        adaptations.check_collection(task, passed, 'green')
        failed = {'collected_nodeids': [name], 'case_outcomes': {name: 'fail'}}
        adaptations.check_collection(task, failed, 'red')
        adaptations.check_collection(task, failed, 'judge')  # Judge records a real regression failure.


if __name__ == '__main__':
    unittest.main()
