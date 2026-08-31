# Recovery EvidenceContract v4：上下文覆盖与修改决策解耦

> 日期：2026-08-30
> 状态：实现与本地端到端验证完成；相同小模型 Flask-8 复测待用户执行
> 范围：SWE-107～SWE-110

## 1. 触发事实

最新用户批次为 `architecture_ok=8/8`、`task_resolved=6/8`。未解决的
`pass-context-dispatch` 与 `session-access-tracking` 均在 Graph/Recovery/最终 Judge
正常收口后以零补丁结束，说明主要瓶颈已从“架构链断裂”缩小为“模型没有形成正确修改”。

对 v3 的复盘发现一个架构性放大器：Recovery Controller 只能指定一个
`first_action=read_file(path)`，L3 在该次读取后无条件要求同路径 `edit_file`，再运行
targeted check。这个状态机只证明“读过一个片段”，没有证明完整文件、外置正文或相关
调用点已经进入可用上下文；同时它把框架的进展要求误写成“Worker 必须编辑”。

## 2. 五层归因

- **L1 Prompt**：应解释证据覆盖与 typed decision，不能鼓励为了进展制造补丁。
- **L2 Context**：文件正文可能分页或以 ContentRef 外置，必须有可审计的连续覆盖事实。
- **L3 Harness**：负责证明最低证据条件、限制当前动作和路径边界，不负责判定业务方案正确。
- **L4 Loop**：只有真实 mutation 加新鲜 CheckRecord 才算完成；check 失败开启新一轮证据覆盖。
- **L5 Graph**：Recovery 只冻结最小证据合同与策略，不替 Worker 编造修改内容。

## 3. 修复

### SWE-107 / L5：RecoveryDelta v4

新增 `agentgo.recovery-delta/v4` 与 `EvidenceContract.files`。新 simple code-change Graph
使用 v4；Controller 可声明最多八个项目相对文件，省略时 framework 以
`first_action.path` 建立最小合同。v1-v3 保持冻结恢复语义，不静默迁移。

### SWE-108 / L2+L3：Evidence Coverage Ledger

L3 从原始 History 机械重建覆盖，不信模型自述：`read_file` 按固定行段连续读取；结果
外置时按冻结 `ref_id/offset/limit` 连续调用 `read_content_ref` 直到 EOF。同一证据文件
最后一次成功 mutation 之前的读取视为 stale；check 失败后下一周期必须重新覆盖。

### SWE-109 / L3+L4：typed ChangeDecision

完整覆盖后普通业务工具全部隐藏，只开放 `submit_change_decision`：

- `edit`：提交有序 `{tool, path}` 步骤；L3 逐步开放 `edit_file`/`write_file`，完成后运行冻结 CheckContract；
- `need_context`：追加一个有因果理由的新证据文件并返回覆盖阶段；
- `hypothesis_rejected` / `blocked`：结构化 blocked，安全交回 L5，而不是制造补丁。

若冻结的 `read_file` 或 `read_content_ref` 已明确失败，L3 进入
`evidence_unavailable`，只保留 `hypothesis_rejected`/`blocked`；这避免不存在文件
造成无限重读，也不以“可恢复”为名放宽到无证据 edit。

EvidenceContract 限定判断依据，不限定修改目标。`write_file` 可声明尚不存在的新文件；
所有路径仍由 ProjectRoot canonical boundary 拒绝逃逸。

### SWE-110 / 装配与观测

`submit_change_decision` 作为 framework control tool 自动注册，但只进入 v4 recovery work
Lease；普通业务轮、acceptance、未知角色与历史 v3 不扩权。Trace 增加 ContentRef 的
`ref_id/offset/limit` 对账；SWE Test Runner 增加 v4 evidence→decision→mutation→check
序列回归。本地冒烟脚本在 Windows 默认选择 `agentgo.exe`，避免误执行仓库遗留的无扩展名
Linux 二进制。

## 4. 验证

- `go test ./internal/tools ./internal/agent ./internal/graph`：通过。
- `go test ./...`：全部 Go 包通过。
- `go vet ./...`：通过。
- `py -3.13 -X utf8 -m unittest scripts/swe_test_runner/runner_test.py`：65 项通过。
- `py -3.13 -X utf8 -m py_compile ...` 与 `git diff --check`：通过。
- `py -3.13 -X utf8 scripts/local_fake_provider_smoke.py`：真实 Windows 二进制通过；
  Graph `completed/success`，Observation 2 条，CheckRecord 1 条，Delivery `committed`，
  主根新增 artifact 断言通过，Run Budget reservation 全部结算。
- `go build -o .\agentgo.exe .`：通过。
- `go test -race ./...`：当前 Windows Go 环境 `CGO_ENABLED=0` 且无 gcc/clang，
  race instrumentation 无法启动；这是验证环境限制，不记为代码通过。

## 5. 尚未证明

本次没有调用真实 provider，也没有重跑 Flask-8。v4 已证明的是机械链路不再强制盲改，
不是两个剩余 Flask 问题已经被模型解决。相同模型复测后再比较：是否主动
`need_context`、EvidenceContract 覆盖文件数、typed edit plan、targeted check 结果和
最终 Judge；这些才是判断架构收益与模型先天能力上限的证据。
