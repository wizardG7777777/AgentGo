"""Shared native SWE transactions for version-three repository suites."""
from __future__ import annotations

import collections
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import time
import uuid
import contextlib

import native_drivers
import native_build_artifacts
import native_reports
import native_test_adaptations
from suite_config import Suite, project_path
import processes as managed_processes
import storage as resource_storage

EXCLUDED = {".git", ".agentgo", ".venv", "node_modules", ".swe-build", "__pycache__", ".pytest_cache"}
SOURCE_SUFFIXES = {".go", ".py", ".rs", ".ts", ".tsx", ".js", ".jsx", ".c", ".cc", ".cpp", ".h", ".hpp", ".s", ".S"}
CONFIG_NAMES = {"go.mod", "go.sum", "go.work", "go.work.sum", "Cargo.toml", "Cargo.lock", "package.json", "package-lock.json",
                "pnpm-lock.yaml", "pnpm-workspace.yaml", "yarn.lock", "pyproject.toml", "setup.py", "setup.cfg", "tox.ini",
                "pytest.ini", "conftest.py", "CMakeLists.txt", "Makefile", ".gitignore", ".gitattributes"}


def save(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    temporary.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    temporary.replace(path)


def read(path):
    return json.loads(path.read_text(encoding="utf-8"))


def load_native_tasks(path, task_class):
    data = read(path)
    if data.get("schema") != "agentgo.native-swe-tasks/v1" or not isinstance(data.get("tasks"), list):
        raise ValueError("invalid native task manifest")
    if len(data["tasks"]) != 8:
        raise ValueError("native repository suite must contain exactly eight tasks")
    tasks, seen, commits = [], set(), set()
    for item in data["tasks"]:
        task_id = item["task_id"]
        for key in ("base_sha", "fix_sha"):
            if not re.fullmatch(r"[0-9a-f]{40}", item.get(key, "")):
                raise ValueError("native task requires full immutable Git identities")
        if task_id in seen or item["fix_sha"] in commits:
            raise ValueError("duplicate native task or source fix")
        seen.add(task_id)
        commits.add(item["fix_sha"])
        tests, sources = item.get("test_files"), item.get("source_files")
        if not tests or not sources or set(tests) & set(sources):
            raise ValueError("native task requires disjoint test and source patch paths")
        for name in tests + sources:
            if project_path(name) != name:
                raise ValueError("native patch paths must be canonical")
        native_test_adaptations.validate(item)
        tasks.append(task_class(task_id, item["fix_sha"], tuple(tests), item["title"],
                                tuple(item.get("test_selectors", [])), item))
    return tasks


def test_path(name):
    p = Path(name)
    if re.search(r"(^|/)(tests?|testing|__tests__|testdata)/", name):
        return True
    if p.suffix.lower() == ".tsbuildinfo":
        return False
    return (name.endswith("_test.go")
            or bool(re.search(r"\.(test|spec)\.", name)) or p.name in CONFIG_NAMES
            or p.name.startswith(("vitest.config", "tsconfig", "jest.config")))


def identity(root):
    root = root.resolve(strict=True)
    listed = managed_processes.run(["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"],
                                   cwd=root, check=True, timeout=60, label="source-inventory")
    visible = set(listed.stdout.decode("utf-8").split("\0"))
    files, caches = {}, {}
    for current, directories, names in os.walk(root, followlinks=False):
        directories[:] = sorted(name for name in directories if name not in EXCLUDED)
        for name in directories:
            if (Path(current) / name).is_symlink():
                raise ValueError("subject directory cannot escape through a symlink")
        for name in names:
            path = Path(current) / name
            relative = path.relative_to(root).as_posix()
            if name == 'CMakeCache.txt' and not path.is_symlink() and path.stat().st_size <= 1024 * 1024:
                caches[relative] = path.read_bytes()
            if relative not in visible and path.suffix not in SOURCE_SUFFIXES and not test_path(relative):
                continue
            if path.is_symlink() or not path.resolve().is_relative_to(root):
                raise ValueError("subject file cannot escape through a symlink")
            if path.is_file():
                files[relative] = hashlib.sha256(path.read_bytes()).hexdigest()
    digest = hashlib.sha256(json.dumps(files, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
    tracked = set(managed_processes.run(['git', 'ls-files', '--cached', '-z'], cwd=root,
        check=True, timeout=60, label='tracked-input-inventory').stdout.decode('utf-8').split('\0')) - {''}
    generated = native_build_artifacts.classify(files, caches, root, tracked)
    return {"schema": "agentgo.swe-test-input/v1", "files": files, "digest": digest,
            "generated_build_artifacts": generated}


def protected(root, task):
    inventory = identity(root)
    files = inventory['files']
    generated = inventory.get('generated_build_artifacts', {}).get('files', {})
    names = set(task.test_files) | {name for name in files if test_path(name) and name not in generated}
    return {name: files.get(name) for name in sorted(names)}


def definition_digest(config, task):
    options = {key: value for key, value in config.suite.options.items() if key != "qualification_status"}
    return hashlib.sha256(json.dumps({"suite": options, "task": task.metadata}, sort_keys=True).encode()).hexdigest()


def _source_digest():
    root = Path(__file__).parent
    values = {path.name: hashlib.sha256(path.read_bytes()).hexdigest() for path in root.glob("*.py")
              if not path.name.endswith("_test.py")}
    return hashlib.sha256(json.dumps(values, sort_keys=True).encode()).hexdigest()


_LOADED_HARNESS_DIGEST = _source_digest()


def harness_digest():
    return _LOADED_HARNESS_DIGEST


def snapshot(config, task, api, target=None):
    if not config.repo.exists():
        config.repo.parent.mkdir(parents=True, exist_ok=True)
        api.run_command(["git", "clone", "--quiet", config.suite.repository, str(config.repo)])
    root = target or config.worktree(task.task_id)
    root.parent.mkdir(parents=True, exist_ok=True)
    prepared = root / ".swe-build" / "prepared.json"
    frozen_dependencies = getattr(config, '_frozen_dependency_files', {})
    environment_key = hashlib.sha256(json.dumps({"base": task.metadata["base_sha"],
        "suite": config.suite.options.get("environment", {}), "task": task.metadata.get("environment", {}),
        "links": config.suite.options.get("materialize_links", []),
        "frozen_dependencies": {name: hashlib.sha256(data).hexdigest() for name, data in frozen_dependencies.items()}}, sort_keys=True).encode()).hexdigest()
    if target is not None and prepared.exists():
        receipt = read(prepared)
        if receipt.get("environment_key") == environment_key and receipt.get("root") == str(root.resolve()):
            api.run_command(["git", "reset", "--hard", receipt["head"]], cwd=root)
            api.run_command(["git", "clean", "-fd"], cwd=root)
            driver = native_drivers.make(config, task, root, existing=True)
            driver.identity()
            return root, driver
    api.safe_remove_worktree(config, root)
    root.mkdir(exist_ok=True)
    api.run_command(["git", "init", "-q", str(root)])
    api.run_command(["git", "config", "core.longpaths", "true"], cwd=root)
    api.run_command(["git", "-c", "core.longpaths=true", "fetch", "--quiet", "--no-tags", "--depth=1", str(config.repo), task.metadata["base_sha"]], cwd=root)
    api.run_command(["git", "checkout", "--quiet", "--detach", "FETCH_HEAD"], cwd=root)
    (root / ".git" / "FETCH_HEAD").unlink(missing_ok=True)
    materialized = []
    for relative in config.suite.options.get("materialize_links", []):
        relative = project_path(relative)
        mode = api.run_command(["git", "ls-files", "-s", "--", relative], cwd=root).stdout.decode()
        if not mode.startswith("120000 "):
            continue
        link = api.run_command(["git", "show", "HEAD:" + relative], cwd=root).stdout.decode().strip()
        path = root / relative
        source = (path.parent / link).resolve(strict=True)
        if not source.is_relative_to(root.resolve()) or source == path:
            raise ValueError("declared documentation link escapes the native checkout")
        path.unlink()
        if source.is_dir():
            shutil.copytree(source, path, symlinks=False)
        else:
            shutil.copy2(source, path)
        materialized.append(relative)
    if materialized:
        api.run_command(["git", "add", "-A", "--", *materialized], cwd=root)
        api.run_command(["git", "-c", "user.name=agentgo-swe", "-c", "user.email=swe-test-runner@agentgo.local",
                         "commit", "-q", "-m", "Materialize declared internal documentation links"], cwd=root)
    with (root / ".git" / "info" / "exclude").open("a", encoding="utf-8") as stream:
        stream.write("\n.swe-build/\n.agentgo/\n")
    for name, data in frozen_dependencies.items():
        if name != 'Cargo.lock' or config.suite.options['driver'] != 'cargo':
            raise ValueError('unsupported frozen dependency input')
        path = root / name
        if path.is_symlink():
            raise ValueError('frozen dependency input cannot be a link')
        if path.exists() and path.read_bytes() != data:
            raise ValueError('frozen dependency input differs from tracked baseline')
        path.write_bytes(data)
    driver = native_drivers.make(config, task, root)
    current = resource_storage.active()
    if current:
        for relative in ("node_modules", ".venv", ".swe-build/venv", ".swe-build/cmake", ".swe-build/cargo"):
            current.register(root / relative, "owned", "rebuildable_environment", archive=False)
    driver.prepare()
    save(root / ".swe-build" / "environment.json", driver.identity())
    save(prepared, {"environment_key": environment_key, "root": str(root.resolve()),
                    "head": api.run_command(["git", "rev-parse", "HEAD"], cwd=root).stdout.decode().strip()})
    return root, driver


def apply_patch(config, task, root, names, api):
    changed = set(api.run_command(["git", "-C", str(config.repo), "diff", "--name-only", "-z",
                                   task.metadata["base_sha"], task.fix_sha]).stdout.decode().split("\0"))
    if not set(names).issubset(changed):
        raise ValueError("declared patch is not part of the upstream fix")
    patch = api.run_command(["git", "-C", str(config.repo), "diff", "--binary", task.metadata["base_sha"], task.fix_sha, "--", *names]).stdout
    if not patch:
        raise ValueError("empty native task patch")
    api.run_command(["git", "apply", "-"], cwd=root, input_data=patch)
    if set(names) == set(task.test_files):
        receipts = native_test_adaptations.apply(root, task)
        if receipts:
            save(root / '.swe-build' / 'test-adaptations.json', {
                'schema': 'agentgo.swe-test-adaptations/v1', 'task_id': task.task_id,
                'definition_digest': definition_digest(config, task), 'files': receipts})
            current = resource_storage.active()
            if current:
                save(current.reports / 'test-adaptations.json', {
                    'schema': 'agentgo.swe-test-adaptations/v1', 'task_id': task.task_id,
                    'definition_digest': definition_digest(config, task), 'files': receipts})


def execute_tests(driver, config, task, root, run_dir, stage, targeted=False, run_id=""):
    current = resource_storage.active()
    with current.phase(stage) if current else contextlib.nullcontext():
        return _execute_tests(driver, config, task, root, run_dir, stage, targeted, run_id)


def _execute_tests(driver, config, task, root, run_dir, stage, targeted=False, run_id=""):
    command_offset = len(driver.commands)
    output = run_dir / stage
    output.mkdir(parents=True, exist_ok=True)
    execution = {"schema": "agentgo.swe-test-execution/v1", "test_engine": config.suite.options["driver"],
                 "test_execution_id": str(uuid.uuid4()), "task_id": task.task_id, "run_id": run_id,
                 "stage": stage, "cwd": str(root.resolve()), "status": "running", "started_at": time.time(),
                 "definition_digest": definition_digest(config, task), "input_before": identity(root)}
    if stage not in {'clean', 'clean-baseline'}:
        execution['test_adaptations'] = [
            {key: item[key] for key in ('platform', 'path', 'upstream_sha256', 'adapted_sha256')}
            for item in task.metadata.get('test_adaptations', []) if item['platform'] == sys_platform()]
    path = output / "execution.json"
    save(path, execution)
    try:
        execution["environment"] = driver.identity()
        if config.suite.options["driver"] == "vitest" and stage in {"clean", "clean-baseline"} and not targeted:
            added = driver.command(["git", "-C", str(config.repo), "diff", "--no-renames", "--diff-filter=A",
                "--name-only", "-z", task.metadata["base_sha"], task.fix_sha], label="baseline-test-additions").stdout.decode("utf-8").split("\0")
            additions = sorted(set(added) & set(task.test_files))
            execution["baseline_test_additions"] = additions
            value, text = driver.test(targeted, output, baseline_added_files=additions)
        else:
            value, text = driver.test(targeted, output)
        execution["input_after"] = identity(root)
        if execution["input_before"] != execution["input_after"]:
            changed = [name for name in set(execution["input_before"]["files"]) | set(execution["input_after"]["files"])
                       if execution["input_before"]["files"].get(name) != execution["input_after"]["files"].get(name)]
            raise ValueError("test inputs changed during native execution: " + ", ".join(sorted(changed)[:10]))
        execution["environment_after"] = driver.identity()
        if execution["environment_after"] != execution["environment"]:
            raise ValueError("native tool/dependency identity changed while testing")
        execution.update(status="completed", exit_code=value["exit_code"], collected_nodeids=value["collected_nodeids"])
        value.update(test_execution_ref=str(path), test_input_digest=execution["input_before"]["digest"])
        save(output / "report.json", value)
        (run_dir / (stage + ".native.log")).write_text(text, encoding="utf-8")
        native_test_adaptations.check_collection(task, value, stage)
        return value
    except BaseException as error:
        execution.update(status="interrupted" if isinstance(error, KeyboardInterrupt) else "invalid", error_type=type(error).__name__)
        raise
    finally:
        execution.update(finished_at=time.time(), commands=driver.commands[command_offset:])
        if resource_storage.active() is None:
            command_dir = output / "commands"
            command_dir.mkdir(exist_ok=True)
            for artifact in driver.build.glob("[0-9]*.*"):
                if artifact.is_file():
                    shutil.copy2(artifact, command_dir / artifact.name)
        save(path, execution)


def qualification_dir(config, task):
    root = config.testbed / "qualification" / task.task_id
    dependencies = getattr(config, '_frozen_dependency_files', {})
    if dependencies:
        digest = hashlib.sha256(json.dumps({name: hashlib.sha256(data).hexdigest()
            for name, data in dependencies.items()}, sort_keys=True).encode()).hexdigest()
        return root / 'dependency-replay' / digest
    return root


def qualify(config, task, api):
    current = resource_storage.active()
    target = current.worktree if current else config.testbed / "worktrees" / ("verify-" + task.task_id)
    root, driver = snapshot(config, task, api, target)
    anchor = qualification_dir(config, task)
    out = current.reports if current else anchor / "attempts" / uuid.uuid4().hex
    clean = execute_tests(driver, config, task, root, out, "clean")
    if clean["errors"] or clean["not_run"] or not (clean["passed"] or clean.get("empty_before_test_patch") is True):
        raise ValueError("clean native baseline did not finish its declared regression scope")
    apply_patch(config, task, root, list(task.test_files), api)
    red = execute_tests(driver, config, task, root, out, "red", targeted=True)
    clean_failed = {item["nodeid"] for item in clean["failure_events"]}
    required = {item["nodeid"] for item in red["failure_events"] if item["nodeid"] not in clean_failed and item["phase"] not in {"build", "collection"}}
    compile_regression = any(item["phase"] in {"build", "collection"} for item in red["failure_events"])
    if not required and not compile_regression:
        raise ValueError("test patch did not reproduce a new source defect")
    apply_patch(config, task, root, task.metadata["source_files"], api)
    target_green = execute_tests(driver, config, task, root, out, "green-targeted", targeted=True)
    full_green = execute_tests(driver, config, task, root, out, "green")
    if compile_regression:
        required = {name for name, outcome in target_green["case_outcomes"].items() if outcome == "pass"}
    if not required or any(target_green["case_outcomes"].get(name) != "pass" for name in required):
        raise ValueError("upstream fix does not pass the required regression tests")
    baseline, replaced_ids = native_reports.project_baseline(clean, full_green["case_outcomes"])
    okay, comparison = native_reports.compare(baseline, full_green["collected_nodeids"], full_green, required)
    if not okay:
        raise ValueError("upstream source fix does not preserve the declared clean baseline")
    reference = {"schema": "agentgo.native-qualification/v1", "task_id": task.task_id,
                 "definition_digest": definition_digest(config, task), "platform": sys_platform(),
                 "harness_digest": harness_digest(),
                 "required_targets": sorted(required), "expected_collection": full_green["collected_nodeids"],
                 "expected_cases": sorted(full_green["case_outcomes"]),
                 "expected_passes": sorted(name for name, state in full_green["case_outcomes"].items() if state == "pass"),
                 "test_patch_replaced_identities": replaced_ids,
                 "clean": clean, "comparison": comparison, "status": "qualified",
                 "target_passed": target_green["passed"], "regression_passed": full_green["passed"]}
    save(out / "reference.json", reference)
    save(anchor / "reference.json", reference)
    print(json.dumps({key: reference[key] for key in ("task_id", "status", "target_passed", "regression_passed")}), flush=True)
    return reference


def sys_platform():
    import sys
    return sys.platform


def prepare_task(config, task, api):
    reference_path = qualification_dir(config, task) / "reference.json"
    reference = read(reference_path) if reference_path.exists() else None
    if (not reference or reference.get("definition_digest") != definition_digest(config, task)
            or reference.get("harness_digest") != harness_digest() or reference.get("platform") != sys_platform()):
        reference = qualify(config, task, api)
    root, driver = snapshot(config, task, api)
    out = config.run_dir(task.task_id)
    clean = execute_tests(driver, config, task, root, out, "clean-baseline")
    apply_patch(config, task, root, list(task.test_files), api)
    api.run_command(["git", "add", "--", *task.test_files], cwd=root)
    api.run_command(["git", "-c", "user.name=agentgo-swe", "-c", "user.email=swe-test-runner@agentgo.local",
                     "commit", "-q", "-m", "Install frozen native regression tests", "--", *task.test_files], cwd=root)
    red = execute_tests(driver, config, task, root, out, "targeted-baseline", targeted=True)
    if not red["failure_events"]:
        raise ValueError("required native red state disappeared")
    save(out / "clean_baseline.json", clean)
    save(out / "native_reference.json", reference)
    save(out / "test_baseline_manifest.json", {"schema": "agentgo.native-test-baseline/v1", "files": protected(root, task)})
    return red


def agent_environment(config, task, root):
    return native_drivers.make(config, task, root, existing=True).agent_environment()


def capture_source_patch(root, out, api):
    """Export Git-visible source; retain ignored source inputs as separate evidence."""
    inventory = identity(root)
    files = inventory['files']
    generated = inventory.get('generated_build_artifacts', {})
    save(out / 'generated_build_artifacts.json', generated)
    api.run_command(['git', 'reset', '-q'], cwd=root)
    visible = set(api.run_command(['git', 'ls-files', '--cached', '--others', '--exclude-standard', '-z'],
                                  cwd=root).stdout.decode('utf-8').split('\0')) - {''}
    tracked = set(api.run_command(['git', 'ls-files', '--cached', '-z'], cwd=root).stdout.decode('utf-8').split('\0')) - {''}
    sources = {name for name in set(files) | tracked if not test_path(name) and name not in generated.get('files', {})}
    paths = sorted(sources & visible)
    ignored = {name: files[name] for name in sorted(sources - visible) if name in files}
    save(out / 'ignored_source_artifacts.json', {'schema': 'agentgo.swe-ignored-source/v1',
        'files': ignored, 'retained_in_input_identity': True, 'retained_in_archive': True,
        'patch_scope': 'git-visible-production-files'})
    if paths:
        # NUL-delimited literal paths preserve spaces, glob characters and large
        # inventories without exceeding Windows CreateProcess argv limits.
        api.run_command(['git', '--literal-pathspecs', 'add', '-A',
                         '--pathspec-from-file=-', '--pathspec-file-nul'], cwd=root,
                        input_data=b''.join(name.encode('utf-8') + b'\0' for name in paths))
        staged = set(api.run_command(['git', 'diff', '--cached', '--name-only', '-z'],
                                    cwd=root).stdout.decode('utf-8').split('\0')) - {''}
        if not staged <= set(paths):
            raise ValueError('patch index contains files outside the selected production paths')
        # The isolated index was reset above and now contains only this selection.
        patch = api.run_command(['git', 'diff', '--cached', '--binary', '--no-ext-diff', '--no-textconv'], cwd=root).stdout
    else:
        patch = b''
    (out / 'model.patch').write_bytes(patch)
    return patch, ignored


def judge_task(config, task, api):
    root, out = config.worktree(task.task_id), config.run_dir(task.task_id)
    run_id = api.read_json(out / "run_contract.json", {}).get("run_id", "")
    before = read(out / "test_baseline_manifest.json")
    if before.get("schema") != "agentgo.native-test-baseline/v1":
        raise ValueError("missing native test protection manifest")
    current = protected(root, task)
    tampered = sorted(name for name in set(before["files"]) | set(current) if before["files"].get(name) != current.get(name))
    patch, ignored_sources = capture_source_patch(root, out, api)
    driver = native_drivers.make(config, task, root, existing=True)
    target = execute_tests(driver, config, task, root, out, "judge-targeted", True, run_id)
    full = execute_tests(driver, config, task, root, out, "judge", False, run_id)
    reference, clean = read(out / "native_reference.json"), read(out / "clean_baseline.json")
    baseline, replaced_ids = native_reports.project_baseline(clean, reference["expected_cases"])
    okay, comparison = native_reports.compare(baseline, reference["expected_collection"], full,
                                              reference["expected_passes"], reference["expected_cases"])
    comparison["test_patch_replaced_identities"] = replaced_ids
    targets_okay = all(target["case_outcomes"].get(name) == "pass" for name in reference["required_targets"])
    verdict = "test_tampered" if tampered else "resolved" if okay and targets_okay else "failed"
    value = dict(full, schema="agentgo.swe-judge/v2", task_id=task.task_id, run_id=run_id, verdict=verdict,
                 patch_lines=len(patch.splitlines()), tampered=bool(tampered), tampered_files=tampered,
                 patch_scope='git-visible-production-files', ignored_source_artifacts=sorted(ignored_sources),
                 targeted=target, baseline_comparison=comparison, required_targets=reference["required_targets"],
                 criterion="required_targets_fixed_and_declared_regression_preserved",
                 regression_scope=task.metadata.get("regression_scope"), full_suite_green=not full["failure_events"] and not full["not_run"])
    save(out / "judge.json", value)
    print(json.dumps({"task": task.task_id, "verdict": verdict, "passed": full["passed"], "failed": full["failed"], "tampered": bool(tampered)}), flush=True)
    return value


def verify_candidates(config, api):
    counts, rows = collections.Counter(), []
    tasks = api.load_tasks(config.tasks_file, "native")
    selected = getattr(config, "task_filter", None)
    if selected:
        tasks = [task for task in tasks if task.task_id == selected]
        if not tasks:
            raise ValueError("unknown native task qualification selector")
    for task in tasks:
        try:
            with api.task_execution_lock(config, task.task_id):
                value = qualify(config, task, api)
            status = "OK"
            rows.append({"task": task.task_id, "status": status, "reference": str(qualification_dir(config, task) / "reference.json")})
        except Exception as error:
            status = "INVALID"
            rows.append({"task": task.task_id, "status": status, "error": api.safe_diagnostic(error)})
            print(json.dumps(rows[-1], ensure_ascii=True), flush=True)
        counts[status] += 1
        save(config.testbed / "test-runner" / "candidates_report.json", {"counts": dict(counts), "tasks": rows})
    return dict(counts)


def group_configs(config, api):
    for name in config.suite.options["members"]:
        directory = Path(__file__).parent / "suites" / name
        suite = Suite.load(directory)
        if suite.engine != "native":
            raise ValueError("native group contains an unsupported member")
        yield api.SWETestRunnerConfig(config.agentgo_root, config.agentgo_bin, config.testbed / name,
                                      directory / "tasks.json", directory / "prompts", config.testbed / "upstream" / suite.project,
                                      config.base_url, config.fast_model, config.flag_ship_model, config.protocol, suite)


def group_verify(config, api):
    total = collections.Counter()
    for member in group_configs(config, api):
        total.update(verify_candidates(member, api))
    return dict(total)


def group_batch(config, args, api):
    api.preflight_probe(config, args.probe_timeout)
    rows = []
    for member in group_configs(config, api):
        for task in api.load_tasks(member.tasks_file, "native"):
            try:
                result = api.execute_task(member, task, args.timeout)
                rows.append({"suite": member.suite.suite_id, "task": task.task_id, "task_resolved": result["task_resolved"],
                             "result_ref": str(member.run_dir(task.task_id) / "result.json")})
            except Exception as error:
                rows.append({"suite": member.suite.suite_id, "task": task.task_id, "task_resolved": False,
                             "error": api.safe_diagnostic(error)})
                save(config.testbed / "core-summary.json", {"expected": 96, "runs": rows})
                raise
            save(config.testbed / "core-summary.json", {"expected": 96, "runs": rows})
    return 0 if len(rows) == 96 and all(row["task_resolved"] for row in rows) else api.EXIT_TASK_FAILURE
