import importlib.util
import json
import os
from pathlib import Path
import platform
import struct
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import export_logs as exporter
from binary_input import inspect_binary


def module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    value = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(value)
    return value


host = module("swe_linux_host", HERE / "run.py")
entry = module("swe_linux_entry", HERE / "entrypoint.py")
prebuilt = module("prebuilt", HERE.parent / "scripts/swe_test_runner/prebuilt_environment.py")
probe = module("probe_archive", HERE.parent / "scripts/swe_test_runner/probe_archive.py")


def elf_fixture(path, machine=None):
    machine = machine or (183 if platform.machine().lower() in {"arm64", "aarch64"} else 62)
    raw = bytearray(120)
    raw[:7] = b"\x7fELF\x02\x01\x01"
    struct.pack_into("<HHI", raw, 16, 2, machine, 1)
    struct.pack_into("<Q", raw, 32, 64)
    struct.pack_into("<HHH", raw, 52, 64, 56, 1)
    struct.pack_into("<IIQQQQQQ", raw, 64, 1, 5, 0, 0x400000, 0x400000, len(raw), len(raw), 4096)
    path.write_bytes(raw)
    return path


class ConfigBoundaryTest(unittest.TestCase):
    def test_missing_config_precedes_build_side_effects(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with mock.patch.object(host.subprocess, "run") as run:
                with self.assertRaises(FileNotFoundError):
                    host.prepare_build(root / "config.json", "amd64", "unused", build_root=root / "build")
            run.assert_not_called()
            self.assertFalse((root / "build").exists())

    def test_host_accepts_empty_invalid_and_non_object_contents_without_reading(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "config.json"
            for raw in (b"", b" \n", b"invalid-json", b"null", b"[]", b"{}"):
                path.write_bytes(raw)
                with mock.patch.object(Path, "read_bytes", side_effect=AssertionError("must not read")), \
                     mock.patch.object(Path, "read_text", side_effect=AssertionError("must not read")):
                    self.assertEqual(host.require_config(path), path.resolve())

    def test_invalid_input_runs_and_exports_error_without_exporting_configuration(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            config = root / "config.json"
            binary = elf_fixture(root / "agentgo")
            for index, raw in enumerate((b"", b"\r\n ", b"not-json", b"null")):
                config.write_bytes(raw)
                batch_id = f"batch-{index}"
                result = subprocess.run([
                    sys.executable, str(HERE / "entrypoint.py"), "--config", str(config),
                    "--data", str(root / "data"), "--exports", str(root / "exports"), "--batch-id", batch_id,
                    "--agentgo-binary", str(binary),
                ], capture_output=True)
                self.assertEqual(result.returncode, 1)
                destination = root / "exports" / batch_id
                self.assertEqual((root / "data" / batch_id / "config.input.json").read_bytes(), raw)
                self.assertFalse((destination / "config.input.json").exists())
                self.assertFalse((destination / "environment.full.json").exists())
                status = json.loads((destination / "archive-status.json").read_text())
                self.assertTrue(status["complete"])
                self.assertEqual(status["profile"], "reports-and-behavior")
                self.assertTrue((destination / "runtime-error.json").is_file())
                exporter.verify_export(destination, batch_id)

    def test_environment_uses_local_values_and_never_inherits_provider_credentials(self):
        with mock.patch.dict(os.environ, {"SWE_API_KEY": "host-only", "SWE_FAST_MODEL": "host-only"}):
            environment, argv = entry.load_environment(b'{"environment":{"SWE_API_KEY":"intentionally-wrong"}}', Path("/data/batch"))
        self.assertEqual(environment["SWE_API_KEY"], "intentionally-wrong")
        self.assertNotIn("SWE_FAST_MODEL", environment)
        self.assertEqual(environment["AGENTGO_TRACE_KEEP_ALL"], "1")
        self.assertEqual(environment["SWE_TESTBED"], str(Path("/data/batch/testbed")))
        self.assertEqual(argv[-3:], ["batch", "--timeout", "1200"])

    def test_missing_runtime_config_does_not_create_batch(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            binary = elf_fixture(root / "agentgo")
            result = subprocess.run([sys.executable, str(HERE / "entrypoint.py"),
                                     "--config", str(root / "config.json"), "--data", str(root / "data"),
                                     "--agentgo-binary", str(binary)], capture_output=True)
            self.assertEqual(result.returncode, 1)
            self.assertFalse((root / "data").exists())


class BinaryInputTest(unittest.TestCase):
    def test_multiarch_parent_id_is_frozen_separately_from_platform_manifest(self):
        labels = {host.IMAGE_LABEL: host.IMAGE_CONTRACT, host.RUNTIME_LABEL: host.RUNTIME_CONTRACT, host.SUITE_LABEL: "flask-8"}
        child = {"Id": "sha256:child", "Os": "linux", "Architecture": "arm64", "Config": {"Labels": labels}}
        result = subprocess.CompletedProcess([], 0, stdout=b"sha256:parent\n")
        with mock.patch.object(host.subprocess, "run", return_value=result), \
             mock.patch.object(host, "capture", return_value=json.dumps(child)) as capture:
            identity = host.image_info("fixture:latest", "arm64")
        self.assertEqual(identity["Id"], "sha256:parent")
        self.assertEqual(identity["PlatformId"], "sha256:child")
        self.assertEqual(capture.call_args.args[0][-1], "sha256:parent")

    def test_missing_binary_stops_before_docker_and_artifacts(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            config = root / "config.json"
            config.write_text("")
            with mock.patch.object(host, "image_info") as image_info, mock.patch.object(host.subprocess, "run") as run:
                with self.assertRaisesRegex(ValueError, "--agentgo-binary"):
                    host.launch(config, root / "artifacts", "unused")
            run.assert_not_called()
            image_info.assert_not_called()
            self.assertFalse((root / "artifacts").exists())

    def test_rejects_host_platform_executables_and_truncated_elf(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "agentgo"
            for raw in (b"MZ" + bytes(100), b"\xcf\xfa\xed\xfe" + bytes(100), b"\x7fELF", bytes(100)):
                path.write_bytes(raw)
                with self.assertRaises(ValueError):
                    inspect_binary(path)

    def test_architecture_and_binary_identity(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            amd = elf_fixture(root / "amd", 62)
            arm = elf_fixture(root / "arm", 183)
            self.assertEqual(inspect_binary(amd)["platform"], "linux/amd64")
            self.assertEqual(inspect_binary(arm)["platform"], "linux/arm64")
            with self.assertRaisesRegex(ValueError, "架构|linux/arm64"):
                inspect_binary(amd, "arm64")
            first = inspect_binary(amd)["sha256"]
            with amd.open("ab") as stream:
                stream.write(b"different-build")
            self.assertNotEqual(inspect_binary(amd)["sha256"], first)

    def test_no_agentgo_binary_is_packaged_by_image_builder(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            config = root / "config.json"
            config.write_text("")
            with mock.patch.object(host.subprocess, "run") as run:
                context = host.prepare_build(config, "all", "fixture", build_root=root / "image")
            run.assert_not_called()
            self.assertFalse((context / "bin/agentgo").exists())
            self.assertFalse((context / "runtime/agentgo").exists())
            self.assertTrue((context / "runtime/scripts/swe_test_runner/runner.py").is_file())
            for role in ("worker", "explorer", "verifier"):
                self.assertTrue((context / "runtime/prompts" / (role + ".md")).is_file())
            identity = json.loads((context / "image.json").read_text(encoding="utf-8"))
            self.assertEqual(identity["suite"], "flask-8")
            self.assertEqual(identity["platforms"], ["linux/amd64", "linux/arm64"])

    def test_build_context_excludes_private_files_beside_runner_sources(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            config = root / "config.json"
            marker = "private-config-fixture:" + root.name
            config.write_text(marker, encoding="utf-8")
            source = host.prepare_build(config, "amd64", "fixture", build_root=root / "source-image") / "runtime"
            runner = source / "scripts" / "swe_test_runner"
            for relative in ("config.json", ".env", "probe.json", "provider-probes/response.body",
                             "worktrees/private.txt", "suites/flask-8/private.json"):
                path = runner / relative
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(marker, encoding="utf-8")
            context = host.prepare_build(config, "amd64", "fixture", root=source, build_root=root / "image")
            for path in context.rglob("*"):
                if path.is_file():
                    self.assertFalse(marker.encode() in path.read_bytes(), str(path.relative_to(context)))
            self.assertTrue((context / "runtime/SWE_LinuxContainers/entrypoint.py").is_file())
            self.assertTrue((context / "runtime/SWE_LinuxContainers/config.json.example").is_file())


class EvidenceTest(unittest.TestCase):
    def test_archive_io_failure_preserves_working_evidence_and_incomplete_status(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            batch = root / "batch"
            batch.mkdir()
            (batch / "keep.json").write_text("{}")
            with mock.patch.object(exporter, "copy_verified", side_effect=OSError("disk-full-fixture")):
                with self.assertRaises(OSError):
                    entry.export_batch(batch, root / "exports")
            self.assertTrue((batch / "keep.json").is_file())
            self.assertFalse(json.loads((root / "exports/archive-status.json").read_text())["complete"])

    def test_export_keeps_behavior_but_excludes_workspaces_and_config(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            old = root / "exports" / "old"
            old.mkdir(parents=True)
            (old / "keep").write_text("old")
            batch = root / "data" / "new"
            state = batch / "testbed/worktrees/task/.agentgo/state"
            state.mkdir(parents=True)
            (state / "facts.jsonl").write_text('{"final":true}\n')
            (batch / "config.input.json").write_text('{"environment":{"SWE_API_KEY":"fixture"}}')
            fixtures = {
                "environment.full.json": '{}', "source.tar.gz": 'source-archive',
                "testbed/worktrees/task/.venv/python": 'venv',
                "testbed/worktrees/task/.git/config": 'git-config',
                "testbed/worktrees/task/src/flask.py": 'source',
                "testbed/worktrees/task/.agentgo/candidates-v1/candidate/source.py": 'candidate',
                "testbed/worktrees/task/.agentgo/state/content/tool-output": 'complete tool body',
                "testbed/worktrees/task/.agentgo/sessions/s1/turns.jsonl": '{"text":"model reply"}\n',
                "testbed/worktrees/task/.agentgo/sessions/s1/logs/trace.jsonl": '{}\n',
                "testbed/runs/task/setting.yaml": 'token: fixture',
                "testbed/runs/task/model.patch": 'source patch',
                "testbed/runs/task/judge.json": '{"verdict":"resolved"}',
                "testbed/runs/task/judge.pytest.log": 'test output',
            }
            for relative, text in fixtures.items():
                path = batch / relative
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(text)
            entry.export_batch(batch, root / "exports/new", recovered=True)
            self.assertEqual((old / "keep").read_text(), "old")
            self.assertTrue((state / "facts.jsonl").exists())
            destination = root / "exports/new"
            self.assertTrue((destination / "behavior/task/state/facts.jsonl").is_file())
            self.assertTrue((destination / "behavior/task/state/content/tool-output").is_file())
            self.assertTrue((destination / "behavior/task/sessions/s1/turns.jsonl").is_file())
            self.assertTrue((destination / "runs/task/judge.json").is_file())
            self.assertTrue((destination / "runs/task/judge.pytest.log").is_file())
            for relative in ("config.input.json", "environment.full.json", "source.tar.gz", "evidence.tar.gz",
                             "runs/task/model.patch", "runs/task/setting.yaml", "behavior/task/candidates-v1"):
                self.assertFalse((destination / relative).exists(), relative)
            exporter.verify_export(destination, "new")
            self.assertFalse(json.loads((batch / "recovery.json").read_text())["execution_complete"])

    def test_symlinked_behavior_cannot_export_outside_files(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            outside = root / "outside"
            outside.mkdir()
            (outside / "secret.json").write_text('{}')
            batch = root / "batch"
            state = batch / "testbed/worktrees/task/.agentgo"
            state.mkdir(parents=True)
            try:
                (state / "state").symlink_to(outside, target_is_directory=True)
            except OSError:
                self.skipTest("platform does not permit symlink creation")
            with self.assertRaises(RuntimeError):
                exporter.export_batch(batch, root / "exports")
            self.assertFalse((root / "exports/behavior/task/state/secret.json").exists())

    def test_probe_preserves_received_prefix_on_stream_error_without_authorization(self):
        with tempfile.TemporaryDirectory() as temporary:
            with mock.patch.dict(os.environ, {"SWE_PROBE_ARCHIVE": temporary}):
                with self.assertRaises(RuntimeError):
                    with probe.record_probe("https://example.invalid/responses", {"model": "fixture"}) as archive:
                        list(archive.lines([b"data: part\n"]))
                        raise RuntimeError("fixture-error")
            path = next(Path(temporary).iterdir())
            self.assertEqual((path / "response.body").read_bytes(), b"data: part\n")
            self.assertFalse(json.loads((path / "transport.json").read_text())["completed"])
            self.assertNotIn("Authorization", (path / "request.json").read_text())

    def test_prebuilt_environment_relocates_editable_flask_and_checks_identity(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            worktree, template = root / "worktree", root / "envs/task"
            worktree.mkdir()
            packages = template / "venv/lib/python3.13/site-packages"
            packages.mkdir(parents=True)
            (packages / "flask.pth").write_text("/old/build/src\n")
            (worktree / "uv.lock").write_text("fixture")
            import hashlib
            identity = {"task_id": "task", "fix_sha": "fix", "build_root": "/old/build",
                        "inputs": {"uv.lock": hashlib.sha256(b"fixture").hexdigest()}}
            (template / "manifest.json").write_text(json.dumps(identity))
            prebuilt.install_prebuilt(root / "envs", "task", "fix", worktree)
            self.assertEqual((worktree / ".venv/lib/python3.13/site-packages/flask.pth").read_text(), worktree.as_posix() + "/src\n")
            (worktree / "uv.lock").write_text("changed")
            with self.assertRaisesRegex(RuntimeError, "uv.lock"):
                prebuilt.install_prebuilt(root / "envs", "task", "fix", worktree)

    def test_pip_launcher_and_editable_metadata_are_relocated(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            build_root, worktree = root / "builder", root / "worktree"
            worktree.mkdir()
            template = root / "envs/task"
            binaries = template / "venv/bin"
            binaries.mkdir(parents=True)
            (binaries / "pip").write_text(f"#!{build_root.as_posix()}/.venv/bin/python\nprint('fixture')\n")
            packages = template / "venv/lib/python3.13/site-packages/flask.dist-info"
            packages.mkdir(parents=True)
            (packages / "direct_url.json").write_text(json.dumps({"url": build_root.as_uri(), "dir_info": {"editable": True}}))
            manifest = {"task_id": "task", "fix_sha": "fix", "inputs": {}, "build_root": build_root.as_posix(),
                        "venv_root": (build_root / ".venv").as_posix(), "machine": platform.machine()}
            (template / "manifest.json").write_text(json.dumps(manifest))
            prebuilt.install_prebuilt(root / "envs", "task", "fix", worktree)
            self.assertIn((worktree / ".venv/bin/python").as_posix(), (worktree / ".venv/bin/pip").read_text())
            direct = json.loads((worktree / ".venv/lib/python3.13/site-packages/flask.dist-info/direct_url.json").read_text())
            self.assertEqual(direct["url"], worktree.as_uri())


class LifecycleTest(unittest.TestCase):
    def test_failed_create_discards_only_a_proven_unstarted_owned_volume(self):
        for existing in ("", "unexpected-container"):
            with tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                batch = "create-fixture"
                volume = host.VOLUME_PREFIX + batch
                with mock.patch.object(host, "capture", side_effect=[existing, json.dumps({host.OWNER_LABEL: batch})]), \
                     mock.patch.object(host.subprocess, "run") as run:
                    host.discard_unstarted_volume(volume, batch, root)
                if existing:
                    run.assert_not_called()
                else:
                    self.assertEqual(run.call_args.args[0], ["docker", "volume", "rm", volume])

    def fixture(self, root):
        batch = root / "batch-fixture"
        batch.mkdir()
        (batch / "process-exit.json").write_text('{"exit_code":0}')
        destination = root / "exports"
        exporter.export_batch(batch, destination)
        return batch.name, destination, host.VOLUME_PREFIX + batch.name

    def test_default_removes_only_batch_container_then_volume(self):
        with tempfile.TemporaryDirectory() as temporary:
            batch, destination, volume = self.fixture(Path(temporary))
            labels = json.dumps({host.OWNER_LABEL: batch})
            with mock.patch.object(host, "capture", side_effect=[labels, labels, "false"]), \
                 mock.patch.object(host.subprocess, "run") as run:
                self.assertTrue(host.finish_resources("own-container-id", volume, batch, destination))
            self.assertEqual([call.args[0] for call in run.call_args_list], [
                ["docker", "rm", "own-container-id"], ["docker", "volume", "rm", volume]])
            receipt = json.loads((destination / "cleanup.json").read_text())
            self.assertTrue(receipt["container_removed"] and receipt["volume_removed"])
            self.assertTrue((destination / "process-exit.json").is_file())

    def test_persist_keeps_container_and_volume(self):
        with tempfile.TemporaryDirectory() as temporary:
            batch, destination, volume = self.fixture(Path(temporary))
            with mock.patch.object(host.subprocess, "run") as run, mock.patch.object(host, "capture") as capture:
                self.assertTrue(host.finish_resources("own-id", volume, batch, destination, persist=True))
            run.assert_not_called()
            capture.assert_not_called()
            receipt = json.loads((destination / "cleanup.json").read_text())
            self.assertEqual(receipt["reason"], "persist_flag")

    def test_corrupt_export_prevents_any_cleanup(self):
        with tempfile.TemporaryDirectory() as temporary:
            batch, destination, volume = self.fixture(Path(temporary))
            (destination / "process-exit.json").write_text("corrupted")
            with mock.patch.object(host.subprocess, "run") as run, mock.patch.object(host, "capture") as capture:
                self.assertFalse(host.finish_resources("own-id", volume, batch, destination))
            run.assert_not_called()
            capture.assert_not_called()
            self.assertTrue(json.loads((destination / "cleanup.json").read_text())["retained"])

    def test_shared_or_foreign_volume_is_never_deleted(self):
        for foreign in ("agentgo-swe-linux-data", "wrong-owner"):
            with tempfile.TemporaryDirectory() as temporary:
                batch, destination, volume = self.fixture(Path(temporary))
                selected = foreign if foreign != "wrong-owner" else volume
                with mock.patch.object(host, "capture", side_effect=['{}', '{}']), \
                     mock.patch.object(host.subprocess, "run") as run:
                    self.assertFalse(host.finish_resources("own-id", selected, batch, destination))
                run.assert_not_called()

    def test_running_container_is_not_force_removed(self):
        with tempfile.TemporaryDirectory() as temporary:
            batch, destination, volume = self.fixture(Path(temporary))
            labels = json.dumps({host.OWNER_LABEL: batch})
            with mock.patch.object(host, "capture", side_effect=[labels, labels, "true"]), \
                 mock.patch.object(host.subprocess, "run") as run:
                self.assertFalse(host.finish_resources("own-id", volume, batch, destination))
            run.assert_not_called()

    def test_partial_cleanup_failure_is_reported(self):
        with tempfile.TemporaryDirectory() as temporary:
            batch, destination, volume = self.fixture(Path(temporary))
            labels = json.dumps({host.OWNER_LABEL: batch})
            with mock.patch.object(host, "capture", side_effect=[labels, labels, "false"]), \
                 mock.patch.object(host.subprocess, "run", side_effect=[None, subprocess.CalledProcessError(1, "docker")]):
                self.assertFalse(host.finish_resources("own-id", volume, batch, destination))
            receipt = json.loads((destination / "cleanup.json").read_text())
            self.assertTrue(receipt["container_removed"])
            self.assertFalse(receipt["volume_removed"] or receipt["complete"])


if __name__ == "__main__":
    unittest.main()
