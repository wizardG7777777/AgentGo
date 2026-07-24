# Artifact 路径归一化缺陷导致验收马拉松事故修复记录（2026-07-21）

## 事故概述

2026-07-21 晨，一个最简健康检查 Plan（创建 `docs/health-check.md` 并验收，session `sess-268cb956`）触发 4 个接力 Plan（`7d2a19f8` → `1fe11dc3` → `d2dc52c7` → `6366a3f6`）、16 个 PlanRevision、8 轮 AcceptanceRun 才通过，累计消耗约 450 万 prompt tokens（约占整个 session 910 万的一半），并触发一次预算告警。

根因是**一行级路径归一化缺陷**，三层证据链：

1. **登记侧**：`record-artifact` Reactor 的 `normalizeArtifactPath` 直接对 `cfg.ProjectRoot` 做 `filepath.Rel`。用户配置 `project_root: "."` 是相对路径，而 `filepath.Rel` 在 base 相对 / target 绝对时直接报错（Windows 与 POSIX 行为一致），于是落入 fallback 分支，artifact 被登记成绝对路径 `C:/Users/.../docs/health-check.md`。
2. **比对侧**：`planAcceptanceVerifier.VerifyAcceptance` 对 ExpectedArtifacts（按合约为相对路径 `docs/health-check.md`）只做 `filepath.Clean` 后精确比对，无归一化、无 basename 容忍——与 agent 任务级 `checkExpectedArtifacts` 的 basename 兜底策略不一致。任务能"完成"，Plan 验收却永远 `lacks expected artifact`（extfact 指纹 `7794494a`）。
3. **放大器**：同指纹失败熔断（`acceptance_fingerprint.go`，连续 2 次熔断）的 epoch = SpecRevision + GraphDigest，Scheduler 每次失败后 replan 改图/改 Spec 使 epoch 复位，8 轮里熔断一次都没机会触发。其间 Scheduler 还尝试了 `test -s`（Unix 命令，Windows cmd 下 exit code 不符）与 Node.js `fs.writeFileSync` 绕开 write_file（文件落盘但不产生 `KindFileWritten`，无 artifact 登记）等死胡同路径。

## 修复内容

- `internal/reactor/builtin/record_artifact.go` 与 `internal/hook/builtin/record_artifact.go`（同名函数，保持字节级一致）：`normalizeArtifactPath` 在直接 `filepath.Rel` 失败时，先 `filepath.Abs(projectRoot)` 再重试。仅在直接 Rel 失败时才走 Abs 重试，词法相对可解场景（如 POSIX 风格测试夹具路径）行为字节级不变。
- `internal/bootstrap/plan_runtime.go`：新增 `canonicalArtifactKey`，`VerifyAcceptance` 的 expected-artifact 比对两侧统一规整为项目根下绝对路径再比较——即使历史数据已混入绝对路径登记的 artifact，验收也能正确通过（防御性对齐，防第二处登记路径不一致再次卡死验收）。

## 验证证据

- `internal/reactor/builtin/record_artifact_test.go`、`internal/hook/builtin/record_artifact_test.go`：各新增 `TestNormalizeArtifactPath_RelativeProjectRoot`——以 `os.Getwd()` 构造真实绝对路径 + `projectRoot="."`，断言登记为 `docs/foo.md` 相对路径。
- `internal/bootstrap/plan_runtime_test.go`：新增 `TestVerifyAcceptanceArtifactMixedPathForms`——同一任务分别以相对 / 绝对两种形态登记 artifact，`VerifyAcceptance` 均通过。
- `internal/hook/builtin` 原有 `TestRecordArtifactHook_ViaRegistry`（POSIX 风格夹具路径）保持通过，确认旧场景行为不变。
- 全量 `go vet ./...` + `go test ./...` 通过（2026-07-21，Windows）。

## 未动的部分

- 未在 bootstrap 启动时把 `cfg.ProjectRoot` 一次性 canonicalize 成绝对路径（更广的隐患面消除，但触及所有裸用点，留待单独变更并全量回归）。
- 未给 `VerifyAcceptance` 比对加 basename 兜底——精确比对是防证据伪造的防线，路径形态问题由归一化解决，不放宽匹配语义。
- artifact 登记仍完全绑定 `KindFileWritten` 事件；shell 写文件（如 Node.js 绕过路径）不留痕是设计使然，未改动。
