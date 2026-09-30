"""Re-evaluate an archived native model patch without replaying its model run."""
from __future__ import annotations

import copy
import hashlib
import json
import os
from pathlib import Path
import zipfile

import artifacts
import cleanup
import lifecycle
import managed_runner
import native_harness
import native_build_artifacts
import storage
from suite_config import Suite, project_path


def source_evidence(attempt):
    if attempt.state.get('kind') != 'model' or attempt.state.get('quiescent') is not True:
        raise ValueError('rejudge requires a quiescent model attempt')
    cleanup._processes_stopped(attempt.path)
    manifest_path = Path(attempt.state['archive_ref'])
    manifest = artifacts.verify(manifest_path)
    if manifest['attempt_id'] != attempt.attempt_id or manifest['root_id'] != attempt.storage.root_id:
        raise ValueError('rejudge archive identity mismatch')
    with zipfile.ZipFile(manifest_path.parent / manifest['archive']) as archive:
        result = json.loads(archive.read('reports/result.json'))
        judge = json.loads(archive.read('reports/judge.json')) if 'reports/judge.json' in archive.namelist() else None
        reference = json.loads(archive.read('reports/native_reference.json'))
        patch = archive.read('reports/model.patch') if 'reports/model.patch' in archive.namelist() else None
        baseline = json.loads(archive.read('reports/test_baseline_manifest.json'))
        dependency_files = {}
        if 'Cargo.lock' in baseline.get('files', {}):
            data = archive.read('worktree/Cargo.lock')
            if hashlib.sha256(data).hexdigest() != baseline['files']['Cargo.lock']:
                raise ValueError('original model changed its dependency lock')
            dependency_files['Cargo.lock'] = data
        ignored_sources = json.loads(archive.read('reports/ignored_source_artifacts.json')) if 'reports/ignored_source_artifacts.json' in archive.namelist() else None
        generated = archived_build_artifacts(manifest, archive, attempt.state['worktree'])
        if judge is None and 'reports/failure.json' not in archive.namelist():
            raise ValueError('missing judgement without an archived failure receipt')
    validate_source_evaluation(result, judge, reference, attempt.state['task'], baseline=baseline, generated=generated)
    if reclassified_build_caches(judge, baseline, generated):
        archived = {item['path']: item for item in manifest['files']}
        if any(archived.get('worktree/' + name, {}).get('sha256') != digest
               for name, digest in baseline['files'].items()):
            raise ValueError('build-cache reclassification cannot conceal original test changes')
    return {'result': result, 'judge': judge, 'reference': reference, 'patch': patch,
            'baseline': baseline, 'manifest': manifest, 'manifest_path': manifest_path,
            'ignored_sources': ignored_sources,
            'dependency_files': dependency_files,
            'generated_build_artifacts': generated,
            'reclassified_build_caches': reclassified_build_caches(judge, baseline, generated)}


def reclassified_build_caches(judge, baseline, generated=None):
    names = (judge or {}).get('tampered_files', [])
    if (not names or (judge or {}).get('tampered') is not True or not isinstance(baseline, dict)
            or baseline.get('schema') != 'agentgo.native-test-baseline/v1' or not isinstance(baseline.get('files'), dict)):
        return []
    for name in names:
        typescript_cache = Path(name).suffix.lower() == '.tsbuildinfo' and not native_harness.test_path(name)
        cmake_output = name in (generated or {}).get('files', {})
        if (project_path(name) != name or not (typescript_cache or cmake_output) or name in baseline.get('files', {})):
            return []
    return list(names)


def validate_source_evaluation(result, judge, reference, task_id, *, baseline=None, generated=None):
    metrics = result.get('metrics', {})
    if (result.get('schema') != 'agentgo.swe-result/v5'
            or not result.get('run_id')
            or judge is not None and (judge.get('schema') != 'agentgo.swe-judge/v2'
                or result['run_id'] != judge.get('run_id')
                or (judge.get('tampered') is not False and not reclassified_build_caches(judge, baseline, generated))
                or judge.get('task_id') != task_id)
            or result.get('execution_complete') is not True or result.get('infrastructure_ok') is not True
            or result.get('settlement_verified') is not True or result.get('external_hard_kill') is not False
            or result.get('evidence_issues') != [] or result.get('pending_outcome_delivery_count') != 0
            or metrics.get('run_usage', {}).get('active_reservations') != []
            or not isinstance(metrics.get('model_calls'), int) or metrics['model_calls'] <= 0
            or metrics.get('model_calls') != metrics.get('completed_invocations')
            or reference.get('task_id') != task_id):
        raise ValueError('source model run is not eligible for same-patch rejudging')


