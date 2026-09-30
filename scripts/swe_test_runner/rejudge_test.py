import copy
import hashlib
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
import zipfile
from unittest.mock import patch

import rejudge
import runner


class RejudgeTest(unittest.TestCase):
    def test_corrected_qualification_can_strengthen_but_not_weaken_passing_baseline(self):
        original = {'required_targets': ['target'], 'expected_collection': ['target', 'existing', 'corrected'],
            'expected_cases': ['target', 'existing', 'corrected'], 'expected_passes': ['target', 'existing']}
        current = copy.deepcopy(original)
        current['expected_passes'].append('corrected')
        self.assertEqual(rejudge.compare_qualification(original, current), ['corrected'])
        self.assertEqual(rejudge.compare_qualification(original, original), [])
        for passes in (['target', 'corrected'], ['target', 'existing', 'unknown']):
            with self.subTest(passes=passes), self.assertRaisesRegex(ValueError, 'weakened or includes unknown'):
                rejudge.compare_qualification(original, {**current, 'expected_passes': passes})
        for field in ('required_targets', 'expected_collection', 'expected_cases'):
            with self.subTest(field=field), self.assertRaisesRegex(ValueError, 'inventory changed'):
                rejudge.compare_qualification(original, {**current, field: ['target'] if field != 'required_targets' else []})

    def fixture(self):
        result = {'schema': 'agentgo.swe-result/v5', 'run_id': 'original', 'task_resolved': False,
            'execution_complete': True, 'infrastructure_ok': True, 'settlement_verified': True,
            'external_hard_kill': False, 'evidence_issues': [], 'pending_outcome_delivery_count': 0,
            'metrics': {'model_calls': 2, 'completed_invocations': 2, 'run_usage': {'active_reservations': []}}}
        judge = {'schema': 'agentgo.swe-judge/v2', 'run_id': 'original', 'task_id': 'task', 'tampered': False,
                 'verdict': 'failed'}
        return result, judge, {'task_id': 'task'}

    def test_closed_failed_judgement_can_be_rejudged_without_changing_source(self):
        values = self.fixture()
        original = copy.deepcopy(values)
        rejudge.validate_source_evaluation(*values, 'task')
        self.assertEqual(values, original)

    def test_unsettled_interrupted_or_mismatched_source_cannot_be_rejudged(self):
        for key, value in (('execution_complete', False), ('settlement_verified', False),
                           ('external_hard_kill', True), ('run_id', 'another'), ('infrastructure_ok', False)):
            with self.subTest(field=key):
                result, judge, reference = self.fixture()
                result[key] = value
                with self.assertRaises(ValueError):
                    rejudge.validate_source_evaluation(result, judge, reference, 'task')
        result, judge, reference = self.fixture()
        result['metrics']['run_usage']['active_reservations'] = ['pending']
        with self.assertRaises(ValueError):
            rejudge.validate_source_evaluation(result, judge, reference, 'task')
        result, judge, reference = self.fixture()
        judge['tampered'] = True
        with self.assertRaises(ValueError):
            rejudge.validate_source_evaluation(result, judge, reference, 'task')

    def test_live_attempt_is_rejected_before_archive_or_process_access(self):
        with patch.object(rejudge.artifacts, 'verify', side_effect=AssertionError('must not read')):
            with self.assertRaises(ValueError):
                rejudge.source_evidence(SimpleNamespace(state={'kind': 'model', 'quiescent': False}))

    def test_only_new_unprotected_build_cache_can_reclassify_old_tamper_verdict(self):
        result, judge, reference = self.fixture()
        judge.update(tampered=True, tampered_files=['temp/tsconfig.tsbuildinfo'])
        baseline = {'schema': 'agentgo.native-test-baseline/v1', 'files': {}}
        rejudge.validate_source_evaluation(result, judge, reference, 'task', baseline=baseline)
        for files, original in ((['tsconfig.json'], {}), (['tests/tsconfig.tsbuildinfo'], {}),
                                (['temp/tsconfig.tsbuildinfo'], {'temp/tsconfig.tsbuildinfo': 'known'}),
                                (['temp/tsconfig.tsbuildinfo', 'tests/test.py'], {})):
            with self.subTest(files=files, original=original):
                judge['tampered_files'] = files
                with self.assertRaises(ValueError):
                    rejudge.validate_source_evaluation(result, judge, reference, 'task', baseline={**baseline, 'files': original})

    def test_cmake_reclassification_requires_evidence_for_every_changed_path(self):
        result, judge, reference = self.fixture()
        name = 'build-generated/test/Makefile'
        judge.update(tampered=True, tampered_files=[name])
        baseline = {'schema': 'agentgo.native-test-baseline/v1', 'files': {}}
        generated = {'files': {name: 'archived-digest'}}
        rejudge.validate_source_evaluation(result, judge, reference, 'task', baseline=baseline, generated=generated)
        with self.assertRaises(ValueError):
            rejudge.validate_source_evaluation(result, judge, reference, 'task', baseline=baseline)
        judge['tampered_files'].append('test/real_test.cc')
        with self.assertRaises(ValueError):
            rejudge.validate_source_evaluation(result, judge, reference, 'task', baseline=baseline, generated=generated)
        judge['tampered_files'] = [name]
        with self.assertRaises(ValueError):
            rejudge.validate_source_evaluation(result, judge, reference, 'task',
                baseline={**baseline, 'files': {name: 'original'}}, generated=generated)

    def test_archived_subject_rejects_links_traversal_and_duplicates(self):
        for files in ([{'path': 'worktree/../outside', 'kind': 'file'}],
                      [{'path': 'worktree/src.ts', 'kind': 'link'}],
                      [{'path': 'worktree/src.ts', 'kind': 'file'}] * 2):
            with self.subTest(files=files), self.assertRaises(ValueError):
                rejudge.archived_subject_files({'files': files})
        self.assertEqual(rejudge.archived_subject_files({'files': [
            {'path': 'worktree/.agentgo/candidate/src.ts', 'kind': 'file'},
            {'path': 'worktree/node_modules/package', 'kind': 'link'}]}), {})

    def test_missing_judge_does_not_allow_unsettled_model_source(self):
        result, _, reference = self.fixture()
        rejudge.validate_source_evaluation(result, None, reference, 'task')
        result['settlement_verified'] = False
        with self.assertRaises(ValueError):
            rejudge.validate_source_evaluation(result, None, reference, 'task')

    def test_archived_subject_recovery_preserves_tests_and_rejects_tampering(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / 'new-worktree'
            root.mkdir()
            (root / 'src.ts').write_bytes(b'old')
            (root / 'removed.ts').write_bytes(b'removed')
            (root / 'test.ts').write_bytes(b'test')
            digest = lambda data: hashlib.sha256(data).hexdigest()
            files = [{'path': 'worktree/src.ts', 'kind': 'file', 'sha256': digest(b'new')},
                     {'path': 'worktree/test.ts', 'kind': 'file', 'sha256': digest(b'test')}]
            archive_path = Path(directory) / 'source.zip'
            with zipfile.ZipFile(archive_path, 'w') as archive:
                archive.writestr('worktree/src.ts', b'new')
                archive.writestr('worktree/test.ts', b'test')
            protected = {'test.ts': digest(b'test')}
            evidence = {'manifest': {'files': files, 'archive': archive_path.name},
                'manifest_path': Path(directory) / 'manifest.json',
                'baseline': {'schema': 'agentgo.native-test-baseline/v1', 'files': protected}}
            identity = {'files': {'src.ts': digest(b'old'), 'removed.ts': digest(b'removed'), **protected}}
            with patch.object(rejudge.native_harness, 'protected', return_value=protected), \
                    patch.object(rejudge.native_harness, 'identity', return_value=identity):
                rejudge.restore_archived_subject(root, evidence, None)
                self.assertEqual((root / 'src.ts').read_bytes(), b'new')
                self.assertEqual((root / 'test.ts').read_bytes(), b'test')
                self.assertFalse((root / 'removed.ts').exists())
                evidence['manifest']['files'][1]['sha256'] = digest(b'tampered')
                with self.assertRaisesRegex(ValueError, 'protected'):
                    rejudge.restore_archived_subject(root, evidence, None)

    def test_public_rejudge_entry_does_not_request_model_credentials(self):
        with patch.object(runner, 'required_environment_values', side_effect=AssertionError('credentials accessed')), \
                patch.object(runner.SWETestRunnerConfig, 'from_env', side_effect=AssertionError('model config accessed')), \
                patch.object(rejudge, 'run', return_value=0) as execute:
            args = runner.parser().parse_args(['rejudge', '--attempt', 'owned-attempt'])
            self.assertEqual(args.func(args), 0)
            execute.assert_called_once()
