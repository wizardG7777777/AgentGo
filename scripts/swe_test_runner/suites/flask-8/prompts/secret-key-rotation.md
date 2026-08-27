You are working in a checkout of the Flask repository (Python web framework).

A prepared virtual environment is available. Run tests cross-platform with:
`uv run --no-sync python -m pytest -q`

# Issue report

Session signing key rotation is broken. The documented configuration order for
`SECRET_KEY_FALLBACKS` is *most recent key first*. However, when fallbacks are
configured, the signing/verification order does not match what the
`itsdangerous` serializer expects (its interface expects the oldest key at
index zero and the active signing key at the end of the list). As a result,
cookies are signed with the wrong key when rotation is in use.

# Constraints

- Do not modify anything under `tests/`; the tests define the expected behavior.
- Make the minimal change under `src/flask/` so the failing tests pass and the
  full test suite stays green.
