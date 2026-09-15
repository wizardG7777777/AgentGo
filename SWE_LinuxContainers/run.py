"""Run a supplied Linux AgentGo binary in a reusable Flask test image."""
from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import uuid

from binary_input import inspect_binary
from export_logs import IMAGE_CONTRACT, IMAGE_LABEL, atomic_json, verify_export

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent
IMAGE = "agentgo-swe-flask:ubuntu24.04-py3.13-v1"
RUNTIME_LABEL = "io.agentgo.swe-linux.runtime"
RUNTIME_CONTRACT = "external-agentgo-v1"
SUITE_LABEL = "io.agentgo.swe-linux.suite"
VOLUME_PREFIX = "agentgo-swe-linux-data-"
OWNER_LABEL = "io.agentgo.swe-linux.batch"


def configure_console():
    for stream in (sys.stdout, sys.stderr):
        if callable(getattr(stream, "reconfigure", None)):
            stream.reconfigure(encoding="utf-8", errors="backslashreplace")


def require_config(path):
    # Do not read, parse, trim, inspect keys, or contact a provider here.
    if not path.is_file():
        raise FileNotFoundError("缺少 config.json；请自行复制 config.json.example 并填写。")
    return path.resolve()


def capture(argv, cwd=None, env=None):
    return subprocess.check_output(argv, cwd=cwd, env=env, text=True, encoding="utf-8").strip()


def write_json(path, value):
    atomic_json(path, value)


def prepare_build(config, arch, image, root=ROOT, build_root=None):
    require_config(config)  # File existence only; no credentials are read.
    build_root = build_root or HERE / ".build" / uuid.uuid4().hex
    runtime = build_root / "runtime"
    runtime.mkdir(parents=True)
    hashes = {}
    files = [Path("setting.swe-flask.yaml"), Path("scripts/local_fake_provider_smoke.py")]
    files.extend(Path("prompts") / (role + ".md") for role in ("worker", "explorer", "verifier"))
    # Package only source assets. Never read/hash a local config, probe archive,
    # worktree or log that happens to live beside the runner source.
    runner = root / "scripts" / "swe_test_runner"
    files.extend(path.relative_to(root) for path in runner.glob("*.py") if path.is_file())
    files.append(Path("scripts") / "swe_test_runner" / "README.md")
    suite = Path("scripts") / "swe_test_runner" / "suites" / "flask-8"
    files.extend(suite / name for name in ("suite.json", "tasks.csv"))
    for folder in (suite / "prompts", Path("prompts") / "swe"):
        files.extend(path.relative_to(root) for path in (root / folder).glob("*.md") if path.is_file())
    files.extend(Path("SWE_LinuxContainers") / name for name in (
        "Dockerfile", ".dockerignore", "run.py", "entrypoint.py", "binary_input.py", "export_logs.py",
        "prepare_dependencies.py", "test_swe_linux.py", "README.md", "config.json.example"))
    for relative in sorted(files):
        if (root / relative).is_symlink():
            raise RuntimeError("测试镜像资源不能是外部符号链接")
        data = (root / relative).read_bytes()
        target = runtime / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(data)
        hashes[relative.as_posix()] = hashlib.sha256(data).hexdigest()
    identity = {
        "schema": "agentgo.swe-flask-image/v1", "suite": "flask-8", "python": "3.13.5", "pip": "25.1.1",
        "platforms": ["linux/amd64", "linux/arm64"] if arch == "all" else ["linux/" + arch],
        "runtime_contract": RUNTIME_CONTRACT, "asset_files": hashes,
        "image_tag": image, "created_at": dt.datetime.now(dt.timezone.utc).isoformat(),
    }
    write_json(build_root / "image.json", identity)
    shutil.copy2(runtime / "SWE_LinuxContainers" / ".dockerignore", build_root / ".dockerignore")
    return build_root


def build(config, arch, image, *, push=False):
    context = prepare_build(config, arch, image)
    platforms = "linux/amd64,linux/arm64" if arch == "all" else "linux/" + arch
    subprocess.run(["docker", "buildx", "build", "--platform", platforms, "--push" if push else "--load",
                    "--secret", "id=swe_config,src=" + str(config),
                    "-f", str(context / "runtime" / "SWE_LinuxContainers" / "Dockerfile"), "-t", image, str(context)], check=True)
    print("镜像构建完成：" + image)
    return context


def mounts(artifacts, volume, *, config=None, binary=None):
    result = ["--mount", f"type=bind,source={artifacts},target=/exports",
              "--mount", f"type=volume,source={volume},target=/data"]
    if config is not None:
        result += ["--mount", f"type=bind,source={config},target=/run/swe/config.json,readonly"]
    if binary is not None:
        result += ["--mount", f"type=bind,source={binary},target=/run/swe/agentgo,readonly"]
    return result


