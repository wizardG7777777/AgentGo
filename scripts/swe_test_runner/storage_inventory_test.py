import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
from dataclasses import replace
from unittest import mock

import artifacts
import cleanup
import process_identity
import processes
import storage
import storage_inventory
from storage_test import small_policy


class InventoryProcessTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = storage.StorageRoot(self.temporary.name, small_policy())
        self.attempt = storage.Attempt(self.root, self.root.batch({}), "fixture", "inventory")

    def test_fast_sample_keeps_unknown_unique_bytes_and_final_sample_is_exact(self):
        cache = self.root.cache_dir('go-build')
        (cache / 'a').write_bytes(b'a' * 20)
        os.link(cache / 'a', cache / 'b')
        self.attempt._inventory_scan()
        self.assertEqual(self.attempt.state['cache_usage']['logical_bytes'], 40)
        self.assertIsNone(self.attempt.state['cache_usage']['unique_file_bytes'])
        self.assertNotIn('cache_exact', self.attempt.state['inventory_sections'])
        self.attempt._inventory_scan(final=True)
        self.assertEqual(self.attempt.state['cache_usage']['unique_file_bytes'], 20)
        self.assertEqual(self.attempt.state['inventory_accounting'], 'exact')
        import json
        samples = [json.loads(line) for line in (self.attempt.reports / 'inventory-samples.jsonl').read_text().splitlines()]
        self.assertEqual([x['final'] for x in samples], [False, True])

    def test_cache_above_logical_cap_is_refined_before_budget_decision(self):
        self.attempt.policy = replace(self.attempt.policy, cache_max_bytes=100)
        self.attempt.state['policy'] = self.attempt.policy.record()
        self.attempt.persist()
        cache = self.root.cache_dir('go-build')
        (cache / 'a').write_bytes(b'a' * 80)
        os.link(cache / 'a', cache / 'b')
        self.attempt._inventory_scan()
        self.assertEqual(self.attempt.state['cache_usage']['logical_bytes'], 160)
        self.assertEqual(self.attempt.state['cache_usage']['unique_file_bytes'], 80)
        self.assertIn('cache_exact', self.attempt.state['inventory_sections'])
        self.assertIsNone(self.attempt._violation)
        (cache / 'independent').write_bytes(b'b' * 60)
        self.attempt._inventory_scan()
        with self.assertRaisesRegex(storage.StorageError, 'cache_budget_exceeded'):
            self.attempt.check()

    def test_low_free_space_still_stops_while_inventory_is_busy(self):
        worker = mock.Mock()
        worker.is_alive.return_value = True
        self.attempt._inventory_worker = worker
        self.attempt._last_inventory = time.monotonic()
        try:
            with mock.patch.object(self.root, 'free', return_value={'local_free_bytes': 0, 'host_free_bytes': None}):
                with self.assertRaisesRegex(storage.StorageError, 'disk_free_below_floor'):
                    self.attempt.check()
        finally:
            self.attempt._inventory_worker = None

    def test_isolated_worker_counts_files_and_leaves_no_live_process(self):
        (self.attempt.worktree / "data").write_bytes(b"x" * 8192)
        cache = self.root.cache_dir("go-build")
        (cache / "a").write_bytes(b"x" * 4096)
        os.link(cache / "a", cache / "b")
        result = storage_inventory.run_inventory(self.attempt)
        self.assertTrue(result["ok"], result)
        self.assertGreaterEqual(result["current"]["logical_bytes"], 8192)
        self.assertEqual(result["cache"]["logical_bytes"], 8192)
        self.assertEqual(result["cache"]["unique_file_bytes"], 4096)
        scanner = self.attempt.state["inventory_scanner"]
        self.assertEqual(result["pid"], scanner["identity"]["pid"])
        self.assertTrue(scanner["quiescent"])
        self.assertFalse(process_identity.alive(scanner["identity"], processes.process_library()))

    def test_permission_failure_keeps_its_section_and_relative_path(self):
        blocked = self.attempt.scratch / 'fixture' / 'inaccessible'
        original = storage.measure_tree
        def measure(path, **kwargs):
            if Path(path) == self.attempt.scratch:
                raise PermissionError(13, 'Permission denied', str(blocked))
            return original(path, **kwargs)
        with mock.patch.object(storage, 'measure_tree', side_effect=measure):
            result = storage_inventory.collect(self.root.path, self.attempt.path, 5)
        self.assertFalse(result['ok'])
        self.assertEqual(result['error_type'], 'PermissionError')
        self.assertEqual(result['blocked_section'], 'scratch')
        self.assertEqual(result['error_path'], blocked.relative_to(self.root.path).as_posix())
        with mock.patch.object(storage_inventory, 'run_inventory', return_value=result):
            self.attempt._inventory_scan()
        self.assertEqual(self.attempt.state['inventory_error_type'], 'PermissionError')
        self.assertEqual(self.attempt.state['inventory_error_path'], result['error_path'])
        self.assertEqual(self.attempt.state['inventory_blocked_section'], 'scratch')
        self.assertTrue(self.attempt.state['inventory_incomplete'])
        if os.name == 'nt':
            # The test-owned mode-changing window is POSIX-specific. A lasting
            # Windows refusal must not be granted a 600-second grace period.
            with self.assertRaisesRegex(storage.StorageError, 'storage_observation_unavailable'):
                self.attempt.check()
            return
        self.attempt.check()  # A recent complete sample bounds a transient test permission change.
        self.attempt._last_complete_inventory = time.monotonic() - self.attempt.policy.max_inventory_age_seconds - 1
        with self.assertRaisesRegex(storage.StorageError, 'storage_observation_unavailable'):
            self.attempt.check()

    def test_active_readonly_scanner_blocks_archive_and_cleanup(self):
        (self.attempt.worktree / "data").write_bytes(b"evidence")
        manifest = artifacts.seal(self.attempt)
        plan = cleanup.plan(self.attempt, manifest)
        process = subprocess.Popen([getattr(sys, "_base_executable", sys.executable), "-c", "import time;time.sleep(30)"],
            **({"creationflags": subprocess.CREATE_NO_WINDOW} if os.name == "nt" else {}))
        try:
            self.attempt.state["inventory_scanner"] = {
                "identity": process_identity.capture(processes.process_library().Process(process.pid)),
                "role": "read_only_inventory", "quiescent": False}
            self.attempt.persist()
            with self.assertRaisesRegex(storage.StorageError, "inventory_process_still_alive"):
                artifacts.seal(self.attempt)
            with self.assertRaisesRegex(storage.StorageError, "inventory_process_still_alive"):
                cleanup.apply(plan)
            self.assertTrue(self.attempt.worktree.exists())
        finally:
            process.kill()
            process.wait()
        # Supervisor loss may leave a stale false receipt; stable PID identity
        # proves the bounded read-only worker has exited without resuming work.
        storage_inventory.ensure_stopped(self.attempt.state)
        self.assertTrue(cleanup.apply(plan)["complete"])
        self.assertFalse(self.attempt.worktree.exists())

    def test_unresponsive_worker_is_killed_and_recorded_before_cleanup(self):
        self.attempt.policy = replace(self.attempt.policy, max_scan_seconds=1)
        original = subprocess.Popen
        def sleeping_worker(argv, **kwargs):
            return original([getattr(sys, "_base_executable", sys.executable), "-c", "import time;time.sleep(30)"], **kwargs)
        with mock.patch.object(storage_inventory.subprocess, "Popen", side_effect=sleeping_worker):
            result = storage_inventory.run_inventory(self.attempt)
        self.assertFalse(result["ok"])
        self.assertEqual(result["error_type"], "InventoryTimeout")
        self.assertEqual(result["blocked_section"], "worker_deadline")
        scanner = self.attempt.state["inventory_scanner"]
        self.assertTrue(scanner["quiescent"])
        self.assertFalse(process_identity.alive(scanner["identity"], processes.process_library()))


if __name__ == "__main__":
    unittest.main()
