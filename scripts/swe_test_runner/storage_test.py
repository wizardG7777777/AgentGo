"""Small deterministic storage tests: never fill the host disk."""
from dataclasses import replace
import json
import os
from pathlib import Path
import tempfile
import subprocess
import threading
import time
import unittest
from unittest import mock

import storage


def small_policy(**values):
    defaults = dict(min_free_bytes=1, shutdown_reserve_bytes=1, archive_reserve_bytes=1,
                    max_attempt_bytes=16 * 1024 ** 2, max_live_bytes=256 * 1024 ** 2,
                    cache_max_bytes=128 * 1024 ** 2, require_wsl_host_space=False, max_scratch_prefix_bytes=240)
    defaults.update(values)
    return replace(storage.StoragePolicy(), **defaults)


class StorageTest(unittest.TestCase):
    def test_scan_budget_must_fit_sample_freshness(self):
        record = storage.StoragePolicy().record()
        self.assertEqual(storage.StoragePolicy.from_record(record).max_scan_seconds, 450)
        with self.assertRaisesRegex(storage.StorageError, 'inventory age'):
            storage.StoragePolicy.from_record({**record, 'max_inventory_age_seconds': 150})

    def test_logical_measurement_does_not_query_file_ids_or_claim_unique_bytes(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            a, b = root / 'a', root / 'b'
            a.write_bytes(b'abc')
            os.link(a, b)
            real_stat = Path.stat
            def no_file_stat(path, *args, **kwargs):
                if path in (a, b):
                    raise AssertionError('logical measurement queried file identity')
                return real_stat(path, *args, **kwargs)
            with mock.patch.object(Path, 'stat', no_file_stat):
                result = storage.measure_tree(root, deduplicate=False)
            self.assertEqual(result['logical_bytes'], 6)
            self.assertIsNone(result['unique_file_bytes'])
            self.assertIsNone(result['allocated_bytes'])
            self.assertEqual(storage.measure_tree(root)['unique_file_bytes'], 3)

    @unittest.skipUnless(os.name == 'nt', 'native Windows directory publication')
    def test_parent_can_be_renamed_during_nested_file_metadata_measurement(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            source, target = root / 'input-build', root / 'workspace-input'
            nested = source / 'dependencies'
            nested.mkdir(parents=True)
            data = nested / 'data'
            data.write_bytes(b'abc')
            real_scandir = os.scandir
            renamed = []
            class Entry:
                def __init__(self, entry): self.entry, self.path = entry, entry.path
                def stat(self, **kwargs):
                    info = self.entry.stat(**kwargs)
                    if Path(self.path) == data and not renamed:
                        self_outer.assertTrue(source.resolve().is_relative_to(root))
                        self_outer.assertTrue(target.resolve().is_relative_to(root))
                        source.rename(target)
                        target.rename(source)
                        renamed.append(True)
                    return info
            class Entries:
                def __init__(self, path): self.iterator = real_scandir(path)
                def __enter__(self): return self
                def __exit__(self, *args): self.iterator.close()
                def __iter__(self): return (Entry(entry) for entry in self.iterator)
            self_outer = self
            with mock.patch.object(storage.os, 'scandir', side_effect=Entries):
                usage = storage.measure_tree(source)
            self.assertEqual(renamed, [True])
            self.assertEqual(usage['logical_bytes'], 3)

    def test_link_and_parent_escape_cannot_be_cleanup_targets(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            with self.assertRaisesRegex(storage.StorageError, "refusing_storage_root"):
                storage.contained(root, root)
            with self.assertRaisesRegex(storage.StorageError, "outside_root"):
                storage.contained(root, root.parent / "elsewhere")
            outside = root / "outside"
            outside.mkdir()
            link = root / "link"
            try:
                link.symlink_to(outside, target_is_directory=True)
            except OSError:
                self.skipTest("OS does not permit symlink creation")
            with self.assertRaisesRegex(storage.StorageError, "path_is_link"):
                storage.contained(root, link / "new")

    def test_unique_hardlink_bytes_do_not_double_count(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / "a").write_bytes(b"x" * 4096)
            os.link(root / "a", root / "b")
            (root / "independent").write_bytes(b"y" * 2048)
            result = storage.measure_tree(root)
            self.assertEqual(result["logical_bytes"], 10240)
            self.assertEqual(result["unique_file_bytes"], 6144)

    def test_partitioned_inventory_counts_current_once_and_deduplicates_links(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            current = root / "current"
            current.mkdir()
            (current / "a").write_bytes(b"x" * 4096)
            os.link(current / "a", root / "b")
            (root / "independent").write_bytes(b"y" * 2048)
            expected = storage.measure_tree(root)
            seen = set()
            actual = storage.combine_usage(storage.measure_tree(current, _seen=seen),
                storage.measure_tree(root, excluded=(current,), _seen=seen))
            self.assertEqual(actual, expected)

    def test_inventory_never_follows_directory_link(self):
        with tempfile.TemporaryDirectory() as temp:
            base = Path(temp)
            root, outside = base / "owned", base / "outside"
            root.mkdir()
            outside.mkdir()
            (root / "small").write_bytes(b"123")
            (outside / "large").write_bytes(b"x" * 8192)
            link = root / "directory-link"
            try:
                link.symlink_to(outside, target_is_directory=True)
            except OSError:
                self.skipTest("symlink not permitted")
            usage = storage.measure_tree(root)
            self.assertEqual(usage["logical_bytes"], 3)
            self.assertEqual(usage["links"], 1)

    @unittest.skipUnless(os.name == "nt", "Windows junction metadata")
    def test_inventory_never_follows_windows_junction(self):
        with tempfile.TemporaryDirectory() as temp:
            base = Path(temp)
            root, outside = base / "owned", base / "outside"
            root.mkdir()
            outside.mkdir()
            (root / "small").write_bytes(b"123")
            (outside / "large").write_bytes(b"x" * 8192)
            script = "$p=[Console]::In.ReadToEnd()|ConvertFrom-Json; New-Item -ItemType Junction -Path $p.link -Target $p.target | Out-Null"
            subprocess.run(["powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script],
                input=json.dumps({"link": str(root / "junction"), "target": str(outside)}).encode(),
                capture_output=True, check=True, creationflags=subprocess.CREATE_NO_WINDOW)
            usage = storage.measure_tree(root)
            self.assertEqual(usage["logical_bytes"], 3)
            self.assertEqual(usage["links"], 1)

    def test_existing_unowned_directories_are_not_adopted(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / "batches").mkdir()
            with self.assertRaisesRegex(storage.StorageError, "unowned_managed"):
                storage.StorageRoot(root, small_policy())

    def test_root_identity_is_verified_before_reopening(self):
        with tempfile.TemporaryDirectory() as temp:
            root = storage.StorageRoot(temp, small_policy())
            self.assertEqual(root.root_id, storage.StorageRoot(temp, small_policy()).root_id)
            marker = Path(temp) / "storage-root.json"
            value = storage.read_json(marker)
            value["identity"]["inode"] += 1
            storage.atomic_json(marker, value)
            with self.assertRaisesRegex(storage.StorageError, "identity_mismatch"):
                storage.StorageRoot(temp, small_policy())

    def test_exclusive_lock_rejects_second_owner_then_releases(self):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "lock"
            with storage.FileLock(path):
                with self.assertRaisesRegex(storage.StorageError, "storage_busy"):
                    with storage.FileLock(path):
                        pass
            with storage.FileLock(path):
                pass

    def test_attempts_are_unique_and_environment_cannot_escape(self):
        with tempfile.TemporaryDirectory() as temp:
            root = storage.StorageRoot(temp, small_policy())
            batch = root.batch({"fixture": True})
            a = storage.Attempt(root, batch, "fixture", "task")
            b = storage.Attempt(root, batch, "fixture", "task")
            self.assertNotEqual(a.path, b.path)
            environment = a.environment({"TEMP": "outside", "SWE_API_KEY": "fixture-only", "PATH": os.environ["PATH"]})
            self.assertNotIn("SWE_API_KEY", environment)
            self.assertTrue(Path(environment["TEMP"]).is_relative_to(a.scratch))
            self.assertTrue(Path(environment["GOTMPDIR"]).is_relative_to(a.scratch))
            self.assertTrue(Path(environment["GOMODCACHE"]).is_relative_to(root.path / "cache"))
            self.assertEqual(a.environment({"SWE_API_KEY": "fixture-only"}, credentials=True)["SWE_API_KEY"], "fixture-only")
            self.assertEqual(a.environment({"GOFLAGS": "-p=100"})["GOFLAGS"], "-p=2")
            self.assertNotIn("goflags", a.environment({"goflags": "-p=100"}))
            with a.activate():
                self.assertIs(storage.active(), a)
                with a.phase("judge"):
                    judge_temp = Path(a.environment()["TEMP"])
                    self.assertEqual(judge_temp.parent, a.scratch)
                    self.assertNotEqual(str(judge_temp), environment["TEMP"])
            self.assertIsNone(storage.active())

    def test_budget_violation_is_sticky_and_cannot_be_hidden_by_deleting_file(self):
        with tempfile.TemporaryDirectory() as temp:
            root = storage.StorageRoot(temp, small_policy(max_attempt_bytes=4096))
            attempt = storage.Attempt(root, root.batch({}), "fixture", "task")
            file = attempt.worktree / "data"
            file.write_bytes(b"x" * 8192)
            with self.assertRaisesRegex(storage.StorageError, "storage_budget_exceeded"):
                attempt.check(force=True)
            file.unlink()
            with self.assertRaisesRegex(storage.StorageError, "storage_budget_exceeded"):
                attempt.check(force=True)

    def test_free_space_gate_uses_controlled_measurement(self):
        with tempfile.TemporaryDirectory() as temp:
            root = storage.StorageRoot(temp, small_policy())
            with mock.patch.object(root, "free", return_value={"local_free_bytes": 0, "host_free_bytes": None}):
                with self.assertRaisesRegex(storage.StorageError, "disk_free_below_floor"):
                    root.batch({})
            self.assertEqual(list((root.path / "batches").iterdir()), [])

    def test_output_charge_stops_exactly_at_cap(self):
        with tempfile.TemporaryDirectory() as temp:
            root = storage.StorageRoot(temp, small_policy(max_attempt_log_bytes=10))
            attempt = storage.Attempt(root, root.batch({}), "fixture", "task")
            self.assertEqual(attempt.charge_output(8), 8)
            self.assertEqual(attempt.charge_output(8), 2)
            self.assertEqual(attempt.output_bytes, 10)
            with self.assertRaisesRegex(storage.StorageError, "output_limit_exceeded"):
                attempt.check()

    def test_cache_eviction_keeps_pinned_input_and_records_receipt(self):
        with tempfile.TemporaryDirectory() as temp:
            root = storage.StorageRoot(temp, small_policy(cache_max_bytes=4096))
            old = root.cache_dir("go-build")
            source = root.cache_dir("upstream")
            (old / "build-output").write_bytes(b"x" * 8192)
            (source / "source-input").write_bytes(b"y" * 512)
            with root.lock:
                removed = root.evict_idle_cache(pinned=[source])
            self.assertEqual([item["group"] for item in removed], ["go-build"])
            self.assertFalse(old.exists())
            self.assertTrue((source / "source-input").is_file())
            self.assertTrue(list((root.path / "cleanup").glob("cache-*.json")))

    def test_unknown_cache_directory_is_not_adopted(self):
        with tempfile.TemporaryDirectory() as temp:
            root = storage.StorageRoot(temp, small_policy())
            (root.path / "cache" / "go-build").mkdir()
            with self.assertRaisesRegex(storage.StorageError, "unowned_cache_directory"):
                root.cache_dir("go-build")

    def test_compact_paths_keep_socket_scratch_out_of_deep_attempt_index(self):
        import socket
        with tempfile.TemporaryDirectory() as temp:
            root = storage.StorageRoot(temp, small_policy())
            a = storage.Attempt(root, root.batch({}), "fixture", "task")
            self.assertFalse(a.worktree.is_relative_to(a.path))
            self.assertFalse(a.scratch.is_relative_to(a.path))
            path = Path(a.environment()["TMPDIR"]) / "server.sock"
            if not hasattr(socket, "AF_UNIX") or len(os.fsencode(str(path))) >= 104:
                self.skipTest("test host temporary base is too long for a Unix socket")
            with socket.socket(socket.AF_UNIX) as server:
                server.bind(str(path))
            path.unlink()

    def test_slow_inventory_does_not_block_output_counter(self):
        with tempfile.TemporaryDirectory() as temp:
            root = storage.StorageRoot(temp, small_policy())
            a = storage.Attempt(root, root.batch({}), "fixture", "task")
            entered, release, charged = threading.Event(), threading.Event(), threading.Event()
            from storage_inventory import run_inventory
            def slow(*args, **kwargs):
                entered.set()
                if not release.wait(3):
                    raise RuntimeError("inventory fixture deadline")
                return run_inventory(*args, **kwargs)
            with mock.patch("storage_inventory.run_inventory", side_effect=slow):
                a.check()
                self.assertTrue(entered.wait(2))
                worker = threading.Thread(target=lambda: (a.charge_output(1), charged.set()))
                worker.start()
                try:
                    self.assertTrue(charged.wait(2), "directory scan held the output mutex")
                finally:
                    release.set()
                    worker.join()
                    a.finish_observation()

    def test_initial_inventory_does_not_wait_for_machine_uptime(self):
        with tempfile.TemporaryDirectory() as temp:
            root = storage.StorageRoot(temp, small_policy())
            a = storage.Attempt(root, root.batch({}), "fixture", "task")
            entered = threading.Event()
            with mock.patch.object(a, "_inventory_scan", side_effect=entered.set), \
                 mock.patch.object(storage.time, "monotonic", return_value=0.5):
                a.check()
                self.assertTrue(entered.wait(2))
                a.finish_observation()

    def test_transient_inventory_timeout_and_stale_observation_are_distinct(self):
        with tempfile.TemporaryDirectory() as temp:
            root = storage.StorageRoot(temp, small_policy())
            a = storage.Attempt(root, root.batch({}), "fixture", "task")
            with mock.patch("storage_inventory.run_inventory", side_effect=storage.InventoryTimeout()):
                a._inventory_scan()
            self.assertTrue(a.state["inventory_incomplete"])
            self.assertIsNone(a._violation)
            a._last_complete_inventory = time.monotonic() - a.policy.max_inventory_age_seconds - 1
            with self.assertRaisesRegex(storage.StorageError, "sample expired"):
                a.check()

    def test_cleanup_twice_and_replacement_identity(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            target = root / "owned"
            target.mkdir()
            (target / "value").write_text("fixture")
            identity = storage.file_identity(target)
            self.assertEqual(storage.remove_owned(root, target, identity), "deleted")
            self.assertEqual(storage.remove_owned(root, target, identity), "already_absent")
            target.mkdir()
            wrong = {"device": identity["device"], "inode": -1}
            with self.assertRaisesRegex(storage.StorageError, "identity_changed"):
                storage.remove_owned(root, target, wrong)
            self.assertTrue(target.is_dir())

    @unittest.skipUnless(os.name == "nt", "Windows ReadOnly cleanup contract")
    def test_readonly_files_cleanup_twice_same_path(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            for _ in range(2):
                target = root / "owned"
                target.mkdir()
                file = target / "object"
                file.write_text("fixture")
                file.chmod(0o444)
                storage.remove_owned(root, target, storage.file_identity(target))
                self.assertFalse(target.exists())

    @unittest.skipUnless(os.name == "nt", "Windows non-delete-sharing reader")
    def test_atomic_metadata_waits_for_a_short_lived_reader(self):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "record.json"
            storage.atomic_json(path, {"value": 1})
            stream = path.open("rb")
            def release():
                time.sleep(0.1)
                stream.close()
            thread = threading.Thread(target=release)
            thread.start()
            try:
                storage.atomic_json(path, {"value": 2})
            finally:
                thread.join()
                stream.close()
            self.assertEqual(storage.read_json(path), {"value": 2})


if __name__ == "__main__":
    unittest.main()
