You are working in a checkout of the Flask repository (Python web framework).

A prepared virtual environment is available. Run tests cross-platform with:
`uv run --no-sync python -m pytest -q`

# Issue report

Setting Flask's `SERVER_NAME` config to an IPv6 address with brackets and a
port — e.g. `"[::1]:8080"` — is parsed incorrectly: the host and port are not
split the way they are for a regular `host:port` value like `"localhost:8080"`.
Expected behavior: host `::1`, port `8080`. This affects the dev server startup
path (`flask run` / `app.run()` picking up `SERVER_NAME`).

# Constraints

- Do not modify anything under `tests/`; the tests define the expected behavior.
- Make the minimal change under `src/flask/` so the failing tests pass and the
  full test suite stays green.
