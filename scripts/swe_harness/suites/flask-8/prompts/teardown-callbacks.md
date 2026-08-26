You are working in a checkout of the Flask repository (Python web framework).

A prepared virtual environment is available. Run tests cross-platform with:
`uv run --no-sync python -m pytest -q`

# Issue report

Teardown callbacks are not all executed when one of them fails. If a
`teardown_request` (or `teardown_appcontext`) callback raises an error, the
remaining callbacks registered for that phase are silently skipped. Expected
behavior: every registered teardown callback runs even when earlier ones raise
(the errors should still propagate after all callbacks have run).

# Constraints

- Do not modify anything under `tests/`; the tests define the expected behavior.
- Make the minimal change under `src/flask/` so the failing tests pass and the
  full test suite stays green.
