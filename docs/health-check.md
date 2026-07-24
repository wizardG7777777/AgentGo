# 项目健康检查报告

> 生成时间：2026-07-21 (UTC)

---

## 1. `go build ./...`

- **命令**: `go build ./...`
- **Exit Code**: `0`
- **状态**: ✅ 通过
- **输出**: （无输出，编译成功）

---

## 2. `go vet ./...`

- **命令**: `go vet ./...`
- **Exit Code**: `0`
- **状态**: ✅ 通过
- **输出**: （无输出，静态分析无警告）

---

## 3. `git status`

- **命令**: `git status`
- **Exit Code**: `0`
- **状态**: ⚠️ 有未提交变更

### 分支状态

- 当前分支: `main`
- 领先 `origin/main` 1 个 commit

### 已暂存变更 (Changes to be committed)

| 类型 | 文件 |
|------|------|
| 修改 | `AGENTS.md` |
| 修改 | `Archtechture.md` |
| 修改 | `README.md` |
| 修改 | `docs/activate/KNOWN_ISSUES.md` |
| 新增 | `docs/archived/acceptance-fact-verification-and-cascade-incident-2026-07-20.md` |
| 新增 | `docs/archived/artifact-path-normalization-incident-2026-07-21.md` |
| 新增 | `docs/health-check.md` |
| 修改 | `internal/agent/agent.go` |
| 修改 | `internal/agent/compress_test.go` |
| 修改 | `internal/agenttemplate/prompts/verifier.md` |
| 修改 | `internal/bootstrap/bootstrap.go` |
| 修改 | `internal/bootstrap/plan_runtime.go` |
| 修改 | `internal/bootstrap/plan_runtime_test.go` |
| 新增 | `internal/bootstrap/ui_scheduler.go` |
| 新增 | `internal/bootstrap/ui_scheduler_test.go` |
| 修改 | `internal/hook/builtin/record_artifact.go` |
| 修改 | `internal/hook/builtin/record_artifact_test.go` |
| 修改 | `internal/plan/acceptance.go` |
| 新增 | `internal/plan/acceptance_circuit_test.go` |
| 新增 | `internal/plan/acceptance_fingerprint.go` |
| 新增 | `internal/plan/acceptance_fingerprint_test.go` |
| 修改 | `internal/plan/coordinator.go` |
| 修改 | `internal/plan/coordinator_test.go` |
| 修改 | `internal/plan/errors.go` |
| 修改 | `internal/plan/mutation_batch_test.go` |
| 新增 | `internal/plan/runtime_summary.go` |
| 新增 | `internal/plan/runtime_summary_test.go` |
| 新增 | `internal/plan/wake_gate_test.go` |
| 修改 | `internal/reactor/builtin/record_artifact.go` |
| 修改 | `internal/reactor/builtin/record_artifact_test.go` |
| 修改 | `internal/scheduler/activator.go` |
| 新增 | `internal/scheduler/activator_watchdog_test.go` |
| 修改 | `internal/scheduler/scheduler.go` |
| 修改 | `internal/scheduler/scheduler_test.go` |
| 修改 | `internal/scheduler/snapshot.go` |
| 新增 | `internal/scheduler/snapshot_result_refs_test.go` |
| 修改 | `internal/session/snapshot_test.go` |
| 修改 | `internal/shell/intercept.go` |
| 修改 | `internal/shell/intercept_test.go` |
| 修改 | `internal/store/export_import_test.go` |
| 新增 | `internal/store/task_visibility.go` |
| 新增 | `internal/store/task_visibility_test.go` |
| 修改 | `internal/team/manager.go` |
| 修改 | `internal/team/manager_test.go` |
| 新增 | `internal/tools/crlf.go` |
| 新增 | `internal/tools/crlf_test.go` |
| 修改 | `internal/tools/known_tools.go` |
| 修改 | `internal/tools/known_tools_test.go` |
| 修改 | `internal/tools/local_read.go` |
| 修改 | `internal/tools/local_read_test.go` |
| 修改 | `internal/tools/local_write.go` |
| 修改 | `internal/tools/plan_control.go` |
| 修改 | `internal/tools/scheduler.go` |
| 新增 | `internal/tools/scheduler_result_test.go` |
| 修改 | `internal/tools/scheduler_test.go` |
| 修改 | `internal/tools/shell.go` |
| 修改 | `internal/tools/shell_test.go` |
| 修改 | `internal/tui/app.go` |
| 修改 | `internal/tui/app_test.go` |
| 修改 | `internal/tui/feed.go` |
| 修改 | `internal/tui/feed_test.go` |
| 修改 | `internal/tui/keymap.go` |
| 新增 | `internal/tui/paste_test.go` |
| 修改 | `internal/tui/suggest_test.go` |
| 修改 | `internal/ui/trace.go` |
| 修改 | `internal/ui/trace_test.go` |
| 修改 | `internal/ui/types.go` |
| 修改 | `internal/watchdog/watchdog.go` |
| 新增 | `internal/watchdog/watchdog_acceptance_cascade_test.go` |
| 修改 | `prompts/program_verifier.md` |
| 修改 | `prompts/worker.md` |
| 新增 | `test-prompt-acceptance-evidence.md` |
| 新增 | `test-prompt-parallel-investigation.md` |
| 新增 | `test-prompt-team-recovery.md` |
| 新增 | `test_findstr.txt` |

### 未暂存变更 (Changes not staged for commit)

| 文件 |
|------|
| `docs/activate/KNOWN_ISSUES.md` |
| `docs/health-check.md` |
| `internal/agenttemplate/prompts/verifier.md` |
| `internal/plan/acceptance.go` |
| `internal/plan/acceptance_circuit_test.go` |
| `internal/plan/acceptance_fingerprint.go` |
| `internal/plan/acceptance_fingerprint_test.go` |
| `internal/scheduler/scheduler.go` |
| `test-prompt-acceptance-evidence.md` |
| `test-prompt-parallel-investigation.md` |

### 未跟踪文件 (Untracked)

- `docs/archived/acceptance-spin-and-env-mutation-incident-2026-07-21.md`

---

## 综合结论

| 检查项 | 结果 |
|--------|------|
| `go build ./...` | ✅ 编译通过 |
| `go vet ./...`   | ✅ 无警告 |
| `git status`     | ⚠️ 有未提交变更（10 个未暂存 + 1 个未跟踪文件） |

go build 和 go vet 均无错误，项目编译与静态分析健康。git 工作区有未提交改动，属正常开发状态。
