"""Conservative CMake output classification; source evidence is never discarded."""
from __future__ import annotations

import hashlib
from pathlib import Path, PurePosixPath
import re

SCHEMA = 'agentgo.swe-generated-build-artifacts/v1'
_METADATA = {'CMakeDirectoryInformation.cmake', 'DependInfo.cmake', 'cmake_clean.cmake',
    'cmake_clean_target.cmake', 'build.make', 'compiler_depend.make', 'compiler_depend.ts',
    'compiler_depend.internal', 'depend.make', 'flags.make', 'link.txt', 'progress.make',
    'progress.marks', 'Makefile.cmake', 'Makefile2', 'TargetDirectories.txt',
    'cmake.check_cache', 'CMakeOutput.log', 'CMakeError.log', 'CMakeConfigureLog.yaml',
    'rules.ninja', 'VerifyGlobs.cmake', 'cmake.verify_globs'}
_DIRECTORY_OUTPUTS = {'Makefile', 'cmake_install.cmake', 'CTestTestfile.cmake'}


def _generated_name(relative):
    parts = PurePosixPath(relative).parts
    name = parts[-1]
    if name in _DIRECTORY_OUTPUTS:
        return True
    if relative in {'CMakeCache.txt', 'build.ninja', '.ninja_log', '.ninja_deps',
                    'compile_commands.json', 'DartConfiguration.tcl'}:
        return True
    if len(parts) >= 3 and parts[-3:-1] == ('Testing', 'Temporary') and name in {
            'CTestCostData.txt', 'LastTest.log', 'LastTestsFailed.log', 'LastTestsDisabled.log'}:
        return True
    if 'CMakeFiles' in parts:
        tail = '/'.join(parts[parts.index('CMakeFiles') + 1:])
        if name in _METADATA or name.endswith(('.o', '.obj', '.o.d', '.obj.d')):
            return True
        if re.fullmatch(r'[0-9.]+/(?:CMake(?:C|CXX)Compiler\.cmake|CMakeSystem\.cmake|'
                        r'CMakeDetermineCompilerABI_(?:C|CXX)\.bin|'
                        r'CompilerIdC/CMakeCCompilerId\.c|CompilerIdCXX/CMakeCXXCompilerId\.cpp|'
                        r'CompilerId(?:C|CXX)/(?:a\.out|CompilerId(?:C|CXX)\.exe))', tail):
            return True
    # A proven out-of-source test directory also holds extensionless executables
    # and runtime data. Original/declared inputs and source files stay protected.
    return name.endswith(('.a', '.lib', '.dll', '.so', '.dylib', '.exe')) or (
        len(parts) >= 2 and parts[-2] == 'test' and re.fullmatch(r'[A-Za-z0-9_-]+', name) is not None)


def classify(files, caches, source_root, original_inputs):
    """Require an isolated build tree, matching CMake source/binary roots and known outputs.

    original_inputs contains Git-tracked inputs for a live checkout, or the
    pre-model input inventory for archived evidence. Generated outputs may be
    untracked without being ignored; that does not turn them into source tests.
    """
    source = Path(source_root)
    classified, roots = {}, {}
    for cache_name, data in caches.items():
        parts = PurePosixPath(cache_name).parts
        if len(parts) < 2 or parts[-1] != 'CMakeCache.txt' or any(p in {'.', '..'} for p in parts):
            continue
        prefix = '/'.join(parts[:-1]) + '/'
        if any(name.startswith(prefix) for name in original_inputs):
            continue
        try:
            content = data.decode('utf-8')
        except UnicodeDecodeError:
            continue
        fields = {}
        for line in content.splitlines():
            if line.startswith(('CMAKE_HOME_DIRECTORY:INTERNAL=', 'CMAKE_CACHEFILE_DIR:INTERNAL=')):
                key, value = line.split('=', 1)
                if key in fields:
                    fields = {}
                    break
                fields[key] = value
        if len(fields) != 2:
            continue
        home = Path(fields['CMAKE_HOME_DIRECTORY:INTERNAL'])
        binary = Path(fields['CMAKE_CACHEFILE_DIR:INTERNAL'])
        if (not home.is_absolute() or not binary.is_absolute() or '..' in home.parts or '..' in binary.parts
                or not home.is_relative_to(source) or binary != home.joinpath(*parts[:-1])):
            continue
        selected = {name: digest for name, digest in files.items()
                    if name.startswith(prefix) and _generated_name(name[len(prefix):])}
        if selected:
            classified.update(selected)
            roots[prefix[:-1]] = {'source': str(home), 'binary': str(binary), 'cache': cache_name,
                                 'cache_sha256': hashlib.sha256(data).hexdigest()}
    return {'schema': SCHEMA, 'files': dict(sorted(classified.items())), 'roots': roots,
            'retained_in_input_identity': True, 'retained_in_archive': True}
