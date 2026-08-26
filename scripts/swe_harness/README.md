# AgentGo SWE Harness

该目录是 Flask SWE 系统回归的受版本控制契约。所有测试入口、进程编排、判题和
汇总逻辑统一由 Python CLI `harness.py` 执行；仓库及 testbed 均不得维护 Bash
测试脚本。正式八题基线位于 `suites/flask-8/`，其中的 `tasks.csv`、八份 prompt
和 `suite.json` 与 runner 一同受版本控制。testbed 只保存 Flask 源仓库、隔离
worktree 与原始运行产物，不保存密钥。

## 启动必要条件

Harness 使用 fail-closed 的 provider 环境契约。以下 3 个环境变量必填；任一变量
未设置或仅含空白时，CLI 会在网络请求、目录创建、worktree 清理和子进程启动前
一次性报告全部缺项并退出：

- `SWE_API_KEY`：外部 provider 密钥，仅从进程环境读取，诊断不得回显值；
- `SWE_BASE_URL`：OpenAI-compatible provider 基础 URL；
- `SWE_MODEL`：所有 SWE 角色统一使用的模型名。

其余变量都是可选覆盖项；未设置或为空时使用跨平台派生值：

- `SWE_PROTOCOL`：默认 `responses`；仅显式兼容旧端点时设为 `chat_completions`；
- `SWE_AGENTGO_ROOT`：默认从 `harness.py` 位置推导当前 AgentGo 仓库根；
- `SWE_AGENTGO_BIN`：默认 `<SWE_AGENTGO_ROOT>/agentgo.exe`（Windows）或
  `<SWE_AGENTGO_ROOT>/agentgo`（macOS/Linux）；
- `SWE_TESTBED`：默认使用当前用户的标准数据目录——Windows 为
  `%LOCALAPPDATA%\AgentGo\swe`（缺失时从 `%USERPROFILE%` 拼装），macOS 为
  `~/Library/Application Support/AgentGo/swe`，Linux 为
  `${XDG_DATA_HOME:-~/.local/share}/agentgo/swe`；
- `SWE_SUITE_DIR`：默认仓库内 `scripts/swe_harness/suites/flask-8`；
- `SWE_TASKS_FILE`：默认 `<SWE_SUITE_DIR>/tasks.csv`；
- `SWE_PROMPT_DIR`：默认 `<SWE_SUITE_DIR>/prompts`；
- `SWE_FLASK_REPO`：默认 `<SWE_TESTBED>/upstream/flask`。

正式八题的 manifest 与 prompt 已随仓库提供，不需要从外部 testbed 复制。执行
`task` / `batch` / `verify-candidates` 前仍须准备包含目标 fix commit 的完整 Flask
Git 仓库；仓库位于其他位置时设置 `SWE_FLASK_REPO` 即可。

`AGENTGO_SWE_PYTEST_REPORT` 与 `PYTHONPATH` 是 Harness 在 pytest 阶段自行注入的内部
变量，不属于用户启动契约。除必填 provider 环境外，机器还必须提供 Git、uv、Python 3.13、
可执行的 AgentGo 二进制、可读的题目/prompt/Flask 数据以及可访问的 provider。

生成配置使用 Context v9 默认能力档案（1M context / 64K completion）与
`swe/v3` Run profile；model/tool/token/cost 全量记账但不作为默认经验硬停止条件，
用户显式业务预算由 RunID 级 Ledger 跨 Task 执法。小窗口模型应在
`setting.swe-flask.yaml` 的 `llm.model_capabilities` 中按精确模型名覆盖。
Ledger `model_calls` 只结算实际越过 provider dispatch 的请求；任务级
ToolRouter/Context/Lease preflight 失败仅关闭 reservation。runner 会把 Ledger
调用数与 `llm_call_end` 对账，并通过非终态
`observation_checkpoint_failed(reason=control_invocation_preflight_failed)` trace
识别周期性 Control Invocation 不可用，不能只从终态 TaskOutcome 反推。

