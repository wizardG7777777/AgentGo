"""Internal startup barrier: no SUT code runs until the supervisor sends argv."""
import json
import os
import subprocess
import sys


def main():
    # Unbuffered reads preserve any following Git patch bytes for the executed process.
    line = bytearray()
    descriptor = int(sys.argv[2]) if len(sys.argv) == 3 and sys.argv[1] == '--argv-fd' else 0
    if descriptor != 0 and (os.name != 'posix' or descriptor < 3):
        return 125
    while True:
        byte = os.read(descriptor, 1)
        if not byte:
            return 125
        if byte == b"\n":
            break
        line.extend(byte)
        if len(line) > 1024 * 1024:
            return 125
    argv = json.loads(line)
    if descriptor:
        os.close(descriptor)
    if not isinstance(argv, list) or not argv or not all(isinstance(x, str) for x in argv):
        return 125
    if os.name == "nt":
        # Windows has no POSIX exec replacement. Keep the barrier process in the
        # assigned Job and let CreateProcess inherit that Job and the pipe handles.
        process = subprocess.Popen(argv, stdin=0, stdout=1, stderr=2, creationflags=subprocess.CREATE_NO_WINDOW)
        return process.wait()
    os.execvpe(argv[0], argv, os.environ)
    return 125


if __name__ == "__main__":
    raise SystemExit(main())
