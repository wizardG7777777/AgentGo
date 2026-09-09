"""测试身份算法的离线回归；不启动 AgentGo、pytest 或外部 provider。"""

from pathlib import Path
import tempfile
import unittest

from test_identity import input_identity, validate_import_origin, compare_failures


class TestInputIdentity(unittest.TestCase):
    def test_uncommitted_create_change_delete_and_configuration(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "src" / "flask" / "__init__.py"
            source.parent.mkdir(parents=True)
            source.write_bytes(b"value = 1\r\n")
            initial = input_identity(root)
            source.write_bytes(b"value = 2\r\n")
            self.assertNotEqual(initial["digest"], input_identity(root)["digest"])
            source.write_bytes(b"value = 1\r\n")
            self.assertEqual(initial, input_identity(root))
            created = source.with_name("new.py")
            created.write_text("", encoding="utf-8")
            self.assertNotEqual(initial["digest"], input_identity(root)["digest"])
            created.unlink()
            self.assertEqual(initial, input_identity(root))
            (root / "pytest.ini").write_text("[pytest]\n", encoding="utf-8")
            self.assertNotEqual(initial["digest"], input_identity(root)["digest"])

    def test_import_origin_cannot_escape_to_other_checkout(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "src" / "flask" / "__init__.py"
            source.parent.mkdir(parents=True)
            source.touch()
            outside = root / "other.py"
            outside.touch()
            python = root / "python"
            environment = {"python": str(python), "flask_file": str(source), "packages": [["pytest", "test"]]}
            validate_import_origin(environment, root, python)
            environment["flask_file"] = str(outside)
            with self.assertRaisesRegex(ValueError, "穿透"):
                validate_import_origin(environment, root, python)

    def test_equal_counts_with_different_failures_are_not_same_failure(self):
        baseline = {"collected_nodeids": ["a", "b"], "failure_events": [{"nodeid": "a", "phase": "call"}]}
        current = {"collected_nodeids": ["a", "b"], "failure_events": [{"nodeid": "b", "phase": "teardown"}]}
        comparison = compare_failures(baseline, current)
        self.assertEqual(comparison["added_failures"], [{"nodeid": "b", "phase": "teardown"}])
        self.assertEqual(comparison["removed_failures"], [{"nodeid": "a", "phase": "call"}])
        self.assertTrue(comparison["same_collection"])
        current["collected_nodeids"] = ["b"]
        self.assertFalse(compare_failures(baseline, current)["same_collection"])


if __name__ == "__main__":
    unittest.main()
