"""POSIX observed-tree guardian; parent EOF cancels recorded live descendants."""
from __future__ import annotations
import json
import os
from pathlib import Path
import queue
import signal
import sys
import threading
import time

import psutil

from storage import atomic_json
import process_identity


def main():
    identity, receipt_path = json.loads(sys.argv[1]), Path(sys.argv[2])
    pid = identity["pid"]
    original = psutil.Process(pid)
    if not process_identity.alive(identity, psutil):
        return 125
    known, inbox = {pid: original}, queue.Queue()
    unknown = False

    def controls():
        inbox.put(sys.stdin.buffer.readline().strip())

    threading.Thread(target=controls, daemon=True).start()
    print("ready", flush=True)
    parent_lost = False
    while True:
        for process in list(known.values()):
            try:
                for child in process.children(recursive=True):
                    known.setdefault(child.pid, child)
            except (psutil.NoSuchProcess, psutil.ZombieProcess):
                pass
            except psutil.AccessDenied:
                unknown = True
        try:
            control = inbox.get(timeout=0.05)
            parent_lost = control != b"stop"
            break
        except queue.Empty:
            continue
    survivors = []
    for process in list(known.values()):
        try:
            if process.is_running() and process.status() != psutil.STATUS_ZOMBIE:
                process.terminate()
                survivors.append(process)
        except (psutil.NoSuchProcess, psutil.ZombieProcess):
            pass
        except psutil.AccessDenied:
            unknown = True
    _, alive = psutil.wait_procs(survivors, timeout=1)
    for process in alive:
        try:
            process.kill()
        except psutil.NoSuchProcess:
            pass
        except psutil.AccessDenied:
            unknown = True
    _, alive = psutil.wait_procs(alive, timeout=2)
    # A sibling guardian cannot reap the supervisor's child. A zombie has
    # already released its files and is not a surviving writer.
    writers = []
    for process in alive:
        try:
            if process.is_running() and process.status() != psutil.STATUS_ZOMBIE:
                writers.append(process)
        except (psutil.NoSuchProcess, psutil.ZombieProcess):
            pass
        except psutil.AccessDenied:
            unknown = True
    alive = writers
    receipt = {"schema": "agentgo.swe-process-guardian/v1", "coverage": "observed_tree",
                "parent_lost": parent_lost, "tracked_processes": len(known), "quiescent": not alive and not unknown,
                "remaining_pids": [p.pid for p in alive], "unknown": unknown, "finished_at": time.time()}
    if len(sys.argv) > 3 and receipt['quiescent']:
        try:
            import scratch_scope
            scratch_scope.recover(sys.argv[3], receipt)
            receipt['scratch_recovered'] = True
        except Exception as error:
            receipt['scratch_error'] = getattr(error, 'code', type(error).__name__)
    atomic_json(receipt_path, receipt)
    return 0 if receipt['quiescent'] and not receipt.get('scratch_error') else 125


if __name__ == "__main__":
    raise SystemExit(main())
