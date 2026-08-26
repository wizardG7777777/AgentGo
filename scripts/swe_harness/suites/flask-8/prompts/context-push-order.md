You are working in a checkout of the Flask repository (Python web framework).

A prepared virtual environment is available. Run tests cross-platform with:
`uv run --no-sync python -m pytest -q`

# Issue report

When the test client follows redirects (`client.get(..., follow_redirects=True)`)
and a redirect step modifies the session, the session state visible after the
request chain completes is wrong — it reflects an intermediate redirect
response rather than the final response's session state.

# Constraints

- Do not modify anything under `tests/`; the tests define the expected behavior.
- Make the minimal change under `src/flask/` so the failing tests pass and the
  full test suite stays green.
