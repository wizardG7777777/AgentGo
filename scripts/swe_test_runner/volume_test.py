from pathlib import Path
import tempfile
import unittest

import storage
from volume import VolumeLease


class VolumeTest(unittest.TestCase):
    def test_different_roots_share_volume_exclusion_and_release(self):
        with tempfile.TemporaryDirectory() as temp:
            first, second = Path(temp) / "first", Path(temp) / "second"
            first.mkdir()
            second.mkdir()
            with VolumeLease(first) as lease:
                lease.ensure_alive()
                with self.assertRaisesRegex(storage.StorageError, "volume_lease_unavailable|storage_busy"):
                    with VolumeLease(second):
                        pass
                if lease.process:
                    self.assertGreater(lease.free_bytes(), 0)
            with VolumeLease(second) as lease:
                lease.ensure_alive()


if __name__ == "__main__":
    unittest.main()
