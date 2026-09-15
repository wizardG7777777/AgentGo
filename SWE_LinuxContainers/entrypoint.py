"""Ubuntu test entry; no Go build, dependency build, or config preflight."""
from __future__ import annotations

import argparse
import datetime as dt
import json
import os
from pathlib import Path
import platform
import shutil
import signal
import subprocess
import sys
import uuid

from export_logs import atomic_json, export_batch
from binary_input import inspect_binary

APP = Path("/opt/agentgo")


def load_environment(raw, batch, app=APP):
    # Interpretation occurs only AFTER the container starts and the raw input
    # has been archived. No schema/credential/model/URL validation or fallback.
    config = json.loads(raw)
    env = os.environ.copy()
    # A missing setting never borrows credentials from Docker/host state.
    for name in list(env):
        if name.startswith(("SWE_", "AGENTGO_")):
            del env[name]
    env.update(config.get("environment", {}))
    env.update({
        "SWE_AGENTGO_ROOT": str(app), "SWE_AGENTGO_BIN": str(batch / "bin" / "agentgo"),
        "SWE_SUITE_DIR": str(app / "scripts/swe_test_runner/suites/flask-8"),
        "SWE_TASKS_FILE": str(app / "scripts/swe_test_runner/suites/flask-8/tasks.csv"),
        "SWE_PROMPT_DIR": str(app / "scripts/swe_test_runner/suites/flask-8/prompts"),
        "SWE_TESTBED": str(batch / "testbed"), "SWE_FLASK_REPO": "/opt/swe/upstream/flask",
        "SWE_PREBUILT_ENVS": "/opt/swe/environments",
        "SWE_PROBE_ARCHIVE": str(batch / "provider-probes"),
        "AGENTGO_DUMP_PROMPTS": "1", "AGENTGO_TRACE_FULL_ARGS": "1",
        "AGENTGO_TRACE_KEEP_ALL": "1", "PYTHONUNBUFFERED": "1", "PYTHONUTF8": "1",
        "PIP_DISABLE_PIP_VERSION_CHECK": "1",
    })
    return env, [sys.executable, "-u", str(app / "scripts" / "swe_test_runner" / "runner.py"),
                 *config.get("runner_args", ["batch", "--timeout", "1200"])]


