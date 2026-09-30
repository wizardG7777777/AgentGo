import copy
import json
from pathlib import Path
import subprocess
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import native_drivers


class CargoExecutionTest(unittest.TestCase):
    def fixture(self, root):
        root = root.resolve()
        packages, artifacts = [], []
        for name in ('alpha', 'beta'):
            package = root / 'members' / name
            package.mkdir(parents=True)
            manifest = package / 'Cargo.toml'
            manifest.write_text(f'[package]\nname="{name}"\n', encoding='utf-8')
            (package / 'fixture.txt').write_text(name, encoding='utf-8')
            identity = 'fixture:' + name
            packages.append({'id': identity, 'name': name, 'manifest_path': str(manifest)})
            artifacts.append({'reason': 'compiler-artifact', 'package_id': identity,
                'manifest_path': str(manifest), 'profile': {'test': True},
                'target': {'name': name}, 'executable': str(root / 'build' / name)})
        (root / 'fixture.txt').write_text('workspace, not a package fixture', encoding='utf-8')
        driver = object.__new__(native_drivers.CargoDriver)
        driver.root, driver.options = root, {}
        driver.task = SimpleNamespace(metadata={})
        return driver, {'workspace_root': str(root), 'packages': packages}, artifacts

    def test_test_binaries_read_each_owning_packages_relative_files(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            driver, metadata, artifacts = self.fixture(root)
            observed = []

            def command(argv, **options):
                if argv[:2] == ['cargo', 'test']:
                    output = '\n'.join(json.dumps(item) for item in artifacts).encode()
                elif argv[:2] == ['cargo', 'metadata']:
                    output = json.dumps(metadata).encode()
                else:
                    cwd = Path(options.get('cwd', root))
                    name = Path(argv[0]).name
                    observed.append((name, argv[1], cwd))
                    if argv[1] == '--list':
                        output = b'loads_package_fixture: test\n'
                    else:
                        passed = (cwd / 'fixture.txt').read_text() == name
                        output = (f'test loads_package_fixture ... {"ok" if passed else "FAILED"}\n'
                            f'test result: {"ok" if passed else "FAILED"}. {int(passed)} passed; {int(not passed)} failed; 0 ignored;\n').encode()
                        return subprocess.CompletedProcess(argv, 0 if passed else 1, output, b'')
                return subprocess.CompletedProcess(argv, 0, output, b'')

            driver.command = command
            output = root / 'reports'
            output.mkdir()
            with patch.object(native_drivers.managed_processes, 'save_output_reference'):
                report, _ = driver.test(False, output)
            self.assertEqual((report['passed'], report['failed']), (2, 0))
            self.assertEqual(len(observed), 4)
            self.assertTrue(all(cwd == root / 'members' / name for name, _, cwd in observed))
            proof = json.loads((output / 'cargo-execution.json').read_text())
            self.assertEqual(proof['working_directory_semantics'], 'cargo-package-root/v1')
            self.assertEqual([item['package_id'] for item in proof['artifacts']], ['fixture:alpha', 'fixture:beta'])

    def test_unknown_mismatched_and_outside_manifests_are_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / 'checkout'
            root.mkdir()
            driver, metadata, artifacts = self.fixture(root)
            unknown = copy.deepcopy(artifacts)
            unknown[0]['package_id'] = 'unknown'
            with self.assertRaisesRegex(ValueError, 'matching package'):
                driver.execution_directories(unknown, metadata)
            mismatched = copy.deepcopy(artifacts)
            mismatched[0]['manifest_path'] = artifacts[1]['manifest_path']
            with self.assertRaisesRegex(ValueError, 'mismatched'):
                driver.execution_directories(mismatched, metadata)
            outside = Path(directory) / 'Cargo.toml'
            outside.write_text('[package]\nname="outside"\n', encoding='utf-8')
            escaped = copy.deepcopy(metadata)
            escaped['packages'][0]['manifest_path'] = str(outside)
            outside_artifact = copy.deepcopy(artifacts)
            outside_artifact[0]['manifest_path'] = str(outside)
            with self.assertRaisesRegex(ValueError, 'outside'):
                driver.execution_directories(outside_artifact, escaped)
            duplicated = copy.deepcopy(metadata)
            duplicated['packages'].append(duplicated['packages'][0])
            with self.assertRaisesRegex(ValueError, 'repeats'):
                driver.execution_directories(artifacts, duplicated)

    def test_manifest_symlink_cannot_move_execution_outside_checkout(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / 'checkout'
            root.mkdir()
            driver, metadata, artifacts = self.fixture(root)
            outside = Path(directory) / 'Cargo.toml'
            outside.write_text('outside', encoding='utf-8')
            manifest = Path(metadata['packages'][0]['manifest_path'])
            manifest.unlink()
            try:
                manifest.symlink_to(outside)
            except OSError:
                self.skipTest('symlink not available')
            with self.assertRaisesRegex(ValueError, 'outside'):
                driver.execution_directories(artifacts, metadata)


if __name__ == '__main__':
    unittest.main()
