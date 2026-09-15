"""Identify an explicitly supplied Linux executable without running it on the host."""
from __future__ import annotations

import hashlib
from pathlib import Path
import struct

ARCHITECTURES = {62: "amd64", 183: "arm64"}
MISSING_BINARY = "必须指定能够在 Docker 内运行的 AgentGo 二进制：使用 --agentgo-binary 提供 Linux amd64 或 arm64 ELF 文件。"


def inspect_binary(path: Path | None, expected_arch: str | None = None) -> dict:
    if path is None or not path.is_file():
        raise ValueError(MISSING_BINARY)
    before = path.stat()
    with path.open("rb") as stream:
        header = stream.read(64)
        if len(header) != 64 or header[:4] != b"\x7fELF":
            raise ValueError("需要 AgentGo Linux ELF 二进制；Windows .exe 和 macOS Mach-O 不能用于此容器。")
        if header[4:7] != b"\x02\x01\x01" or header[7] not in {0, 3}:
            raise ValueError("需要 64 位 little-endian Linux ELF 二进制。")
        kind, machine = struct.unpack_from("<HH", header, 16)
        arch = ARCHITECTURES.get(machine)
        if kind not in {2, 3} or arch is None:
            raise ValueError("仅支持 Linux amd64 或 arm64 可执行 ELF。")
        if expected_arch is not None and arch != expected_arch:
            raise ValueError(f"AgentGo 二进制是 linux/{arch}，目标容器是 linux/{expected_arch}；请提供匹配的 Linux 二进制。")
        phoff = struct.unpack_from("<Q", header, 32)[0]
        phsize, phcount = struct.unpack_from("<HH", header, 54)
        if phsize != 56 or not phcount or phoff < 64 or phoff + phsize * phcount > before.st_size:
            raise ValueError("AgentGo ELF 程序头不完整，无法在容器内运行。")
        loadable = False
        interpreter = None
        for index in range(phcount):
            stream.seek(phoff + index * phsize)
            ph = stream.read(phsize)
            segment = struct.unpack_from("<I", ph)[0]
            offset, length = struct.unpack_from("<Q", ph, 8)[0], struct.unpack_from("<Q", ph, 32)[0]
            if offset + length > before.st_size:
                raise ValueError("AgentGo ELF 段超出文件范围。")
            loadable |= segment == 1
            if segment == 3:
                if length > 4096:
                    raise ValueError("AgentGo ELF 动态加载器字段无效。")
                stream.seek(offset)
                raw = stream.read(length)
                if not raw.endswith(b"\0") or not raw.startswith(b"/"):
                    raise ValueError("AgentGo ELF 动态加载器路径无效。")
                interpreter = raw[:-1].decode("utf-8")
        if not loadable:
            raise ValueError("AgentGo ELF 没有可加载的程序段。")
        stream.seek(0)
        digest = hashlib.file_digest(stream, "sha256").hexdigest()
    after = path.stat()
    if (before.st_size, before.st_mtime_ns) != (after.st_size, after.st_mtime_ns):
        raise ValueError("读取期间 AgentGo 二进制发生变化，请在构建完成后重新启动。")
    return {"format": "ELF64", "platform": "linux/" + arch, "arch": arch,
            "sha256": digest, "bytes": before.st_size, "interpreter": interpreter}
