import errno
import json
from dataclasses import replace
import os
from pathlib import Path
import shutil
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch
import zipfile

import artifacts
import cleanup
import lifecycle
import storage
from storage_test import small_policy


class ArtifactLifecycleTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = storage.StorageRoot(self.temp.name, small_policy())
        self.attempt = storage.Attempt(self.root, self.root.batch({}), "fixture", "task")
        (self.attempt.worktree / "source.py").write_bytes(b"print('candidate')\n")
        (self.attempt.reports / "result.json").write_text('{"task_resolved":true}', encoding="utf-8")

    def test_archive_member_mapping_preserves_names_and_reserves_its_namespace(self):
        name = 'scratch/foo\udcffbar'
        mapped = artifacts._zip_member_name(name)
        mapped.encode('ascii')
        self.assertNotEqual(mapped, artifacts._zip_member_name(mapped))
        self.assertNotEqual(mapped, artifacts._zip_member_name('scratch/foo\ufffdbar'))
        manifest = {'path_encoding': artifacts.PATH_ENCODING}
        self.assertEqual(artifacts.archive_member_name({'path': name, 'member': mapped}, manifest), mapped)
        with self.assertRaisesRegex(storage.StorageError, 'mapping_mismatch'):
            artifacts.archive_member_name({'path': name, 'member': 'reports/result.json'}, manifest)
        with self.assertRaisesRegex(storage.StorageError, 'encoding_missing'):
            artifacts.archive_member_name({'path': name, 'member': mapped}, {})
        with self.assertRaises(storage.StorageError):
            artifacts._zip_member_name('scratch/../outside')

    def test_legacy_archive_without_path_mapping_stays_readable(self):
        manifest = artifacts.seal(self.attempt)
        data = storage.read_json(manifest)
        self.assertTrue(all('member' not in record for record in data['files']))
        data['schema'] = artifacts.LEGACY_SCHEMA
        for field in ('path_encoding', 'filesystem_encoding', 'filesystem_errors'):
            data.pop(field)
        storage.atomic_json(manifest, data)
        artifacts.verify(manifest)
        cleanup.apply(cleanup.plan(self.attempt, manifest))
        self.assertEqual(artifacts.read_member(manifest, 'worktree/source.py'), b"print('candidate')\n")

    @unittest.skipUnless(os.name == 'posix', 'requires POSIX filename bytes')
    def test_non_utf8_filename_and_link_survive_archive_cleanup_and_mapping_tamper_rejection(self):
        filename = os.fsdecode(b'foo\xffbar')
        path = self.attempt.scratch / filename
        try:
            path.write_bytes(b'filename-byte fixture')
        except OSError as error:
            if error.errno in (errno.EINVAL, errno.EILSEQ):
                self.skipTest('filesystem does not support non-UTF-8 names')
            raise
        (self.attempt.scratch / 'link-to-byte-name').symlink_to(filename)
        first_manifest = artifacts.seal(self.attempt)
        first = artifacts.verify(first_manifest)
        name = 'scratch/' + filename
        first_record = next(record for record in first['files'] if record['path'] == name)
        reserved = self.attempt.path / first_record['member']
        reserved.parent.mkdir(parents=True)
        reserved.write_bytes(b'ordinary source in reserved namespace')
        manifest = artifacts.seal(self.attempt)
        data = artifacts.verify(manifest)
        record = next(item for item in data['files'] if item['path'] == name)
        reserved_record = next(item for item in data['files'] if item['path'] == first_record['member'])
        self.assertNotEqual(record['member'], reserved_record['member'])
        with zipfile.ZipFile(manifest.parent / data['archive']) as archive:
            self.assertEqual(archive.read('scratch/link-to-byte-name'), b'foo\xffbar')
        saved_member = record['member']
        record['member'] = 'reports/result.json'
        storage.atomic_json(manifest, data)
        with self.assertRaisesRegex(storage.StorageError, 'mapping_mismatch'):
            artifacts.verify(manifest)
        self.assertTrue(path.exists())
        record['member'] = saved_member
        storage.atomic_json(manifest, data)
        plan = cleanup.plan(self.attempt, manifest)
        self.assertTrue(cleanup.apply(plan)['complete'])
        self.assertTrue(cleanup.apply(plan)['complete'])
        self.assertFalse(path.exists())
        self.assertEqual(artifacts.read_member(manifest, name), b'filename-byte fixture')
        self.assertEqual(artifacts.read_member(manifest, first_record['member']), b'ordinary source in reserved namespace')

    def test_verified_archive_survives_cleanup_and_second_cleanup(self):
        (self.attempt.reports / "setting.yaml").write_text("private: fixture-value")
        manifest = artifacts.seal(self.attempt)
        value = artifacts.verify(manifest)
        self.assertNotIn("reports/setting.yaml", [x["path"] for x in value["files"]])
        plan = cleanup.plan(self.attempt, manifest)
        receipt = cleanup.apply(plan)
        self.assertTrue(receipt["complete"])
        self.assertFalse(self.attempt.worktree.exists())
        self.assertFalse((self.attempt.reports / "setting.yaml").exists())
        self.assertTrue(self.attempt.reports.is_dir())
        self.assertEqual(artifacts.read_member(manifest, "worktree/source.py"), b"print('candidate')\n")
        self.assertTrue(cleanup.apply(plan)["complete"])

    def test_interrupted_cleanup_resumes_without_losing_archive_or_remaining_resources(self):
        (self.attempt.scratch / "pending").write_bytes(b"scratch evidence")
        manifest = artifacts.seal(self.attempt)
        plan = cleanup.plan(self.attempt, manifest)
        remove = cleanup.remove_owned

        def interrupt_before_scratch(root, target, identity):
            if target == self.attempt.scratch:
                raise KeyboardInterrupt("fixture interruption")
            return remove(root, target, identity)

        with patch.object(cleanup, "remove_owned", side_effect=interrupt_before_scratch):
            with self.assertRaises(KeyboardInterrupt):
                cleanup.apply(plan)
        receipt = storage.read_json(plan.with_name("receipt.json"))
        self.assertFalse(receipt["complete"])
        self.assertEqual(receipt["error_code"], "KeyboardInterrupt")
        self.assertFalse(self.attempt.worktree.exists())
        self.assertEqual((self.attempt.scratch / "pending").read_bytes(), b"scratch evidence")
        self.assertEqual(artifacts.read_member(manifest, "worktree/source.py"), b"print('candidate')\n")
        self.assertTrue(cleanup.apply(plan)["complete"])
        self.assertFalse(self.attempt.scratch.exists())
        self.assertTrue(cleanup.apply(plan)["complete"])
        artifacts.verify(manifest)

    def test_interrupted_cleanup_refuses_replacement_before_deleting_remaining_resources(self):
        (self.attempt.scratch / "pending").write_bytes(b"retain until verified")
        plan = cleanup.plan(self.attempt, artifacts.seal(self.attempt))
        remove = cleanup.remove_owned

        def interrupt_before_scratch(root, target, identity):
            if target == self.attempt.scratch:
                raise KeyboardInterrupt("fixture interruption")
            return remove(root, target, identity)

        with patch.object(cleanup, "remove_owned", side_effect=interrupt_before_scratch):
            with self.assertRaises(KeyboardInterrupt):
                cleanup.apply(plan)
        self.attempt.worktree.mkdir()
        (self.attempt.worktree / "replacement").write_bytes(b"new owner")
        with self.assertRaisesRegex(storage.StorageError, "resource_identity_changed"):
            cleanup.apply(plan)
        self.assertEqual((self.attempt.worktree / "replacement").read_bytes(), b"new owner")
        self.assertEqual((self.attempt.scratch / "pending").read_bytes(), b"retain until verified")
        self.assertFalse(storage.read_json(plan.with_name("receipt.json"))["complete"])

    def test_registered_environment_copy_is_omitted_but_source_fixture_survives(self):
        environment = self.attempt.worktree / "node_modules"
        environment.mkdir()
        (environment / "dependency.js").write_text("rebuildable")
        self.attempt.register(environment, "owned", "rebuildable_environment", archive=False)
        self.attempt.mark_rebuildable_resources()
        copied = self.attempt.worktree / "copies" / "candidate" / "node_modules"
        shutil.copytree(environment, copied)
        fixture = self.attempt.worktree / "tests" / "node_modules"
        fixture.mkdir(parents=True)
        (fixture / "fixture.js").write_text("unique source evidence")
        manifest = artifacts.verify(artifacts.seal(self.attempt))
        names = {item["path"] for item in manifest["files"]}
        self.assertNotIn("worktree/copies/candidate/node_modules/dependency.js", names)
        self.assertIn("worktree/tests/node_modules/fixture.js", names)
        self.assertIn("worktree/source.py", names)
        self.assertEqual([item["path"] for item in manifest["omitted_copies"]],
                         ["worktree/copies/candidate/node_modules"])
        self.assertTrue((copied / "dependency.js").is_file())

    def test_missing_or_modified_marker_and_renamed_directory_are_archived(self):
        environment = self.attempt.worktree / "node_modules"
        environment.mkdir()
        (environment / "dependency.js").write_text("rebuildable")
        self.attempt.register(environment, "owned", "rebuildable_environment", archive=False)
        self.attempt.mark_rebuildable_resources()
        copied = self.attempt.worktree / "copies" / "node_modules"
        shutil.copytree(environment, copied)
        (copied / storage.REBUILDABLE_MARKER).write_text("unrecognized marker")
        partial = self.attempt.worktree / "partial-copy" / "node_modules"
        shutil.copytree(environment, partial)
        # A previously marked copy can lose its marker during interrupted
        # disposal. Its remaining files are evidence, not an omission by name.
        (partial / storage.REBUILDABLE_MARKER).unlink()
        renamed = self.attempt.worktree / "unique-source"
        shutil.copytree(environment, renamed)
        manifest_path = artifacts.seal(self.attempt)
        manifest = artifacts.verify(manifest_path)
        names = {item["path"] for item in manifest["files"]}
        self.assertIn("worktree/copies/node_modules/dependency.js", names)
        self.assertIn("worktree/partial-copy/node_modules/dependency.js", names)
        self.assertIn("worktree/unique-source/dependency.js", names)
        self.assertEqual(manifest["omitted_copies"], [])
        self.assertEqual(artifacts.read_member(manifest_path,
                         "worktree/partial-copy/node_modules/dependency.js"), b"rebuildable")

    def test_environment_marker_collision_does_not_overwrite_existing_file(self):
        environment = self.attempt.worktree / "node_modules"
        environment.mkdir()
        marker = environment / storage.REBUILDABLE_MARKER
        marker.write_text("existing unrelated file")
        self.attempt.register(environment, "owned", "rebuildable_environment", archive=False)
        with self.assertRaisesRegex(storage.StorageError, "rebuildable_marker_collision"):
            self.attempt.mark_rebuildable_resources()
        self.assertEqual(marker.read_text(), "existing unrelated file")

    def test_prepared_environment_is_marked_before_baseline_and_marking_is_idempotent(self):
        environment = self.attempt.worktree / "node_modules"
        environment.mkdir()
        self.attempt.register(environment, "owned", "rebuildable_environment", archive=False)
        with self.attempt.phase("clean"):
            marker = environment / storage.REBUILDABLE_MARKER
            self.assertTrue(marker.is_file())
            resources_before = (self.attempt.path / "resources.jsonl").read_bytes()
            self.attempt.mark_rebuildable_resources()
            self.assertEqual((self.attempt.path / "resources.jsonl").read_bytes(), resources_before)

    def test_archive_corruption_prevents_all_deletion(self):
        manifest = artifacts.seal(self.attempt)
        plan = cleanup.plan(self.attempt, manifest)
        with (manifest.parent / "evidence.zip").open("ab") as out:
            out.write(b"corrupt")
        with self.assertRaisesRegex(storage.StorageError, "archive_digest_mismatch"):
            cleanup.apply(plan)
        self.assertTrue(self.attempt.worktree.exists())

    def test_retained_failure_discards_generated_private_configuration(self):
        for name in ("setting.yaml", "setting.yaml.tmp"):
            (self.attempt.reports / name).write_text("ephemeral authentication: fixture")
        manifest = artifacts.seal(self.attempt)
        receipt = cleanup.apply(cleanup.plan(self.attempt, manifest, retained=True, reason="fixture failure"))
        self.assertTrue(receipt["retained"])
        self.assertTrue(self.attempt.worktree.exists())
        self.assertTrue((self.attempt.reports / "result.json").exists())
        self.assertFalse((self.attempt.reports / "setting.yaml").exists())
        self.assertFalse((self.attempt.reports / "setting.yaml.tmp").exists())

    def test_full_history_stops_new_batch_without_discarding_evidence(self):
        manifest = artifacts.seal(self.attempt)
        size = storage.measure_tree(self.root.path / "archives")["logical_bytes"]
        self.root.policy = replace(self.root.policy, archive_max_bytes=size, archive_reserve_bytes=1)
        previous_batches = list((self.root.path / "batches").iterdir())
        with self.assertRaisesRegex(storage.StorageError, "archive_budget_exceeded"):
            self.root.batch({})
        with self.assertRaisesRegex(storage.StorageError, "archive_budget_exceeded"):
            artifacts.seal(self.attempt)
        self.assertEqual(previous_batches, list((self.root.path / "batches").iterdir()))
        self.assertTrue(artifacts.verify(manifest)["complete"])
        self.assertTrue(self.attempt.worktree.exists())

    def test_incompressible_output_is_bounded_and_partial_is_counted(self):
        self.root.policy = replace(self.root.policy, archive_max_bytes=48 * 1024)
        data = os.urandom(256 * 1024)
        source = self.attempt.worktree / "database"
        source.write_bytes(data)
        with self.assertRaisesRegex(storage.StorageError, "archive_budget_exceeded"):
            artifacts.seal(self.attempt)
        usage = storage.measure_tree(self.root.path / "archives")["logical_bytes"]
        self.assertLessEqual(usage, self.root.policy.archive_max_bytes)
        self.assertEqual(self.root.archive_capacity(), self.root.policy.archive_max_bytes - usage)
        self.assertEqual(source.read_bytes(), data)
        manifest = next((self.root.path / "archives").rglob("manifest.json"))
        with self.assertRaisesRegex(storage.StorageError, "archive_incomplete"):
            artifacts.verify(manifest)

    def test_compressible_source_can_exceed_archive_byte_budget(self):
        self.root.policy = replace(self.root.policy, archive_max_bytes=128 * 1024)
        (self.attempt.worktree / "large-log").write_bytes(b"0" * 1024 * 1024)
        manifest = artifacts.seal(self.attempt)
        data = artifacts.verify(manifest)
        self.assertGreater(data["source_bytes"], self.root.policy.archive_max_bytes)
        self.assertLess(storage.measure_tree(self.root.path / "archives")["logical_bytes"], self.root.policy.archive_max_bytes)

    def test_modified_plan_cannot_delete_cache(self):
        manifest = artifacts.seal(self.attempt)
        plan = cleanup.plan(self.attempt, manifest)
        value = storage.read_json(plan)
        value["resources"][-1]["path"] = "cache"
        value["resources"][-1]["identity"] = storage.file_identity(self.root.path / "cache")
        storage.atomic_json(plan, value)
        with self.assertRaisesRegex(storage.StorageError, "cleanup_target_not_allowed"):
            cleanup.apply(plan)
        self.assertTrue(self.attempt.worktree.exists())
        self.assertTrue((self.root.path / "cache").is_dir())

    def test_recreated_path_after_completed_cleanup_is_not_removed(self):
        plan = cleanup.plan(self.attempt, artifacts.seal(self.attempt))
        cleanup.apply(plan)
        self.attempt.worktree.mkdir()
        (self.attempt.worktree / "new-work").write_text("retain")
        with self.assertRaisesRegex(storage.StorageError, "identity_changed"):
            cleanup.apply(plan)
        self.assertTrue((self.attempt.worktree / "new-work").is_file())

    def test_unfinished_process_receipt_prevents_cleanup(self):
        directory = self.attempt.path / "commands" / "running"
        directory.mkdir()
        storage.atomic_json(directory / "command.json", {"schema": "agentgo.swe-command/v1", "quiescent": False})
        plan = cleanup.plan(self.attempt, artifacts.seal(self.attempt))
        with self.assertRaisesRegex(storage.StorageError, "process_quiescence_unknown"):
            cleanup.apply(plan)
        self.assertTrue(self.attempt.worktree.is_dir())

    def test_borrowed_link_is_archived_as_data_without_following(self):
        target = self.root.path / "borrowed"
        target.write_text("must stay")
        link = self.attempt.worktree / "link"
        try:
            link.symlink_to(target)
        except OSError:
            self.skipTest("symlink not permitted")
        manifest = artifacts.seal(self.attempt)
        data = artifacts.verify(manifest)
        self.assertEqual(next(x for x in data["files"] if x["path"] == "worktree/link")["kind"], "link")
        with self.assertRaisesRegex(storage.StorageError, "archive_file_unavailable"):
            artifacts.read_member(manifest, "worktree/link")
        cleanup.apply(cleanup.plan(self.attempt, manifest))
        self.assertEqual(target.read_text(), "must stay")

    def test_failed_attempt_retains_reproduction_and_rerun_is_independent(self):
        session = lifecycle.Session(self.root.path, small_policy(), {"fixture": True})
        config = SimpleNamespace(suite=SimpleNamespace(suite_id="fixture"))
        def failed(attempt):
            (attempt.worktree / "database").write_bytes(b"reproduction")
            raise ValueError("fixture failure")
        with self.assertRaises(ValueError):
            session.execute(config, "task", "qualification", failed)
        old = config._managed_worktrees["task"]
        self.assertTrue((old / "database").exists())
        result = session.execute(config, "task", "qualification", lambda a: {"qualified": True})
        self.assertTrue(result["qualified"])
        self.assertNotEqual(old, config._managed_worktrees["task"])
        self.assertFalse(config._managed_worktrees["task"].exists())
        self.assertTrue(old.exists())


if __name__ == "__main__":
    unittest.main()
