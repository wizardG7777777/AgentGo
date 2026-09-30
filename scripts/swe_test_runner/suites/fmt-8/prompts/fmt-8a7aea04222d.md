You are working in the fmt repository.

# Reported behavior

out-of-bounds read in error code format parsing.

Implement the intended behavior demonstrated by the supplied regression tests. Preserve existing behavior outside this issue. The authoritative baseline failure excerpt is attached to this request.

# Prepared native environment

Dependencies and tools are prepared. Use the current working view as the source root. The declared regression scope is {"kind": "ctest-project", "paths": ["test"]}.

This task requires AddressSanitizer. The external runner enables it during qualification and judging. Preserve it when reproducing the failure. The Windows environment provides MSVC; Linux provides a native C++ compiler.

For a fresh Linux build:

```text
cmake -S . -B .swe-build/cmake -G Ninja -DFMT_TEST=ON -DFMT_DOC=OFF -DCMAKE_BUILD_TYPE=Debug "-DCMAKE_CXX_FLAGS=-fsanitize=address -fno-omit-frame-pointer" -DCMAKE_EXE_LINKER_FLAGS=-fsanitize=address
```

For a fresh Windows build:

```text
cmake -S . -B .swe-build/cmake -G Ninja -DFMT_TEST=ON -DFMT_DOC=OFF -DCMAKE_BUILD_TYPE=Debug "-DCMAKE_CXX_FLAGS=/EHsc /fsanitize=address" "-DCMAKE_CXX_FLAGS_DEBUG=/Zi /Od" "-DCMAKE_EXE_LINKER_FLAGS=/INCREMENTAL:NO" "-DCMAKE_EXE_LINKER_FLAGS_DEBUG=/DEBUG /INCREMENTAL:NO"
```

Then build and test:

```text
cmake --build .swe-build/cmake --parallel 2
ctest --test-dir .swe-build/cmake --output-on-failure
```

Do not modify tests, fixtures, dependency manifests or lockfiles, or build/test configuration. Do not fetch upstream fixes or use an old checkout or cached binary as the subject under test. Modify production source in this candidate and report concrete verification evidence.

The external SWE Runner checks the required regression cases and all originally passing tests within the declared scope. Pre-existing platform failures are recorded separately; they do not authorize unrelated changes.