def archived_subject_files(manifest):
    files = {}
    for item in manifest['files']:
        if not item['path'].startswith('worktree/'):
            continue
        name = item['path'][len('worktree/'):]
        if not name:
            continue
        if project_path(name) != name:
            raise ValueError('noncanonical archived subject path')
        if set(Path(name).parts[:-1]) & native_harness.EXCLUDED:
            continue
        if item['kind'] == 'directory':
            continue
        if item['kind'] != 'file':
            raise ValueError('archived subject contains an unsupported link')
        if name in files:
            raise ValueError('duplicate archived subject path')
        files[name] = item
    return files


def archived_build_artifacts(manifest, archive, source_root):
    files = archived_subject_files(manifest)
    caches = {}
    for name, item in files.items():
        if name.endswith('/CMakeCache.txt'):
            member = artifacts.archive_member_name(item, manifest)
            if archive.getinfo(member).file_size <= 1024 * 1024:
                caches[name] = archive.read(member)
    if not caches:
        return {'schema': native_build_artifacts.SCHEMA, 'files': {}, 'roots': {}}
    before = json.loads(archive.read('reports/targeted-baseline/execution.json'))
    if Path(before['cwd']) != Path(source_root):
        raise ValueError('archived build classification has a different baseline workspace')
    return native_build_artifacts.classify({name: item['sha256'] for name, item in files.items()},
        caches, source_root, before['input_before']['files'])


def validate_reclassified_artifacts(root, evidence, api):
    names = sorted(set(evidence['reclassified_build_caches']) |
                   set(evidence.get('generated_build_artifacts', {}).get('files', {})))
    if not names:
        return
    tracked = set(api.run_command(['git', 'ls-files', '--cached', '-z'], cwd=root).stdout.decode('utf-8').split('\0'))
    if tracked.intersection(names):
        raise ValueError('reclassified build cache is a tracked input')
    # TypeScript's extension-based legacy exception still requires ignore proof.
    # CMake outputs instead have source/binary metadata and pre-model inventory proof.
    require_ignored = sorted(set(names) - set(evidence.get('generated_build_artifacts', {}).get('files', {})))
    if require_ignored:
        ignored = api.run_command(['git', 'check-ignore', '--no-index', '--stdin', '-z'], cwd=root,
            input_data=b''.join(name.encode('utf-8') + b'\0' for name in require_ignored), check=False)
        ignored_names = set(ignored.stdout.decode('utf-8').split('\0')) - {''}
        if ignored.returncode != 0 or ignored_names != set(require_ignored):
            raise ValueError('reclassified build cache is a non-ignored input')
    entries = {item['path']: item for item in evidence['manifest']['files']}
    for name in names:
        entry = entries.get('worktree/' + name)
        if not entry or entry['kind'] != 'file':
            raise ValueError('reclassified build cache has no archived file evidence')


def restore_archived_subject(root, evidence, task):
    """Recover the delivered source when the old judge failed before saving a patch."""
    files = archived_subject_files(evidence['manifest'])
    files = {name: item for name, item in files.items()
             if name not in evidence.get('generated_build_artifacts', {}).get('files', {})}
    baseline = evidence['baseline']
    current_protected = native_harness.protected(root, task)
    archived_protected = {name: files.get(name, {}).get('sha256') for name in current_protected}
    archived_protected.update({name: item['sha256'] for name, item in files.items() if native_harness.test_path(name)})
    if (baseline.get('schema') != 'agentgo.native-test-baseline/v1'
            or current_protected != baseline.get('files') or archived_protected != current_protected):
        raise ValueError('archived source changed protected tests or configuration')
    before = native_harness.identity(root)['files']
    archive_path = evidence['manifest_path'].parent / evidence['manifest']['archive']
    with zipfile.ZipFile(archive_path) as archive:
        for name in sorted(set(before) - set(files)):
            (root / name).unlink()
        for name, item in files.items():
            target = root / name
            if not target.resolve().is_relative_to(root.resolve()) or target.is_symlink():
                raise ValueError('archived source recovery escaped its new workspace')
            if before.get(name) != item['sha256']:
                target.parent.mkdir(parents=True, exist_ok=True)
                data = archive.read(artifacts.archive_member_name(item, evidence['manifest']))
                if hashlib.sha256(data).hexdigest() != item['sha256']:
                    raise ValueError('archived source recovery content changed')
                target.write_bytes(data)
            if os.name != 'nt' and 'mode' in item:
                target.chmod(item['mode'] & 0o777)
    return hashlib.sha256(json.dumps({name: item['sha256'] for name, item in files.items()}, sort_keys=True).encode()).hexdigest()


