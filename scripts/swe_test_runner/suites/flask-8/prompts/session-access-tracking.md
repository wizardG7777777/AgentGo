You are working in a checkout of the Flask repository (Python web framework).

A prepared virtual environment is available. Run tests cross-platform with:
`uv run --no-sync python -m pytest -q`

# Issue report

Session access tracking is incomplete. Operations that only look at the
session's keys without reading values — e.g. `'key' in session` or
`len(session)` — do not mark the session as *accessed*. Downstream behavior
that depends on access tracking (for example deciding whether to send
`Vary: Cookie` or refresh a permanent session's lifetime) therefore behaves
as if the session was never touched.

# Constraints

- Do not modify anything under `tests/`; the tests define the expected behavior.
- Make the minimal change under `src/flask/` so the failing tests pass and the
  full test suite stays green.
