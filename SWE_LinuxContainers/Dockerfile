# syntax=docker/dockerfile:1
# One reusable test-series image. The AgentGo binary is a runtime input.
FROM ghcr.io/astral-sh/uv:0.8.19 AS uv
FROM ubuntu:24.04 AS os-base
# Existence only. Empty/malformed configuration is deliberately accepted.
# The secret is not read or copied into an image layer.
RUN --mount=type=secret,id=swe_config,required=true test -f /run/secrets/swe_config
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/*

# uv only downloads CPython and exports the existing lockfile in this builder.
# Final test containers have Python 3.13 + pip; no uv or Go toolchain.
FROM os-base AS dependencies
COPY --from=uv /uv /usr/local/bin/uv
ENV UV_PYTHON_INSTALL_DIR=/opt/python
RUN uv python install 3.13.5 \
    && ln -s "$(uv python find 3.13.5)" /opt/python/python \
    && "$(uv python find 3.13.5)" -m venv /opt/python-tools/.venv \
    && ln -s /opt/python-tools/.venv/bin/python /usr/local/bin/python3
COPY runtime/SWE_LinuxContainers/prepare_dependencies.py /opt/agentgo/SWE_LinuxContainers/prepare_dependencies.py
COPY runtime/scripts/swe_test_runner/suites/flask-8 /opt/agentgo/scripts/swe_test_runner/suites/flask-8
RUN python3 /opt/agentgo/SWE_LinuxContainers/prepare_dependencies.py

FROM os-base AS test
LABEL io.agentgo.swe-linux.lifecycle="reports-and-behavior-v2"
LABEL io.agentgo.swe-linux.runtime="external-agentgo-v1"
LABEL io.agentgo.swe-linux.suite="flask-8"
COPY --from=dependencies /opt/python /opt/python
COPY --from=dependencies /opt/python-tools /opt/python-tools
RUN ln -s /opt/python-tools/.venv/bin/python /usr/local/bin/python3 \
    && ln -s /opt/python-tools/.venv/bin/python /usr/local/bin/python \
    && ln -s /opt/python-tools/.venv/bin/pip /usr/local/bin/pip \
    && ln -s /opt/python-tools/.venv/bin/pip /usr/local/bin/pip3
COPY --from=dependencies /opt/swe /opt/swe
COPY runtime /opt/agentgo
COPY image.json /opt/agentgo/image.json
ENV PYTHONUNBUFFERED=1 PYTHONUTF8=1 PIP_DISABLE_PIP_VERSION_CHECK=1
WORKDIR /opt/agentgo
ENTRYPOINT ["python3", "/opt/agentgo/SWE_LinuxContainers/entrypoint.py"]
