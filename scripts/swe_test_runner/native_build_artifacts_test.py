from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest

import native_build_artifacts as builds
import native_harness
import runner


class CMakeArtifactTest(unittest.TestCase):
    def cache(self, root, build='build-other'):
        return (f'CMAKE_HOME_DIRECTORY:INTERNAL={root}\n'
                f'CMAKE_CACHEFILE_DIR:INTERNAL={root / build}\n').encode('utf-8')

    def test_requires_matching_cache_and_absence_of_original_inputs(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            files = {'build-other/test/Makefile': 'make', 'build-other/test/new_test.cc': 'test',
                     'build-other/test/CMakeFiles/std-test.dir/build.make': 'rules',
                     'build-other/CMakeFiles/3.22.1/CompilerIdCXX/CMakeCXXCompilerId.cpp': 'compiler',
                     'test/Makefile': 'source'}
            caches = {'build-other/CMakeCache.txt': self.cache(root)}
            result = builds.classify(files, caches, root, {'test/Makefile'})
            self.assertEqual(set(result['files']), set(files) - {'test/Makefile', 'build-other/test/new_test.cc'})
            for invalid in ({}, {'build-other/CMakeCache.txt': self.cache(root.parent / 'outside')},
                            {'build-other/CMakeCache.txt': self.cache(root, 'another')},
                            {'build-other/CMakeCache.txt': b'not CMake metadata'}):
                with self.subTest(caches=invalid):
                    self.assertFalse(builds.classify(files, invalid, root, {})['files'])
            self.assertFalse(builds.classify(files, caches, root, {'build-other/source.cc'})['files'])

    def test_generated_outputs_remain_evidence_but_real_and_declared_tests_stay_protected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            runner.run_command(['git', 'init', '-q'], cwd=root)
            (root / '.gitignore').write_text('CMakeFiles/\nCMakeCache.txt\nMakefile\n', encoding='utf-8')
            (root / 'test').mkdir()
            (root / 'test/real.cc').write_text('real test', encoding='utf-8')
            (root / 'build-other/test').mkdir(parents=True)
            (root / 'build-other/CMakeCache.txt').write_bytes(self.cache(root))
            (root / 'build-other/test/Makefile').write_text('generated make', encoding='utf-8')
            (root / 'build-other/test/new_test.cc').write_text('unrecognized real test', encoding='utf-8')
            (root / 'build-other/test/temp').write_text('generated runtime data', encoding='utf-8')
            inventory = native_harness.identity(root)
            self.assertIn('build-other/test/Makefile', inventory['files'])
            self.assertIn('build-other/test/Makefile', inventory['generated_build_artifacts']['files'])
            self.assertIn('build-other/test/temp', inventory['generated_build_artifacts']['files'])
            task = SimpleNamespace(test_files=('test/real.cc',))
            protected = native_harness.protected(root, task)
            self.assertNotIn('build-other/test/Makefile', protected)
            self.assertNotIn('build-other/test/temp', protected)
            self.assertIn('test/real.cc', protected)
            self.assertIn('build-other/test/new_test.cc', protected)
            task.test_files += ('build-other/test/Makefile',)
            self.assertIn('build-other/test/Makefile', native_harness.protected(root, task))


if __name__ == '__main__':
    unittest.main()
