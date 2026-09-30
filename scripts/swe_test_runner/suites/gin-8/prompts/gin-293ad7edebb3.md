You are working in the gin repository.

# Reported behavior

Copy() copies Errors and Accepted fields.

Implement the intended behavior demonstrated by the supplied regression tests. Preserve existing behavior outside this issue. The authoritative baseline failure excerpt is attached to this request.

# Prepared native environment

Dependencies and tools are prepared. Use the current working view as the source root. The declared regression scope is {"kind": "module", "paths": ["."]}.

```text
go test -mod=readonly -count=1 -timeout 600s ./...
```

Do not modify tests, fixtures, dependency manifests or lockfiles, or build/test configuration. Do not fetch upstream fixes or use an old checkout or cached binary as the subject under test. Modify production source in this candidate and report concrete verification evidence.

The external SWE Runner checks the required regression cases and all originally passing tests within the declared scope. Pre-existing platform failures are recorded separately; they do not authorize unrelated changes.
