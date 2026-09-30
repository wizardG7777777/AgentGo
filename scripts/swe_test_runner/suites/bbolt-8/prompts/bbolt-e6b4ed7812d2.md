You are working in the bbolt repository.

# Reported behavior

Rollback leaves transaction allocation bookkeeping behind.

Implement the intended behavior demonstrated by the supplied regression tests. Preserve existing behavior outside this issue. The authoritative baseline failure excerpt is attached to this request.

# Prepared native environment

Dependencies and tools are prepared. Use the current working view as the source root. The declared regression scope is {"kind": "upstream-unit-short", "packages": [".", "./internal/...", "./cmd/bbolt/..."], "excluded_modes": ["separate failpoint instrumentation", "power-failure robustness", "tests skipped by upstream short mode"]}.

```text
go test -mod=readonly -short -count=1 -timeout 600s . ./internal/... ./cmd/bbolt/...
```

Do not modify tests, fixtures, dependency manifests or lockfiles, or build/test configuration. Do not fetch upstream fixes or use an old checkout or cached binary as the subject under test. Modify production source in this candidate and report concrete verification evidence.

The external SWE Runner checks the required regression cases and all originally passing tests within the declared scope. Pre-existing platform failures are recorded separately; they do not authorize unrelated changes.

The declared scope is the upstream ordinary unit-test packages in short mode. Separate failpoint instrumentation and power-failure robustness modes are outside this task; do not alter tests or add skips.
