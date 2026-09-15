"""Optional lossless prefix recording for the Python preflight transport."""
from __future__ import annotations

import contextlib
import datetime as dt
import json
import os
from pathlib import Path
import uuid


class ProbeArchive:
    def __init__(self, endpoint, body):
        root = os.environ.get("SWE_PROBE_ARCHIVE")
        self.path = Path(root) / uuid.uuid4().hex if root else None
        self.body = None
        self.state = {"endpoint": endpoint, "started_at": dt.datetime.now(dt.timezone.utc).isoformat(),
                      "completed": False}
        if self.path:
            self.path.mkdir(parents=True)
            self.write("request.json", body)
            self.write("transport.json", self.state)
            self.body = (self.path / "response.body").open("wb")

    def write(self, name, data):
        if self.path:
            (self.path / name).write_text(json.dumps(data, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")

    def record_bytes(self, data):
        if self.body:
            self.body.write(data)
            self.body.flush()

    def lines(self, response):
        for line in response:
            self.record_bytes(line)
            yield line

    def headers(self, response):
        self.state.update(status=response.status, content_type=response.headers.get("Content-Type"))
        self.write("transport.json", self.state)

    def finish(self, error=None):
        if self.body:
            self.body.close()
        self.state.update(completed=error is None, ended_at=dt.datetime.now(dt.timezone.utc).isoformat())
        if error is not None:
            self.state["error_type"] = type(error).__name__
        self.write("transport.json", self.state)


@contextlib.contextmanager
def record_probe(endpoint, body):
    archive = ProbeArchive(endpoint, body)
    try:
        yield archive
    except BaseException as error:
        archive.finish(error)
        raise
    else:
        archive.finish()
