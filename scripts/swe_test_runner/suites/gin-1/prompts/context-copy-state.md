You are working in a checkout of Gin, the Go HTTP framework.

# Issue report

Middleware may attach errors to a context and set its accepted response formats.
When application code calls `Context.Copy()` before handing the copy to another
goroutine, that error history and content-negotiation state are lost.

The copied context should preserve the existing Errors and Accepted entries.
Changing either slice on the copy must not change the original context's slice.
A context without these fields should still copy normally, preserving nil values.
Existing Copy behavior and other request handling must remain compatible.

# Environment and verification

Go 1.25.0 and the module dependencies are prepared. The same commands work in
PowerShell and zsh:

```text
go test -count=1 -timeout 180s -run '^TestContextCopy' .
go test -count=1 -timeout 180s ./...
```

Fix production Go source. Do not change `_test.go` files, test fixtures, go.mod,
go.sum, or build/test configuration. Do not fetch upstream fixes or patches.
Use this checkout and the supplied failure evidence to investigate the issue.
The external SWE Runner independently judges the supplied regression tests and
the complete module suite, recording any pre-existing platform failures separately.
