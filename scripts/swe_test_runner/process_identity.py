"""Stable process identities for offline resource reconciliation."""
import os
from pathlib import Path


def capture(process):
    value = {"pid": process.pid, "birth": process.create_time()}
    stat_path = Path("/proc") / str(process.pid) / "stat"
    if os.name == "posix" and Path("/proc/sys/kernel/random/boot_id").exists():
        # The comm field may contain spaces and ')'; fields after its final ')'
        # begin at field 3. Kernel start ticks survive wall-clock adjustments.
        rest = stat_path.read_text().rpartition(")")[2].split()
        value.update(start_ticks=rest[19], boot_id=Path("/proc/sys/kernel/random/boot_id").read_text().strip())
    return value


def alive(value, psutil):
    try:
        process = psutil.Process(value["pid"])
        actual = capture(process)
        keys = ("pid", "start_ticks", "boot_id") if "start_ticks" in value else ("pid", "birth")
        return all(actual.get(k) == value.get(k) for k in keys) and process.is_running() and process.status() != psutil.STATUS_ZOMBIE
    except (psutil.NoSuchProcess, psutil.ZombieProcess, FileNotFoundError, ProcessLookupError):
        return False