def image_info(image, arch):
    fields = '{"Id":{{json .Id}},"Os":{{json .Os}},"Architecture":{{json .Architecture}},"Config":{"Labels":{{json .Config.Labels}}}}'
    index_command = ["docker", "image", "inspect", "--format", "{{.Id}}", image]
    result = subprocess.run(index_command, capture_output=True)
    if result.returncode:
        subprocess.run(["docker", "pull", "--platform", "linux/" + arch, image], check=True)
        image_id = capture(index_command)
    else:
        image_id = result.stdout.decode("utf-8").strip()
    # containerd's platform inspect returns a child manifest digest that is not
    # necessarily registered as a runnable image ID. Freeze the parent index,
    # inspect its selected child, and create from that parent plus --platform.
    command = ["docker", "image", "inspect", "--platform", "linux/" + arch, "--format", fields, image_id]
    info = json.loads(capture(command))
    labels = info.get("Config", {}).get("Labels") or {}
    if (info.get("Os"), info.get("Architecture")) != ("linux", arch):
        raise RuntimeError("测试镜像 CPU 架构不匹配")
    if (labels.get(IMAGE_LABEL), labels.get(RUNTIME_LABEL), labels.get(SUITE_LABEL)) != (
            IMAGE_CONTRACT, RUNTIME_CONTRACT, "flask-8"):
        raise RuntimeError("需要支持外部 AgentGo 二进制的 Flask 测试镜像；请拉取新版本或执行 build-image。")
    info["PlatformId"], info["Id"] = info["Id"], image_id
    return info


def finish_resources(container_id, volume, batch, destination, *, persist=False):
    """Only a verified export can authorize deletion of this batch's resources."""
    receipt = {"batch_id": batch, "container_id": container_id, "volume": volume,
               "persist": persist, "container_removed": False, "volume_removed": False}
    try:
        verify_export(destination, batch)
        if persist:
            receipt.update(complete=True, retained=True, reason="persist_flag")
            write_json(destination / "cleanup.json", receipt)
            return True
        if volume != VOLUME_PREFIX + batch.lower():
            raise RuntimeError("拒绝清理非本批独立数据卷")
        labels = json.loads(capture(["docker", "inspect", "--format", "{{json .Config.Labels}}", container_id])) or {}
        volume_labels = json.loads(capture(["docker", "volume", "inspect", "--format", "{{json .Labels}}", volume])) or {}
        if labels.get(OWNER_LABEL) != batch or volume_labels.get(OWNER_LABEL) != batch:
            raise RuntimeError("容器或数据卷的批次归属不匹配，拒绝清理")
        if capture(["docker", "inspect", "--format", "{{.State.Running}}", container_id]) != "false":
            raise RuntimeError("测试容器仍在运行，拒绝清理")
        # No --force, no prune, no shared historical volume deletion.
        subprocess.run(["docker", "rm", container_id], check=True, stdout=subprocess.DEVNULL)
        receipt["container_removed"] = True
        subprocess.run(["docker", "volume", "rm", volume], check=True, stdout=subprocess.DEVNULL)
        receipt.update(volume_removed=True, complete=True, retained=False)
        write_json(destination / "cleanup.json", receipt)
        return True
    except (OSError, ValueError, KeyError, RuntimeError, subprocess.SubprocessError) as error:
        receipt.update(complete=False, retained=True, error_type=type(error).__name__)
        write_json(destination / "cleanup.json", receipt)
        print("SWE_LinuxContainers: 导出校验或清理未完成，未删除的本批资源保留；详见 cleanup.json。", file=sys.stderr)
        return False


def discard_unstarted_volume(volume, batch, destination):
    """A failed create may have allocated only a volume; never prune globally."""
    receipt = {"batch_id": batch, "volume": volume, "phase": "container_create",
               "complete": False, "retained": True, "volume_removed": False}
    try:
        containers = capture(["docker", "ps", "-aq", "--filter", f"label={OWNER_LABEL}={batch}"])
        labels = json.loads(capture(["docker", "volume", "inspect", "--format", "{{json .Labels}}", volume])) or {}
        if containers or volume != VOLUME_PREFIX + batch.lower() or labels.get(OWNER_LABEL) != batch:
            raise RuntimeError("无法确认这是未启动批次的独立空卷")
        subprocess.run(["docker", "volume", "rm", volume], check=True, stdout=subprocess.DEVNULL)
        receipt.update(complete=True, retained=False, volume_removed=True)
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
        receipt["error_type"] = type(error).__name__
    write_json(destination / "cleanup.json", receipt)