当前 SWE 全局契约固定 `reasoning_effort=low`（thinking 仍开启），双层能力探针与
Scheduler/Proposal 机械阶段使用 `tool_choice=auto` + singleton ToolRouter +
L3 required-action gate。Graph 最终交付是狭义例外：历史投影收窄后，仅该次
Invocation 覆盖为 `reasoning_effort=none` + exact `submit_task_result`；不按
provider/model 名称分支，其余业务推理仍走 thinking。

## 启动方式

前置依赖为 Go 1.25、Git、Python 3、`uv`，以及可用的 `SWE_API_KEY`。先在 AgentGo
仓库根目录构建当前二进制；POSIX 使用 `go build -o agentgo .`，Windows 使用
`go build -o agentgo.exe .`。

首次运行前准备 Flask 上游仓库。仓库位于默认 testbed 之外时，通过可选变量显式
指向它。macOS / Linux 示例：

```console
git clone https://github.com/pallets/flask.git "$HOME/src/flask"
export SWE_FLASK_REPO="$HOME/src/flask"
```

Windows PowerShell：

```powershell
$flaskRepo = Join-Path $env:LOCALAPPDATA "AgentGo\swe\upstream\flask"
New-Item -ItemType Directory -Force (Split-Path -Parent $flaskRepo) | Out-Null
git clone https://github.com/pallets/flask.git $flaskRepo
$env:SWE_FLASK_REPO = $flaskRepo
```

题目 prompt 使用跨平台的 `uv run --no-sync python -m pytest -q`。runner 会先用
`uv sync --frozen` 创建并冻结 `.venv`；`--no-sync` 确保 Agent 执行测试时不重新解析
或修改依赖环境。

完整执行一道题（能力探针 → prepare → run → judge）：

```console
python3 scripts/swe_harness/harness.py task automatic-options --timeout 1200
```

Windows PowerShell 使用 Python Launcher：

```powershell
py -3.13 -X utf8 scripts/swe_harness/harness.py task automatic-options --timeout 1200
```

执行 `tasks.csv` 中的完整批次：

```console
python3 scripts/swe_harness/harness.py batch --timeout 1200
```

Windows PowerShell：

```powershell
py -3.13 -X utf8 scripts/swe_harness/harness.py batch --timeout 1200
```

独立运行能力探针：

```console
python3 scripts/swe_harness/harness.py probe
```

Windows PowerShell：

```powershell
py -3.13 -X utf8 scripts/swe_harness/harness.py probe
```

`prepare`、`run`、`judge` 是 `task` / `batch` 内部的固定阶段，不提供独立 CLI
入口，避免绕过能力探针、终态采集或最终判题。

每道题的终端输出固定标注四个阶段，并在 pytest 阶段同时打印测试内容、测试范围、
判定目标、原始摘要以及 phase-aware 结构化计数：

```text
[第1/4阶段][目标测试红态确认]
[第2/4阶段][全量测试红态基线]
[第3/4阶段][AgentGo 修复执行]
[第4/4阶段][最终全量 Judge]
```

前两阶段出现 `FAILED` 是题目有效性的预期红态；只有第四阶段是修复后的最终测试
结论。Provider function-call 探针独立标为 `[前置检查]`，不混入四个任务阶段。

pytest 计数以同进程加载的 `agentgo_swe_pytest_reporter` 机器侧车为权威，分别写入
`targeted-baseline.pytest.json`、`baseline.pytest.json` 与 `judge.pytest.json`。
JUnit 继续保存 traceback 和测试名，但不再用 `tests - failures - errors - skipped`
推算通过数。终端字段语义为：

- `collected`：pytest 实际收集的逻辑测试数；兼容 JSON 字段 `tests` 与其相等；
- `passed` / `failed`：call 阶段结果；
- `error_events`：collection/setup/teardown 失败事件数；
- `skipped` / `xfailed` / `xpassed`：分别统计，不折叠进 passed。

