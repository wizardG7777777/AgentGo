You are working in a checkout of the Flask repository (Python web framework).

A prepared virtual environment is available. Run tests cross-platform with:
`uv run --no-sync python -m pytest -q`

# Issue report

The test client's `session_transaction` does not work with an IPv6 `base_url`
such as `"http://[::1]:8000/"`: the session cookie written inside the
transaction block is not visible to subsequent requests made with the same
IPv6 `base_url`. The same flow works fine with a hostname-based `base_url`
like `"http://localhost/"`.

# Constraints

- Do not modify anything under `tests/`; the tests define the expected behavior.
- Make the minimal change under `src/flask/` so the failing tests pass and the
  full test suite stays green.