def launch(config, artifacts, image, *, binary=None, arch=None, persist=False):
    require_config(config)
    binary_identity = inspect_binary(binary, arch)
    arch = binary_identity["arch"]
    selected_image = image_info(image, arch)
    image_id = selected_image["Id"]
    batch = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ-") + uuid.uuid4().hex[:8]
    name = "swe-linux-" + batch.lower()
    volume = VOLUME_PREFIX + batch.lower()
    artifacts.mkdir(parents=True, exist_ok=True)
    destination = artifacts / batch
    destination.mkdir()
    write_json(destination / "host.json", {
        "batch_id": batch, "container_name": name, "volume": volume, "persist": persist,
        "image_id": image_id, "platform": "linux/" + arch, "agentgo_binary": binary_identity,
        "platform_image_id": selected_image["PlatformId"],
        "docker_kernel": capture(["docker", "info", "--format", "{{.KernelVersion}}"]),
        "docker_architecture": capture(["docker", "info", "--format", "{{.Architecture}}"]),
    })
    subprocess.run(["docker", "volume", "create", "--label", f"{OWNER_LABEL}={batch}", volume],
                   check=True, stdout=subprocess.DEVNULL)
    try:
        container_id = capture(["docker", "create", "--init", "--platform", "linux/" + arch, "--name", name,
                                "--label", f"{OWNER_LABEL}={batch}", *mounts(artifacts, volume, config=config, binary=binary.resolve()),
                                image_id, "--batch-id", batch, "--binary-sha256", binary_identity["sha256"]])
    except (OSError, subprocess.SubprocessError):
        discard_unstarted_volume(volume, batch, destination)
        raise
    subprocess.run(["docker", "start", container_id], check=True, stdout=subprocess.DEVNULL)
    print(f"容器：{name}\n报告与日志：{destination}\n持久化：{persist}", flush=True)
    follower = subprocess.Popen(["docker", "logs", "--follow", container_id])
    try:
        try:
            code = int(capture(["docker", "wait", container_id]))
        except KeyboardInterrupt:
            subprocess.run(["docker", "stop", "--time", "60", container_id], check=True)
            code = int(capture(["docker", "wait", container_id]))
    finally:
        try:
            follower.wait(timeout=10)
        except subprocess.TimeoutExpired:
            follower.terminate()
            follower.wait(timeout=10)
        state = json.loads(capture(["docker", "inspect", "--format", "{{json .State}}", container_id]))
        write_json(destination / "container-state.json", state)
    try:
        verify_export(destination, batch)
    except (OSError, ValueError, KeyError, RuntimeError):
        print("报告/日志尚未完整导出；从本批数据卷恢复导出。", flush=True)
        subprocess.run(["docker", "run", "--rm", "--init", "--platform", "linux/" + arch,
                        "--label", f"{OWNER_LABEL}={batch}", *mounts(artifacts, volume), image_id,
                        "--batch-id", batch, "--config", f"/data/{batch}/config.input.json", "--export-only"])
    finished = finish_resources(container_id, volume, batch, destination, persist=persist)
    print(f"测试退出码：{code}；报告与日志：{destination}")
    return code if finished else 1


def main():
    configure_console()
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("build-image", "run"), nargs="?", default="run")
    parser.add_argument("--config", type=Path, default=HERE / "config.json")
    parser.add_argument("--arch", choices=("amd64", "arm64", "all"))
    parser.add_argument("--agentgo-binary", type=Path, help="必需的待测 AgentGo Linux amd64/arm64 ELF 文件")
    parser.add_argument("--image", default=IMAGE)
    parser.add_argument("--artifacts", type=Path, default=HERE / "artifacts")
    parser.add_argument("--push", action="store_true", help="build-image 将多架构镜像发布到 --image 指定的仓库")
    parser.add_argument("--persist", action="store_true", help="测试后保留停止状态的容器及本批数据卷")
    args = parser.parse_args()
    try:
        config = require_config(args.config)
        if args.command == "build-image":
            arch = args.arch or "all"
            build(config, arch, args.image, push=args.push)
            return 0
        inspect_binary(args.agentgo_binary)
        if args.arch == "all":
            raise ValueError("一次测试容器只使用一个 CPU 架构；all 仅用于 build-image。")
        arch = args.arch or {"x86_64": "amd64", "aarch64": "arm64", "amd64": "amd64", "arm64": "arm64"}[
            capture(["docker", "info", "--format", "{{.Architecture}}"])]
        return launch(config, args.artifacts.resolve(), args.image, binary=args.agentgo_binary, arch=arch, persist=args.persist)
    except (OSError, RuntimeError, ValueError, subprocess.SubprocessError) as error:
        print(f"SWE_LinuxContainers: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