这些字段采用 `pytest-phase-overlap/v1` 语义，允许同一测试同时计入 `passed` 和
`error_events`（例如 call 成功但 teardown 失败），因此各字段不得相加推导
`collected`。侧车缺失、非法或与 JUnit 的 failures/errors/skipped 冲突时，runner
按基础设施错误 fail-closed，不回退到文本或旧 JUnit 公式。

验证所有候选题目的“干净基线绿 → test patch 红 → source fix 绿”语义：

```console
python3 scripts/swe_harness/harness.py verify-candidates
```

Windows PowerShell：

```powershell
py -3.13 -X utf8 scripts/swe_harness/harness.py verify-candidates
```

运行 harness 单元测试：

```console
python3 -m unittest scripts/swe_harness/harness_test.py
```

Windows PowerShell：

```powershell
py -3.13 -X utf8 -m unittest scripts/swe_harness/harness_test.py
```

`result.json` 使用 `agentgo.swe-result/v2`，分别报告 `architecture_ok` 与
`task_resolved`；禁止用 pytest 偶然通过覆盖架构事故，也禁止用模型业务失误伪造
框架错误。CLI 退出码为：`0` 表示所选测试全部通过，`1` 表示 harness/环境故障，
`2` 表示架构门失败，`3` 表示任务正确率门失败；批次不再在未达到目标时返回成功。
批次遇到架构门失败会立即停止，普通任务正确率失败则继续完成剩余题目并在汇总后
返回 `3`。

架构门按 source Task 核验 L4→L5 恢复：任何 Graph TaskOutcome 的
`reason_code=loop_intervention_required|no_progress_budget_exhausted|observation_state_stalled`
都必须存在一个已交付 Outcome 的
`graph_controller_role=loop_recovery` Task，且其 `recovery_source_task_id` 精确指向
该 source；缺失时记录 `loop_intervention_without_recovery` 并令
`architecture_ok=false`。Recovery Controller 自身是有界裁决终点，不递归要求
下一层 recovery。

mutating Graph 还必须在 `.agentgo/state/checks/` 形成绑定最新 workspace revision
的 `verification` CheckRecord。零改动 completed、stale/missing check、RecoveryDelta
参数拒绝都会令架构门失败；普通 `run_shell` exit=0 不再证明新主链测试通过。

Recovery retry 还必须在 decision commit 前证明下一 execution Activation 可启动。
若 Graph 已选择 retry、随后才因 execution window 关闭而发布失败，runner 记录
`recovery_retry_activation_unstartable` 并令 `architecture_ok=false`。机械阶段
返回错工具属于 L3 `action_contract_rejected`，不再混入 provider
`malformed_response`。

Graph terminal 后 runner 还会等待 graph-ended final-report Task，并核验其
`final_report_graph_id`、completed 状态与 TaskOutcome commit。final-report 读取
结果发生 scope 拒绝时记录 `final_report_result_scope_failure`，不得继续计为
`architecture_ok=true`。

批次 summary 只代表当前 `.batch_start`：启动时先清空旧 summary，结束或异常时
都在 `finally` 原子重写。每个 tasks.csv 条目均有一行，`run_state` 为
`completed`、`completed_with_infrastructure_error`、`infrastructure_error` 或
`not_run`。终端按 completed 分母分别报告任务正确率和架构通过率，再独立报告
infra/not-run；不得把未执行题计成模型业务失败。AgentGo 在 healthz 前退出时，
runner 保存 `infrastructure_error.json`，包含脱敏 reason code、stage、exit code 与
日志引用。provider HTTP 402 使用 `provider_quota_exhausted`，批次以 exit 1 停止；
同一类型也适用于 batch 级直连 function-call preflight，且不会对余额、认证或权限
错误执行无意义的三次探针重试。