def restore_ignored_source_inputs(root, evidence, api):
    record = evidence.get('ignored_sources')
    if record is None:
        return []
    if record.get('schema') != 'agentgo.swe-ignored-source/v1' or not isinstance(record.get('files'), dict):
        raise ValueError('ignored source evidence schema is invalid')
    manifest = {item['path']: item for item in evidence['manifest']['files']}
    restored = []
    with zipfile.ZipFile(evidence['manifest_path'].parent / evidence['manifest']['archive']) as archive:
        for name, digest in record['files'].items():
            if name in evidence.get('generated_build_artifacts', {}).get('files', {}):
                if evidence['generated_build_artifacts']['files'][name] != digest:
                    raise ValueError('generated build artifact digest differs from original input')
                continue
            if (project_path(name) != name or native_harness.test_path(name)
                    or set(Path(name).parts[:-1]) & native_harness.EXCLUDED):
                raise ValueError('ignored source evidence includes protected input')
            tracked = api.run_command(['git', 'ls-files', '--error-unmatch', '--', name], cwd=root, check=False)
            ignored = api.run_command(['git', 'check-ignore', '--quiet', '--no-index', '--', name], cwd=root, check=False)
            item = manifest.get('worktree/' + name)
            if tracked.returncode == 0 or ignored.returncode != 0 or not item or item['kind'] != 'file' or item['sha256'] != digest:
                raise ValueError('ignored source evidence does not match the frozen checkout and archive')
            target = root / name
            if target.is_symlink() or not target.resolve().is_relative_to(root.resolve()):
                raise ValueError('ignored source input escapes the replay workspace')
            data = archive.read(artifacts.archive_member_name(item, evidence['manifest']))
            if hashlib.sha256(data).hexdigest() != digest:
                raise ValueError('ignored source input digest changed')
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(data)
            if os.name != 'nt' and 'mode' in item:
                target.chmod(item['mode'] & 0o777)
            restored.append(name)
    return sorted(restored)


def compare_qualification(original, current):
    for key in ('required_targets', 'expected_collection', 'expected_cases'):
        if set(current[key]) != set(original[key]):
            raise ValueError('qualification test inventory changed during rejudge')
    old_passes, new_passes = set(original['expected_passes']), set(current['expected_passes'])
    if (not old_passes <= new_passes or not new_passes <= set(current['expected_cases'])):
        raise ValueError('qualification passing baseline weakened or includes unknown tests')
    return sorted(new_passes - old_passes)