def execute(argv, environment, logfile):
    process = subprocess.Popen(argv, env=environment, stdout=subprocess.PIPE,
                               stderr=subprocess.STDOUT, start_new_session=True)
    interrupted = False

    def stop(signum, frame):
        nonlocal interrupted
        interrupted = True
        if process.poll() is None:
            # Let the Python runner unwind its own AgentGo process groups.
            try:
                os.killpg(process.pid, signal.SIGINT)
            except ProcessLookupError:
                pass

    previous = {sig: signal.signal(sig, stop) for sig in (signal.SIGTERM, signal.SIGINT)}
    try:
        with process.stdout, logfile.open("wb") as output:
            while data := process.stdout.read1(65536):
                output.write(data)
                output.flush()
                sys.stdout.buffer.write(data)
                sys.stdout.buffer.flush()
        code = process.wait()
        return {"exit_code": code, "interrupted": interrupted}
    finally:
        for sig, handler in previous.items():
            signal.signal(sig, handler)
        if process.poll() is None:
            stop(signal.SIGTERM, None)
            process.wait()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--batch-id", default=dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ-") + uuid.uuid4().hex[:8])
    parser.add_argument("--config", type=Path, default=Path("/run/swe/config.json"))
    parser.add_argument("--data", type=Path, default=Path("/data"))
    parser.add_argument("--exports", type=Path, default=Path("/exports"))
    parser.add_argument("--export-only", action="store_true")
    parser.add_argument("--agentgo-binary", type=Path, default=Path("/run/swe/agentgo"))
    parser.add_argument("--binary-sha256", help="宿主冻结的待测二进制摘要")
    args = parser.parse_args()
    # Batch identity is a filesystem boundary, not config content validation.
    if not args.batch_id or any(c not in "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_" for c in args.batch_id):
        raise ValueError("invalid batch identity")
    if not args.export_only:
        try:
            arch = {"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[platform.machine().lower()]
            binary_identity = inspect_binary(args.agentgo_binary, arch)
            if args.binary_sha256 and binary_identity["sha256"] != args.binary_sha256:
                raise ValueError("AgentGo 二进制在容器创建后发生变化，拒绝使用不同版本。")
        except (OSError, ValueError, KeyError) as error:
            print(f"SWE_LinuxContainers binary input: {error}", file=sys.stderr)
            return 1
    if not args.config.is_file():
        print("SWE_LinuxContainers: 缺少 config.json", file=sys.stderr)
        return 1
    batch, destination = args.data / args.batch_id, args.exports / args.batch_id
    if args.export_only:
        export_batch(batch, destination, recovered=True)
        return 0
    batch.mkdir(parents=True, exist_ok=False)
    destination.mkdir(parents=True, exist_ok=True)
    raw = args.config.read_bytes()
    (batch / "config.input.json").write_bytes(raw)
    code = 1
    stage = "binary_copy"
    try:
        binary = batch / "bin" / "agentgo"
        binary.parent.mkdir()
        shutil.copyfile(args.agentgo_binary, binary)
        binary.chmod(0o755)
        if inspect_binary(binary, arch)["sha256"] != binary_identity["sha256"]:
            raise ValueError("AgentGo 二进制复制后摘要不一致")
        atomic_json(batch / "agentgo-binary.json", binary_identity)
        if (APP / "image.json").is_file():
            shutil.copy2(APP / "image.json", batch / "image.json")
        stage = "config_load"
        environment, argv = load_environment(raw, batch)
        atomic_json(batch / "environment.full.json", environment)
        atomic_json(batch / "run.json", {
            "schema": "agentgo.swe-linux-run/v1", "batch_id": args.batch_id, "argv": argv,
            "platform": platform.platform(), "uname": list(platform.uname()), "python": sys.version,
            "started_at": dt.datetime.now(dt.timezone.utc).isoformat(),
        })
        shutil.copytree(Path("/opt/swe/environments"), batch / "dependency-identities",
                        ignore=lambda path, names: [name for name in names if name in {"venv", ".venv"}])
        stage = "binary_startup"
        startup_log = batch / "testbed" / "runs" / "binary-startup.log"
        startup_log.parent.mkdir(parents=True, exist_ok=True)
        with startup_log.open("wb") as log:
            startup = subprocess.run([str(binary), "-h"], env=environment, stdout=log,
                                     stderr=subprocess.STDOUT, timeout=15)
        if startup.returncode != 0:
            raise RuntimeError("AgentGo 二进制无法在当前 Ubuntu 容器启动；查看 binary-startup.log")
        stage = "swe_runner"
        result = execute(argv, environment, destination / "batch.log")
        atomic_json(batch / "process-exit.json", result)
        shutil.copy2(destination / "batch.log", batch / "batch.log")
        code = result["exit_code"]
    except Exception as error:
        # Do not stringify arbitrary config/type errors: they may contain keys.
        record = {"stage": stage, "error_type": type(error).__name__, "exit_code": 1}
        atomic_json(batch / "runtime-error.json", record)
        atomic_json(batch / "process-exit.json", {"exit_code": 1, "stage": stage})
        print(f"SWE_LinuxContainers runtime error: stage={stage} type={type(error).__name__}", file=sys.stderr)
    finally:
        try:
            export_batch(batch, destination)
        except Exception as error:
            atomic_json(destination / "archive-status.json", {"complete": False, "error_type": type(error).__name__})
            print("SWE_LinuxContainers: 归档未完成，Linux 持久卷保留现场。", file=sys.stderr)
            code = 1
    return code


if __name__ == "__main__":
    raise SystemExit(main())
