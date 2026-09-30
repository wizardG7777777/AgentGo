"""Go/Cargo exec hook. Only the firewall broker is elevated, never this process."""
import os
from pathlib import Path
import subprocess
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent))
import firewall_windows


def main():
    session = firewall_windows.from_environment()
    if session is None or not os.environ.get(firewall_windows.ENV_RUN):
        raise RuntimeError("firewall_session_missing")
    program, args = sys.argv[1], sys.argv[2:]
    run_id = os.environ[firewall_windows.ENV_RUN]
    lease = session.acquire(program, run_id)
    try:
        environment = {key: value for key, value in os.environ.items()
                       if not key.startswith(("SWE_", "AGENTGO_", "OPENAI_", "ANTHROPIC_"))
                       or key in (firewall_windows.ENV_SESSION, firewall_windows.ENV_RUN)}
        environment["SWE_WINDOWS_FIREWALL_RULE_NAME"] = session.last_acquire_receipt["rule_name"]
        process = subprocess.Popen([program, *args], stdin=0, stdout=1, stderr=2,
                                   env=environment, creationflags=subprocess.CREATE_NO_WINDOW)
        try:
            return process.wait()
        except BaseException:
            process.kill()
            process.wait()
            raise
    finally:
        session.release(lease, run_id)


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as error:
        detail = str(error) if isinstance(error, firewall_windows.StorageError) else type(error).__name__
        print("SWE_FIREWALL_ERROR:" + detail, file=sys.stderr, flush=True)
        raise SystemExit(125)
