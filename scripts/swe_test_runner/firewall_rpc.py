"""Bounded, credential-free named-pipe client; launched with the base Python."""
import json
import os
import re
import sys
import threading
import time


def main():
    raw = sys.stdin.buffer.read(32769)
    if len(raw) > 32768:
        raise ValueError("request_too_large")
    request = json.loads(raw)
    name = request["pipe"]
    if not re.fullmatch(r"agentgo-swe-[a-f0-9]{32}", name):
        raise ValueError("invalid_pipe")
    timeout = min(20, max(1, float(request["timeout"])))
    # Also bounds blocking open/write/read if the calling Runner disappears.
    timer = threading.Timer(timeout, lambda: os._exit(124))
    timer.daemon = True
    timer.start()
    try:
        deadline = time.monotonic() + timeout
        while True:
            try:
                stream = open("\\\\.\\pipe\\" + name, "r+b", buffering=0)
                break
            except OSError:
                if time.monotonic() >= deadline:
                    raise TimeoutError("pipe_unavailable") from None
                time.sleep(0.05)
        payload = json.dumps(request["payload"], ensure_ascii=True).encode("utf-8") + b"\n"
        if len(payload) > 16384:
            raise ValueError("request_too_large")
        with stream:
            stream.write(payload)
            response = bytearray()
            while not response.endswith(b"\n"):
                byte = stream.read(1)
                if not byte or len(response) >= 65536:
                    raise ValueError("invalid_response")
                response.extend(byte)
        json.loads(response)
        sys.stdout.buffer.write(response)
        sys.stdout.buffer.flush()
    finally:
        timer.cancel()


if __name__ == "__main__":
    try:
        main()
    except Exception:
        sys.stderr.write("firewall_rpc_failed\n")
        raise SystemExit(125)