def run(args, api):
    source = managed_runner.existing_attempt(args.attempt)
    evidence = source_evidence(source)
    if evidence['reference'].get('platform') != native_harness.sys_platform():
        raise ValueError('same-patch rejudge must use the source assessment platform')
    suite_dir = Path(__file__).parent / 'suites' / source.state['suite']
    suite = Suite.load(suite_dir)
    if suite.engine != 'native':
        raise ValueError('same-patch rejudge currently requires a native suite')
    task = next((item for item in api.load_tasks(suite_dir / 'tasks.json', 'native')
                 if item.task_id == source.state['task']), None)
    if task is None:
        raise ValueError('source task is not in the current task catalog')
    upstreams = [item['external_path'] for item in source.resources
                 if item.get('owner') == 'borrowed' and item.get('purpose') == 'repo']
    repo = Path(args.upstream_repo).resolve(strict=True) if args.upstream_repo else (
        Path(upstreams[0]).resolve(strict=True) if len(upstreams) == 1 else None)
    if repo is None:
        raise ValueError('source upstream repository is unavailable; supply --upstream-repo')
    root = Path(args.testbed).resolve() if args.testbed else source.storage.path
    config = api.SWETestRunnerConfig(Path(__file__).resolve().parents[2], None, root,
        suite_dir / 'tasks.json', suite_dir / 'prompts', repo, '', '', '', 'responses', suite)
    config.managed_storage = True
    config._frozen_dependency_files = evidence['dependency_files']
    if native_harness.definition_digest(config, task) != evidence['reference'].get('definition_digest'):
        raise ValueError('task definition changed; old model evidence cannot be silently reassigned')
    policy_file = getattr(args, 'storage_policy', None)
    policy = storage.StoragePolicy.load(policy_file) if policy_file else source.storage.policy
    session = lifecycle.Session(root, policy, {'mode': 'rejudge', 'suite': suite.suite_id,
        'source_attempt': str(source.path), 'source_archive_sha256': evidence['manifest']['archive_sha256'],
        'new_model_calls': 0})
    session.expected = [{'suite': suite.suite_id, 'task': task.task_id}]
    session.expected_kind = 'rejudge'
    session.checkpoint()
    storage.atomic_json(session.batch_dir / 'completion.json', {'complete': False, 'mode': 'rejudge', 'expected_count': 1})
    print(json.dumps({'managed_batch': session.batch_id, 'path': str(session.batch_dir), 'mode': 'rejudge'}), flush=True)
    try:
        managed_runner.ensure_qualified(session, config, task, api)
        reference_path = native_harness.qualification_dir(config, task) / 'reference.json'
        reference = storage.read_json(reference_path)
        added_baseline_passes = compare_qualification(evidence['reference'], reference)

        def evaluate(attempt):
            native_harness.prepare_task(config, task, api)
            validate_reclassified_artifacts(attempt.worktree, evidence, api)
            storage.atomic_json(attempt.reports / 'source-generated-build-artifacts.json', evidence['generated_build_artifacts'])
            storage.atomic_json(attempt.reports / 'run_contract.json', {'run_id': evidence['result']['run_id']})
            patch = evidence['patch']
            source_tree_digest = None
            if patch is not None:
                (attempt.reports / 'source-model.patch').write_bytes(patch)
            baseline_files = native_harness.identity(attempt.worktree)['files']
            if patch is None:
                source_tree_digest = restore_archived_subject(attempt.worktree, evidence, task)
            elif patch:
                api.run_command(['git', 'apply', '--check', '-'], cwd=attempt.worktree, input_data=patch)
                api.run_command(['git', 'apply', '-'], cwd=attempt.worktree, input_data=patch)
            restored_ignored = restore_ignored_source_inputs(attempt.worktree, evidence, api) if patch is not None else []
            current_files = native_harness.identity(attempt.worktree)['files']
            changed = [name for name in set(baseline_files) | set(current_files)
                       if baseline_files.get(name) != current_files.get(name)]
            manifest_files = {item['path']: item for item in evidence['manifest']['files']}
            for name in changed:
                if project_path(name) != name or native_harness.test_path(name):
                    raise ValueError('source patch modifies protected or noncanonical input')
                path = attempt.worktree / name
                archived = manifest_files.get('worktree/' + name)
                if path.exists():
                    if path.is_symlink() or not archived or archived.get('kind') != 'file' or artifacts.digest_file(path) != archived['sha256']:
                        raise ValueError('recreated model source differs from its archived content')
                elif archived is not None:
                    raise ValueError('recreated model deletion differs from archived content')
            provenance = {'schema': 'agentgo.swe-rejudge/v1', 'source_attempt': str(source.path),
                'source_attempt_id': source.attempt_id, 'source_archive_sha256': evidence['manifest']['archive_sha256'],
                'source_patch_sha256': hashlib.sha256(patch).hexdigest() if patch is not None else None,
                'source_tree_sha256': source_tree_digest, 'source_judge_absent': evidence['judge'] is None,
                'source_reclassified_build_caches': evidence['reclassified_build_caches'],
                'source_generated_build_artifacts_ref': str(attempt.reports / 'source-generated-build-artifacts.json'),
                'restored_ignored_source_inputs': restored_ignored,
                'frozen_dependency_inputs': {name: hashlib.sha256(data).hexdigest()
                    for name, data in evidence['dependency_files'].items()},
                'qualification_passes_added': added_baseline_passes,
                'source_run_id': evidence['result']['run_id'],
                'new_model_calls': 0, 'model_history_replayed': False, 'source_result_modified': False,
                'qualification_inventory_unchanged': True}
            original = copy.deepcopy(evidence['result'])
            original['rejudge'] = provenance
            storage.atomic_json(attempt.reports / 'source-result.json', evidence['result'])
            storage.atomic_json(attempt.reports / 'result.json', original)
            native_harness.judge_task(config, task, api)
            result = api.finalize_result(str(attempt.reports / 'result.json'), str(attempt.reports / 'judge.json'))
            storage.atomic_json(attempt.reports / 'rejudge.json', provenance)
            return result

        result = session.execute(config, task.task_id, 'rejudge', evaluate)
        code = api.final_exit_code(result)
        storage.atomic_json(session.batch_dir / 'completion.json', {'complete': True, 'mode': 'rejudge',
            'expected_count': 1, 'task_resolved': result.get('task_resolved'), 'exit_code': code, 'new_model_calls': 0})
        return code
    except BaseException as error:
        storage.atomic_json(session.batch_dir / 'completion.json', {'complete': False, 'mode': 'rejudge',
            'expected_count': 1, 'reason': getattr(error, 'code', type(error).__name__), 'new_model_calls': 0})
        raise
