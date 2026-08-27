You are working in a checkout of the Flask repository (Python web framework).

A prepared virtual environment is available. Run tests cross-platform with:
`uv run --no-sync python -m pytest -q`

# Issue report

`provide_automatic_options` only works in one direction. Setting it to `True`
on an individual view cannot re-enable automatic `OPTIONS` responses when the
global config `PROVIDE_AUTOMATIC_OPTIONS` disables them; only the reverse
(disabling per-view when globally enabled) works. Expected: a per-view
`provide_automatic_options=True` overrides the global disabling.

# Constraints

- Do not modify anything under `tests/`; the tests define the expected behavior.
- Make the minimal change under `src/flask/` so the failing tests pass and the
  full test suite stays green.
