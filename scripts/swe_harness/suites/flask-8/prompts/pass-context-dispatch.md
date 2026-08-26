You are working in a checkout of the Flask repository (Python web framework).

A prepared virtual environment is available. Run tests cross-platform with:
`uv run --no-sync python -m pytest -q`

# Task

Refactor Flask's internal request-dispatch helpers so they no longer rely on
proxy objects for the current app context: the `Flask` methods involved in
request dispatch (e.g. URL matching / error handling during dispatch) should
take the current `AppContext` as their first parameter instead.

Backward compatibility is required: if a subclass overrides these methods with
the *old* signature (no `AppContext` parameter), the override must be detected,
emit a `DeprecationWarning`, and continue to work.

# Constraints

- Do not modify anything under `tests/`; the tests define the expected behavior
  (including the deprecation-warning path for old-style overrides).
- Make the change under `src/flask/` so the failing tests pass and the full
  test suite stays green.
