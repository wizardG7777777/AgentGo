You are working in the fmt repository.

# Reported behavior

debug format width calculation.

Implement the intended behavior demonstrated by the supplied regression tests. Preserve existing behavior outside this issue. The authoritative baseline failure excerpt is attached to this request.

# Prepared native environment

Dependencies and tools are prepared. Use the current working view as the source root. The declared regression scope is {"kind": "ctest-project", "paths": ["test"]}.

```text
cmake -S . -B .swe-build/cmake -G Ninja -DFMT_TEST=ON -DFMT_DOC=OFF
cmake --build .swe-build/cmake --parallel 4
ctest --test-dir .swe-build/cmake --output-on-failure
```

Do not modify tests, fixtures, dependency manifests or lockfiles, or build/test configuration. Do not fetch upstream fixes or use an old checkout or cached binary as the subject under test. Modify production source in this candidate and report concrete verification evidence.

The external SWE Runner checks the required regression cases and all originally passing tests within the declared scope. Pre-existing platform failures are recorded separately; they do not authorize unrelated changes.
