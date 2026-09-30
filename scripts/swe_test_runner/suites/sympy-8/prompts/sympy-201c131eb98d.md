You are working in the sympy repository.

# Reported behavior

Set sort_key on NaN.

Implement the intended behavior demonstrated by the supplied regression tests. Preserve existing behavior outside this issue. The authoritative baseline failure excerpt is attached to this request.

# Prepared native environment

Dependencies and tools are prepared. Use the current working view as the source root. The declared regression scope is {"kind": "test-modules", "paths": ["sympy/core/tests/test_expr.py", "sympy/printing/tests/test_str.py"]}.

```text
python -m pytest -q sympy/core/tests/test_expr.py sympy/printing/tests/test_str.py
```

Do not modify tests, fixtures, dependency manifests or lockfiles, or build/test configuration. Do not fetch upstream fixes or use an old checkout or cached binary as the subject under test. Modify production source in this candidate and report concrete verification evidence.

The external SWE Runner checks the required regression cases and all originally passing tests within the declared scope. Pre-existing platform failures are recorded separately; they do not authorize unrelated changes.
