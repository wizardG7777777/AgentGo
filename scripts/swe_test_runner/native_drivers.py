"""Native language runners; commands and subject roots are recorded by the harness."""
from __future__ import annotations

import hashlib
import json
import os
import platform
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tomllib

import native_go
import native_reports as reports
import native_vitest
import processes as managed_processes
import storage as resource_storage


def executable(name):
    path = shutil.which(name)
    if not path:
        raise RuntimeError(f"required native tool is missing: {name}")
    return path


def base_environment():
    env = os.environ.copy()
    for name in list(env):
        if name.startswith(("SWE_", "AGENTGO_", "OPENAI_", "ANTHROPIC_")):
            del env[name]
    for name in ("GOFLAGS", "GOWORK", "PYTEST_ADDOPTS", "PYTEST_PLUGINS", "NODE_OPTIONS", "RUSTFLAGS", "RUSTDOCFLAGS", "CARGO_ENCODED_RUSTFLAGS"):
        env.pop(name, None)
    env.update(CI="1", NO_COLOR="1", FORCE_COLOR="0", PYTHONUTF8="1", PYTHONDONTWRITEBYTECODE="1")
    return env


class Driver:
    def __init__(self, config, task, root):
        self.config, self.task, self.root = config, task, root.resolve()
        self.options = dict(config.suite.options.get("environment", {}))
        self.options.update(task.metadata.get("environment", {}))
        self.build = self.root / ".swe-build"
        self.build.mkdir(exist_ok=True)
        self.env = base_environment()
        self.commands = []
        self.env.update(self.options.get("env", {}))

    def command(self, argv, *, cwd=None, check=True, timeout=1200, label="command", firewall_programs=()):
        if resource_storage.active() is not None:
            result = managed_processes.run(argv, cwd=cwd or self.root, env=self.env,
                                           check=check, timeout=timeout, label=label, firewall_programs=firewall_programs)
            self.commands.append({"command_ref": result.command_ref, "label": label,
                                  "exit_code": result.returncode})
            return result
        cwd = Path(cwd or self.root)
        command = [str(x) for x in argv]
        record = {"argv": command, "cwd": str(cwd), "label": label, "status": "running"}
        self.commands.append(record)
        serial = len(self.commands)
        stdout, stderr = self.build / f"{serial:03d}-{label}.stdout", self.build / f"{serial:03d}-{label}.stderr"
        with stdout.open("wb") as out, stderr.open("wb") as err:
            process = subprocess.Popen(command, cwd=cwd, env=self.env, stdout=out, stderr=err,
                                       **({"start_new_session": True} if os.name == "posix" else {}))
            record["pid"] = process.pid
            print(json.dumps({"native_step": label, "task": self.task.task_id, "pid": process.pid}), flush=True)
            try:
                code = process.wait(timeout=timeout)
            except BaseException:
                if process.poll() is None:
                    if os.name == "nt":
                        subprocess.run(["taskkill", "/PID", str(process.pid), "/T", "/F"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                    else:
                        os.killpg(process.pid, signal.SIGKILL)
                    process.wait()
                record.update(status="interrupted", exit_code=process.returncode)
                raise
        completed = subprocess.CompletedProcess(command, code, stdout.read_bytes(), stderr.read_bytes())
        record.update(status="completed", exit_code=code)
        if check and completed.returncode:
            raise RuntimeError(f"native {label} failed (exit={completed.returncode}); logs: {self.build}")
        return completed

    def prepare(self):
        raise NotImplementedError

    def prepare_existing(self):
        """Recreate environment variables without reinstalling dependencies."""

    def identity(self):
        return {"driver": self.config.suite.options["driver"], "subject_root": str(self.root), "options": self.options}

    def agent_environment(self):
        result = os.environ.copy()
        result.update(self.env)
        return result


class GoDriver(Driver):
    def prepare_existing(self):
        self.env.update(GOTOOLCHAIN=self.options.get("toolchain", "go1.25.0"), GOWORK="off", GOFLAGS="",
                        CGO_ENABLED=str(self.options.get("cgo", 0)))
        self.env.pop("GOOS", None)
        self.env.pop("GOARCH", None)
        if self.config.suite.project == "gorm":
            self.env.update(GORM_DIALECT="sqlite", GORM_DSN=str(self.build / "gorm.db"))

    def prepare(self):
        self.prepare_existing()
        for relative in self.options.get("roots", ["."]):
            self.command(["go", "mod", "download", "all"], cwd=self.root / relative, label="go-modules")
        self.identity()

    def identity(self):
        version = self.command(["go", "version"], label="go-version").stdout.decode().strip()
        if self.env["GOTOOLCHAIN"] not in version.split():
            raise ValueError("Go suite toolchain mismatch")
        modules = []
        for relative in self.options.get("roots", ["."]):
            directory = (self.root / relative).resolve()
            module = json.loads(self.command(["go", "list", "-mod=readonly", "-m", "-json"], cwd=directory, label="go-origin").stdout)
            if not module.get("Main") or Path(module["Dir"]).resolve() != directory:
                raise ValueError("Go tests are not bound to the task module")
            self.command(["go", "mod", "verify"], cwd=directory, label="go-verify")
            modules.append(module)
        if self.config.suite.project == "gorm" and "tests" in self.options.get("roots", []):
            subject = json.loads(self.command(["go", "list", "-mod=readonly", "-m", "-json", "gorm.io/gorm"], cwd=self.root / "tests", label="gorm-origin").stdout)
            if Path(subject.get("Replace", {}).get("Dir", "")).resolve() != self.root:
                raise ValueError("GORM integration tests resolve a different checkout")
        return dict(super().identity(), go_version=version, modules=modules)

    def test(self, targeted, output):
        items, human = [], []
        roots = self.task.metadata.get("target_roots", self.options.get("roots", ["."])) if targeted else self.options.get("roots", ["."])
        for index, relative in enumerate(roots):
            cwd = self.root / relative
            selectors = self.task.test_selectors if targeted else ()
            packages = self.options.get("packages", ["./..."])
            mode = ["-short"] if self.options.get("short") else []
            collected = self.command(["go", "test", "-mod=readonly", "-json", *mode, "-list", ".", *packages], cwd=cwd, label="go-list", check=False)
            expected = native_go.enumeration(native_go.decode_events(managed_processes.stdout_source(collected)), selectors) if collected.returncode == 0 else None
            if targeted and expected == []:
                continue
            argv = ["go", "test", "-mod=readonly", "-json", "-count=1", *mode, "-timeout", f"{self.options.get('timeout', 300)}s"]
            if selectors:
                argv += ["-run", "^(" + "|".join(re.escape(name) for name in selectors) + ")$"]
            argv += packages
            completed = self.command(argv, cwd=cwd, label="go-test", check=False)
            events = native_go.decode_events(managed_processes.stdout_source(completed))
            managed_processes.save_output_reference(completed, output / f"go-{index}.jsonl")
            human.extend(event.get("Output", "") for event in events)
            human.append(managed_processes.output_summary(completed).decode(errors="replace")[-4096:])
            parsed = native_go.parse_report(events, expected, completed.returncode)
            parsed["case_outcomes"] = {event["Package"] + "::" + event["Test"]: event["Action"]
                                       for event in events if event.get("Test") and event["Action"] in {"run", "pass", "fail", "skip"}}
            items.append(parsed)
        if not items:
            raise ValueError("Go target selected no test-bearing package")
        return reports.merge(items, "go"), "".join(human)[-65536:]


class PytestDriver(Driver):
    def prepare_existing(self):
        venv = self.build / "venv"
        self.python = venv / ("Scripts/python.exe" if os.name == "nt" else "bin/python")
        self.env["PATH"] = str(self.python.parent) + os.pathsep + self.env["PATH"]
        self.env["PYTHONPATH"] = os.pathsep.join(["src", "."])

    def prepare(self):
        self.prepare_existing()
        if not self.python.exists():
            self.command([Path(sys.executable).resolve(), "-m", "venv", str(self.build / "venv")], label="python-venv")
        packages = self.options.get("packages", ["pytest==8.4.2"])
        self.command(["uv", "pip", "install", "--python", str(self.python), *packages], label="python-dependencies")
        if self.config.suite.project == "pytest":
            self.env["SETUPTOOLS_SCM_PRETEND_VERSION_FOR_PYTEST"] = self.options.get("version", "9.1.0.dev0")
            self.command(["uv", "pip", "install", "--python", str(self.python), "-e", str(self.root)], label="pytest-subject")
        elif (self.root / "setup.py").exists() or (self.root / "pyproject.toml").exists():
            self.command(["uv", "pip", "install", "--python", str(self.python), "--no-deps", "-e", str(self.root)], label="python-subject")
        self.identity()

    def identity(self):
        subject = self.options["subject"]
        code = "import importlib.util,importlib.metadata as m,json,sys; x=importlib.util.find_spec(sys.argv[1]); print(json.dumps({'subject_file':getattr(x,'origin',None),'python':sys.version,'packages':sorted((d.metadata['Name'],d.version) for d in m.distributions())}))"
        value = json.loads(self.command([self.python, "-c", code, subject], label="python-origin").stdout)
        if not Path(value["subject_file"]).resolve().is_relative_to(self.root):
            raise ValueError("Python subject import escaped the task checkout")
        return dict(super().identity(), **value)

    def test(self, targeted, output):
        report_path = output / "pytest.json"
        env_before = dict(self.env)
        self.env["PYTHONPATH"] += os.pathsep + str(Path(__file__).resolve().parent)
        self.env["AGENTGO_NATIVE_PYTEST_REPORT"] = str(report_path)
        self.env["AGENTGO_NATIVE_PYTHON_SUBJECT"] = self.options["subject"]
        targets = self.task.metadata["target_args"] if targeted else self.task.metadata["regression_args"]
        try:
            completed = self.command([self.python, "-m", "pytest", "-q", "-p", "native_pytest_reporter", *targets], label="pytest", check=False)
        finally:
            self.env = env_before
        if not report_path.is_file():
            if completed.returncode == 1:
                # pytest may itself be the repaired subject. A source import or
                # bootstrap failure can prevent loading the reporter; retain the
                # original command and report failure without inventing test runs.
                value = reports.report({}, engine="pytest", exit_code=1,
                                       errors=[{"nodeid": "<pytest-bootstrap>", "phase": "build"}])
                return value, managed_processes.output_summary(completed).decode("utf-8", errors="replace")
            raise ValueError("pytest did not produce its root-session report")
        data = json.loads(report_path.read_text(encoding="utf-8"))
        if data["exit_code"] != completed.returncode or not Path(data["subject_file"]).resolve().is_relative_to(self.root):
            raise ValueError("pytest result does not belong to the task subject")
        value = reports.report(data["outcomes"], engine="pytest", exit_code=completed.returncode,
                               errors=data["errors"], expected=data["inventory"])
        return value, managed_processes.output_summary(completed).decode("utf-8", errors="replace")


class VitestDriver(Driver):
    def prepare_existing(self):
        package = json.loads((self.root / "package.json").read_text(encoding="utf-8"))
        manager = self.options.get("manager") or package.get("packageManager", "pnpm@10.19.0").split("+")[0]
        name, version = manager.split("@", 1)
        if name not in {"pnpm", "yarn"} or not re.fullmatch(r"[0-9.]+", version):
            raise ValueError("native Node suite requires a pinned pnpm or Yarn version")
        self.manager_name, self.manager_version = name, version
        attempt = resource_storage.active()
        tools_root = attempt.storage.cache_dir("tools") if attempt else Path(os.environ.get("SWE_NATIVE_TOOLS") or (self.config.testbed / "tools"))
        cache = tools_root / "node-managers" / (name + "-" + version)
        self.manager_root = cache
        self.manager_cli = cache / "node_modules" / name / "bin" / ("pnpm.cjs" if name == "pnpm" else "yarn.js")
        self.node = executable("node")
        self.env.update(HUSKY="0", COREPACK_ENABLE_PROJECT_SPEC="0")
        self.env["PATH"] = os.pathsep.join([str(cache / "node_modules" / ".bin"), str(self.root / "node_modules" / ".bin"), self.env["PATH"]])

    def manager(self, *args, **kwargs):
        return self.command([self.node, self.manager_cli, *args], **kwargs)

    def prepare(self):
        self.prepare_existing()
        if not self.manager_cli.exists():
            npm = Path(self.node).parent / "node_modules" / "npm" / "bin" / "npm-cli.js"
            if not npm.is_file():
                installed = Path(executable("npm")).resolve()
                npm = installed if installed.name == "npm-cli.js" else Path(self.node).parent.parent / "lib" / "node_modules" / "npm" / "bin" / "npm-cli.js"
            self.manager_root.mkdir(parents=True, exist_ok=True)
            self.command([self.node, npm, "install", "--prefix", self.manager_root, "--ignore-scripts", "--no-audit", "--no-fund",
                          f"{self.manager_name}@{self.manager_version}"], label="node-manager")
        args = ["install", "--frozen-lockfile"]
        if self.manager_name == "pnpm":
            args += ["--ignore-scripts"]
            if resource_storage.active() is not None:
                args += ["--store-dir", str(resource_storage.active().storage.path / "cache" / "pnpm")]
        else:
            args += ["--ignore-scripts", "--non-interactive"]
        self.manager(*args, label="node-dependencies", timeout=1800)
        if os.name == "nt":
            import native_windows_links
            resource_storage.atomic_json(self.build / "dependency-links.json",
                native_windows_links.normalize_node_junctions(self.root))
        # esbuild's platform package is normally installed as an optional dependency.
        self.identity()

    def identity(self):
        version = self.command([self.node, "--version"], label="node-version").stdout.decode().strip()
        locks = {name: hashlib.sha256((self.root / name).read_bytes()).hexdigest()
                 for name in ("package.json", "pnpm-lock.yaml", "yarn.lock") if (self.root / name).exists()}
        vitest = json.loads((self.root / "node_modules" / "vitest" / "package.json").read_text(encoding="utf-8"))["version"]
        value = dict(super().identity(), node_version=version, vitest_version=vitest,
                     manager=f"{self.manager_name}@{self.manager_version}", locks=locks)
        if os.name == "nt":
            value["dependency_links"] = resource_storage.read_json(self.build / "dependency-links.json")
        return value

    def test(self, targeted, output, *, baseline_added_files=()):
        targets = self.task.metadata["target_args"] if targeted else self.task.metadata["regression_args"]
        if targeted and baseline_added_files:
            raise ValueError("new-test baseline exception cannot apply to targeted tests")
        selected = reports.vitest_paths(self.root, targets, baseline_added_files=baseline_added_files)
        absent = [name for name in targets if not (self.root / name).exists()]
        if not selected:
            return reports.report({}, engine="vitest", exit_code=0, baseline_added_files=absent), "No pre-existing declared test files; no tests executed before applying the test patch.\n"
        # CLI filters are only a discovery hint, including for absolute paths.
        # Native Vitest selects exact specifications before executing modules.
        filters = [path.as_posix() + ("/" if directory else "") for path, directory in selected]
        report_path = output / "vitest.json"
        test_args = list(self.options.get("test_args", []))
        version = json.loads((self.root / "node_modules" / "vitest" / "package.json").read_text(encoding="utf-8"))["version"]
        major = int(version.split(".")[0])
        if major not in {3, 4}:
            raise ValueError("native Vitest selection requires a validated Vitest 3 or 4 API")
        if major == 4:
            test_args = [value for value in test_args if not value.startswith("--minWorkers")]
        arguments = ["run", "--reporter=json", "--outputFile", str(report_path), *test_args, *filters]
        helper, request = native_vitest.command_files(self.root, output, selected, arguments)
        completed = self.command([executable("node"), helper, request], label="vitest", check=False, timeout=1200)
        if not report_path.exists():
            raise ValueError("Vitest did not produce a JSON report")
        value = reports.vitest(report_path, self.root, completed.returncode, selected=selected)
        if absent:
            value["absent_new_test_files"] = absent
        return value, managed_processes.output_summary(completed).decode("utf-8", errors="replace")


def rust_target(host_platform, machine):
    architecture = {"arm64": "aarch64", "aarch64": "aarch64", "x86_64": "x86_64", "amd64": "x86_64"}.get(machine.lower())
    if architecture is None:
        raise ValueError("unsupported native Rust architecture")
    if host_platform == "win32":
        return "x86_64-pc-windows-gnu" if architecture == "x86_64" else "aarch64-pc-windows-msvc"
    if host_platform == "darwin":
        return architecture + "-apple-darwin"
    if host_platform == "linux":
        return architecture + "-unknown-linux-gnu"
    raise ValueError("unsupported native Rust platform")


class CargoDriver(Driver):
    def prepare_existing(self):
        target = rust_target(sys.platform, platform.machine())
        self.toolchain = self.options.get("toolchain", "1.90.0") + "-" + target
        self.env["RUSTUP_TOOLCHAIN"] = self.toolchain
        attempt = resource_storage.active()
        cache = self.build / "cargo" if attempt else Path(os.environ.get("SWE_NATIVE_TOOLS") or self.config.testbed.parent / "tools") / "cargo-cache" / self.config.suite.project
        cache.mkdir(parents=True, exist_ok=True)
        self.env["CARGO_TARGET_DIR"] = str(cache)
        self.env["CARGO_BUILD_JOBS"] = str(attempt.policy.max_build_jobs if attempt else 4)

    def prepare(self):
        self.prepare_existing()
        locked = ['--locked'] if getattr(self.config, '_frozen_dependency_files', {}) else []
        self.command(["cargo", "fetch", *locked], label="cargo-dependencies", timeout=1200)
        self.identity()

    def metadata(self):
        metadata = json.loads(self.command(["cargo", "metadata", "--locked", "--no-deps", "--format-version=1"], label="cargo-origin").stdout)
        if Path(metadata["workspace_root"]).resolve() != self.root:
            raise ValueError("Cargo workspace is not the task checkout")
        return metadata

    def identity(self):
        metadata = self.metadata()
        version = self.command(["rustc", "--version"], label="rust-version").stdout.decode().strip()
        return dict(super().identity(), rust_version=version, test_working_directory="cargo-package-root/v1",
                    packages=[{"name": p["name"], "manifest_path": p["manifest_path"]} for p in metadata["packages"]])

    def execution_directories(self, artifacts, metadata):
        packages = {item["id"]: item for item in metadata["packages"]}
        if len(packages) != len(metadata["packages"]):
            raise ValueError("Cargo metadata repeats a package identity")
        directories = []
        for artifact in artifacts:
            package = packages.get(artifact["package_id"])
            if package is None:
                raise ValueError("Cargo test artifact has no matching package")
            manifest = Path(package["manifest_path"]).resolve(strict=True)
            if (not manifest.is_relative_to(self.root) or not manifest.is_file()
                    or manifest.name != "Cargo.toml"
                    or Path(artifact["manifest_path"]).resolve(strict=True) != manifest):
                raise ValueError("Cargo test package manifest is outside the checkout or mismatched")
            directories.append(manifest.parent)
        return directories

    def test(self, targeted, output):
        package = self.task.metadata.get("cargo_package", self.options.get("package"))
        args = ["cargo", "test", "--locked", "--no-run", "--message-format=json"]
        if package:
            args += ["-p", package]
        features = self.options.get("features", [])
        if features:
            args += ["--features", ",".join(features)]
        if targeted:
            for target in self.task.metadata.get("cargo_tests", []):
                args += ["--test", target]
        completed = self.command(args, label="cargo-build", check=False, timeout=1800)
        managed_processes.save_output_reference(completed, output / "cargo-build.jsonl")
        if completed.returncode:
            return reports.report({}, engine="cargo", exit_code=1, errors=[{"nodeid": "<cargo-build>", "phase": "build"}]), managed_processes.output_summary(completed).decode(errors="replace")
        artifacts = []
        for line in completed.stdout_lines() if isinstance(completed, managed_processes.CommandResult) else completed.stdout.splitlines():
            value = json.loads(line)
            if value.get("reason") == "compiler-artifact" and value.get("profile", {}).get("test") and value.get("executable"):
                artifacts.append(value)
        directories = self.execution_directories(artifacts, self.metadata())
        (output / "cargo-execution.json").write_text(json.dumps({
            "schema": "agentgo.swe-cargo-execution/v1", "working_directory_semantics": "cargo-package-root/v1",
            "artifacts": [{"package_id": artifact["package_id"], "manifest_path": artifact["manifest_path"],
                "executable": artifact["executable"], "target": artifact["target"]["name"], "cwd": str(directory)}
                for artifact, directory in zip(artifacts, directories, strict=True)]}, indent=2) + "\n", encoding="utf-8")
        items, logs = [], []
        for index, (artifact, directory) in enumerate(zip(artifacts, directories, strict=True)):
            binary = artifact["executable"]
            listed = self.command([binary, "--list"], cwd=directory, label="rust-list")
            names = reports.libtest_inventory(listed.stdout.decode())
            if not names:
                continue
            ran = self.command([binary, "--test-threads=1", "--format=pretty"], cwd=directory, label="rust-test", check=False)
            source = managed_processes.stdout_source(ran)
            text = source.read_text(encoding="utf-8", errors="replace") if isinstance(source, Path) else source.decode("utf-8", errors="replace")
            managed_processes.save_output_reference(ran, output / f"libtest-{index}.log")
            prefix = artifact["target"]["name"]
            items.append(reports.libtest(text, names, prefix, ran.returncode))
            logs.append(text[-65536:])
        if not items:
            raise ValueError("Cargo produced no test-bearing executable")
        return reports.merge(items, "cargo"), "\n".join(logs)[-65536:]


class CTestDriver(Driver):
    def prepare_existing(self):
        self.cmake_build = self.build / "cmake"
        self.sanitizer = self.options.get("sanitizer")
        self.cmake_definitions = {}
        if self.sanitizer is None:
            return
        if self.sanitizer != "address":
            raise ValueError("unsupported CTest sanitizer")
        # A memory-safety task must retain instrumentation in qualification,
        # model commands, and final judging. Never silently use a plain build.
        compiler = shutil.which("cl.exe" if os.name == "nt" else "c++", path=self.env.get("PATH"))
        if not compiler:
            raise RuntimeError("CTest AddressSanitizer requires a native compiler (MSVC developer environment on Windows)")
        self.sanitizer_compiler = Path(compiler).resolve(strict=True)
        flags = "/EHsc /fsanitize=address" if os.name == "nt" else "-fsanitize=address -fno-omit-frame-pointer"
        linker_flags = "/INCREMENTAL:NO" if os.name == "nt" else "-fsanitize=address"
        self.env.update(CXX=str(self.sanitizer_compiler), CXXFLAGS=flags, LDFLAGS=linker_flags,
                        ASAN_OPTIONS="halt_on_error=1:abort_on_error=0:exitcode=1")
        self.cmake_definitions.update(CMAKE_CXX_COMPILER=self.sanitizer_compiler.as_posix(),
                                      CMAKE_CXX_FLAGS=flags, CMAKE_EXE_LINKER_FLAGS=linker_flags,
                                      CMAKE_SHARED_LINKER_FLAGS=linker_flags)
        if os.name == "nt":
            # CMake's default MSVC Debug /RTC1 and incremental linker conflict
            # with AddressSanitizer. Keep debug symbols and no optimization.
            self.cmake_definitions.update(CMAKE_CXX_FLAGS_DEBUG="/Zi /Od",
                                          CMAKE_EXE_LINKER_FLAGS_DEBUG="/DEBUG /INCREMENTAL:NO",
                                          CMAKE_SHARED_LINKER_FLAGS_DEBUG="/DEBUG /INCREMENTAL:NO")
            for name in ("CL", "_CL_", "LINK", "_LINK_"):
                self.env.pop(name, None)

    def prepare(self):
        self.prepare_existing()
        self.identity()

    def identity(self):
        version = self.command(["cmake", "--version"], label="cmake-version").stdout.decode().splitlines()[0]
        value = dict(super().identity(), cmake_version=version)
        if self.sanitizer:
            banner = self.command([self.sanitizer_compiler, "/Bv" if os.name == "nt" else "--version"],
                                  label="sanitizer-compiler-version", check=False)
            value.update(sanitizer=self.sanitizer, sanitizer_compiler=str(self.sanitizer_compiler),
                         compiler_version=(banner.stdout + banner.stderr).decode(errors="replace").strip(),
                         cmake_definitions=self.cmake_definitions, asan_options=self.env["ASAN_OPTIONS"])
        return value

    def test(self, targeted, output):
        args = ["cmake", "-S", self.root, "-B", self.cmake_build, "-G", "Ninja", "-DFMT_TEST=ON", "-DFMT_DOC=OFF", "-DCMAKE_BUILD_TYPE=Debug"]
        args += ["-D" + name + "=" + value for name, value in self.cmake_definitions.items()]
        configured = self.command(args, label="cmake-configure", check=False)
        if configured.returncode:
            raise ValueError("CMake environment configuration failed")
        attempt = resource_storage.active()
        built = self.command(["cmake", "--build", self.cmake_build, "--parallel", str(attempt.policy.max_build_jobs if attempt else 4)], label="cmake-build", check=False, timeout=1800)
        if built.returncode:
            return reports.report({}, engine="ctest", exit_code=1, errors=[{"nodeid": "<cmake-build>", "phase": "build"}]), managed_processes.output_summary(built).decode(errors="replace")
        cache = (self.cmake_build / "CMakeCache.txt").read_text(encoding="utf-8")
        origin = re.search(r"(?m)^CMAKE_HOME_DIRECTORY:INTERNAL=(.+)$", cache)
        if not origin or Path(origin.group(1)).resolve() != self.root:
            raise ValueError("CMake test binary was built from another checkout")
        attempt = resource_storage.active()
        firewall = getattr(attempt.storage, "firewall", None) if attempt else None
        firewall_programs = []
        if os.name == "nt" and firewall:
            catalog = self.command(["ctest", "--test-dir", self.cmake_build, "--show-only=json-v1"], label="ctest-network-inventory")
            for test in json.loads(catalog.stdout).get("tests", []):
                if targeted and test.get('name') not in self.task.metadata['ctest_targets']:
                    continue
                command = test.get("command", [])
                if command and Path(command[0]).is_file():
                    firewall_programs.append(command[0])
        path = output / "ctest.xml"
        args = ["ctest", "--test-dir", self.cmake_build, "--output-on-failure", "--output-junit", path]
        if targeted:
            args += ["-R", "^(" + "|".join(re.escape(name) for name in self.task.metadata["ctest_targets"]) + ")$"]
        ran = self.command(args, label="ctest", check=False, firewall_programs=firewall_programs)
        if not path.exists():
            raise ValueError("CTest did not produce its JUnit report")
        return reports.junit(path, engine="ctest", exit_code=int(ran.returncode != 0)), managed_processes.output_summary(ran).decode(errors="replace")


DRIVERS = {"go": GoDriver, "pytest": PytestDriver, "vitest": VitestDriver, "cargo": CargoDriver, "ctest": CTestDriver}


def make(config, task, root, *, existing=False):
    driver = DRIVERS[config.suite.options["driver"]](config, task, root)
    if existing:
        driver.prepare_existing()
    return driver
