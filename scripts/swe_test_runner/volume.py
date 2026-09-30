"""Cross-root volume leases; Windows and WSL use the same native host lock."""
from __future__ import annotations
import base64
import json
import os
from pathlib import Path
import subprocess
import threading

from storage import FileLock, StorageError

HOST_SCRIPT = r"""
$ErrorActionPreference='Stop'
[Console]::InputEncoding=[Text.UTF8Encoding]::new($false)
[Console]::OutputEncoding=[Text.UTF8Encoding]::new($false)
$lease=$null
try {
  $request=[Console]::ReadLine() | ConvertFrom-Json
  $base=[string]$request.root
  if($request.mode -eq 'wsl') {
    if($request.drive) { $base=([string]$request.drive)+':\' }
    else {
      $d=@(Get-ChildItem 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Lxss' | ForEach-Object {Get-ItemProperty $_.PSPath} | Where-Object {$_.DistributionName -eq $request.distro})
      if($d.Count -ne 1) {throw 'distribution lookup failed'}
      $base=[string]$d[0].BasePath
    }
  }
  if($base.StartsWith('\\?\')) {$base=$base.Substring(4)}
  $driveRoot=[IO.Path]::GetPathRoot($base)
  if($driveRoot -notmatch '^[A-Za-z]:\\$') {throw 'unsupported volume'}
  $directory=[IO.Path]::Combine([Environment]::GetFolderPath('LocalApplicationData'),'AgentGo','swe-volume-leases')
  [IO.Directory]::CreateDirectory($directory) | Out-Null
  $path=[IO.Path]::Combine($directory,($driveRoot.Substring(0,1).ToUpperInvariant()+'.lock'))
  $lease=[IO.File]::Open($path,[IO.FileMode]::OpenOrCreate,[IO.FileAccess]::ReadWrite,[IO.FileShare]::None)
  [Console]::WriteLine((@{ready=$true;volume=$driveRoot;coordinator=$path} | ConvertTo-Json -Compress))
  while($null -ne ($line=[Console]::ReadLine())) {
    if($line -eq 'release') {break}
    if($line -ne 'space') {throw 'unknown operation'}
    $volume=[IO.DriveInfo]::new($driveRoot)
    [Console]::WriteLine((@{free_bytes=$volume.AvailableFreeSpace} | ConvertTo-Json -Compress))
  }
} catch {
  [Console]::WriteLine((@{error='volume_lease_unavailable'} | ConvertTo-Json -Compress))
  exit 3
} finally {
  if($null -ne $lease) {$lease.Dispose()}
}
"""


def in_wsl():
    path = Path("/proc/sys/kernel/osrelease")
    return os.name != "nt" and path.is_file() and "microsoft" in path.read_text().lower()


class VolumeLease:
    def __init__(self, root, *, host_required=True):
        self.root = Path(root)
        self.wsl = in_wsl() and host_required
        self.process = self.file_lock = None
        self.mutex = threading.Lock()
        self.metadata = {}

    def _line(self):
        result = []
        thread = threading.Thread(target=lambda: result.append(self.process.stdout.readline()), daemon=True)
        thread.start()
        thread.join(10)
        if thread.is_alive():
            self.process.kill()
            self.process.wait()
            thread.join(2)
            raise StorageError("storage_observation_unavailable", "host lease response timeout")
        try:
            value = json.loads(result[0])
        except (IndexError, ValueError):
            raise StorageError("volume_lease_unavailable") from None
        if value.get("error"):
            raise StorageError("volume_lease_unavailable", "another run may own this volume")
        return value

    def __enter__(self):
        if os.name != "nt" and not self.wsl:
            directory = Path(os.environ.get("XDG_DATA_HOME") or Path.home() / ".local/share") / "agentgo/swe-volume-leases"
            self.file_lock = FileLock(directory / (str(self.root.stat().st_dev) + ".lock"))
            self.file_lock.__enter__()
            self.metadata = {"coverage": "native_volume_lock", "coordinator": str(self.file_lock.path)}
            return self
        request = {"mode": "wsl" if self.wsl else "native", "root": str(self.root)}
        if self.wsl:
            request["distro"] = os.environ.get("WSL_DISTRO_NAME", "")
            parts = self.root.resolve().parts
            if len(parts) >= 3 and parts[1] == "mnt" and len(parts[2]) == 1 and parts[2].isalpha():
                request["drive"] = parts[2]
        env = {key: value for key, value in os.environ.items() if not key.startswith(("SWE_", "AGENTGO_", "OPENAI_", "ANTHROPIC_"))}
        code = base64.b64encode(HOST_SCRIPT.encode("utf-16le")).decode("ascii")
        options = {"creationflags": subprocess.CREATE_NO_WINDOW} if os.name == "nt" else {}
        self.process = subprocess.Popen(["powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", code],
                    stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, env=env, **options)
        try:
            self.process.stdin.write(json.dumps(request).encode() + b"\n")
            self.process.stdin.flush()
            self.metadata = self._line()
            if self.metadata.get("ready") is not True:
                raise StorageError("volume_lease_unavailable")
            self.metadata["coverage"] = "windows_host_volume_lock"
            return self
        except BaseException:
            self.__exit__(None, None, None)
            raise

    def free_bytes(self):
        if self.process is None:
            return None
        with self.mutex:
            if self.process.poll() is not None:
                raise StorageError("storage_observation_unavailable", "host lease exited")
            self.process.stdin.write(b"space\n")
            self.process.stdin.flush()
            return int(self._line()["free_bytes"])

    def ensure_alive(self):
        if self.process is not None and self.process.poll() is not None:
            raise StorageError("volume_lease_lost")
        if self.file_lock is not None and (self.file_lock.stream is None or self.file_lock.stream.closed):
            raise StorageError("volume_lease_lost")

    def __exit__(self, *args):
        if self.file_lock:
            self.file_lock.__exit__(*args)
        if self.process:
            try:
                if self.process.poll() is None:
                    self.process.stdin.write(b"release\n")
                    self.process.stdin.flush()
                self.process.stdin.close()
                self.process.wait(timeout=5)
            except (OSError, subprocess.TimeoutExpired):
                if self.process.poll() is None:
                    self.process.kill()
                    self.process.wait()
            finally:
                self.process.stdout.close()
                if not self.process.stdin.closed:
                    self.process.stdin.close()
