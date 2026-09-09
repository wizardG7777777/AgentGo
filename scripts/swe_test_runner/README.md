# AgentGo SWE Test Runner

本目录是外部 Flask SWE 测试程序的唯一入口，负责考题准备、AgentGo 进程管理、正式 pytest 判题和批次汇总。它不属于 L3 Harness Engineering。工具重建的适配计划与阶段证据见 [工具契约第 12–13 章](../../docs/design/tool-taxonomy-and-contracts.md)。本地迁移与验证已完成，真实批测按用户要求暂缓。

## 启动条件

所有公开 CLI 入口先一次性校验以下四项非空环境变量；缺项时在网络、文件写入和子进程启动之前退出，诊断只列变量名：

- `SWE_API_KEY`：provider 密钥，仅从进程环境读取。
- `SWE_BASE_URL`：OpenAI-compatible API 基础地址。
- `SWE_FAST_MODEL`：快速能力档位。
- `SWE_FLAG_SHIP_MODEL`：旗舰能力档位。

两个模型可以相同，通用能力探针按实际模型去重。旧 `SWE_MODEL`、`SWE_BASE_MODEL`、`SWE_WORKER_MODEL` 不作为 fallback。角色模型由 [setting.swe-flask.yaml](../../setting.swe-flask.yaml) 决定：Scheduler/Verifier 使用快速档，Explorer/Worker 使用旗舰档。已删除独立 Observation 模型配置及多版本 Observation 探针。

需要 Git、uv、Python 3.13、已构建的 AgentGo，以及包含目标 fix commit 的完整 Flask Git 仓库。公开入口自行将 stdout/stderr 配置为 UTF-8；Windows 不依赖调用者设置代码页或 PYTHONUTF8。

可选变量：

| 变量 | 默认值或含义 |
|---|---|
| SWE_PROTOCOL | responses；也可明确选择 chat_completions，两者均只使用 SSE |
| SWE_AGENTGO_ROOT | 从 runner.py 推导仓库根 |
| SWE_AGENTGO_BIN | 仓库根的 agentgo.exe（Windows）或 agentgo（macOS/Linux） |
| SWE_TESTBED | 当前用户的平台数据目录下 AgentGo/swe 或 agentgo/swe |
| SWE_SUITE_DIR | scripts/swe_test_runner/suites/flask-8 |
| SWE_TASKS_FILE | suite 下的 tasks.csv |
| SWE_PROMPT_DIR | suite 下的 prompts |
| SWE_FLASK_REPO | testbed/upstream/flask |

testbed 默认目录：Windows `%LOCALAPPDATA%/AgentGo/swe`（缺失时使用 USERPROFILE）；macOS `~/Library/Application Support/AgentGo/swe`；Linux `$XDG_DATA_HOME/agentgo/swe`（缺失时使用 `~/.local/share/agentgo/swe`）。不硬编码用户目录。正式八题 manifest、题目 prompt 与 suite.json 随仓库提供；testbed 只保存源仓库、临时 worktree 和运行产物。

## 完整事务命令

在仓库根运行。以下是后续使用命令，本次改造期间尚未执行 SWE 探针、任务或批次。

```powershell
go build -o .\agentgo.exe .
py -3.13 scripts/swe_test_runner/runner.py probe
py -3.13 scripts/swe_test_runner/runner.py task automatic-options --timeout 1200
py -3.13 scripts/swe_test_runner/runner.py batch --timeout 1200
py -3.13 scripts/swe_test_runner/runner.py verify-candidates
```

macOS/Linux 使用 `go build -o ./agentgo .` 和 `python3` 替换对应命令。四项 provider 环境变量必须已在当前进程中设置。密钥不要写入命令行参数或版本控制的 YAML。

- `probe`：逐模型验证所选协议的 SSE 完整结束、typed function call、非空且唯一的 call_id 和正确 nonce。文本声称调用了工具不能通过；不再启动 `agentgo probe observation`。
- `task`：通用探针 → 准备红态与基线 → 启动 AgentGo → 收集新运行事实 → 独立 Judge → 汇总。
- `batch`：按 tasks.csv 串行执行完整单题事务，每题后和 finally 原子更新本批次汇总。
- `verify-candidates`：干净基线 → golden tests 红态 → golden source fix 绿态，验证题目本身有效。golden source fix 不交给修复 Agent。

`--timeout` 只限定外部评测进程运行时间；不换算成 Agent 的轮数、阶段预留、token/cost 上限或强制 Recovery。旧 240/480 秒下限已删除。HTTP 与单条 Shell 的传输/进程超时仍是各自配置的执行限制。

## 输入与工具边界

Python 使用 `agentgo.run-contract/v3` 创建 Run 身份和 `swe/v4` 记账标签，不注入 CheckContract、check_id、exact_command 或阶段 reserve。Agent 可以用 run_shell 自行运行 pytest 调查；AgentGo 只保存通用命令执行事实。正式“测了什么、测的是哪份代码、测试是否通过”由 Python 判定。

Worker 使用 run_shell/read_file/apply_change 及结果、检视、通信工具；Explorer 使用读取、检视与 Shell 调查，不承担文件修改；Verifier 读取上游结果和证据并提交结论，无 Shell 或文件写入权限。未配置独立 Web 工具不能证明无网络，Shell 的网络能力必须由实际执行环境约束。

基线失败日志仍被归一、去 ANSI 并裁剪为最多 6000 字符的 `<swe-baseline-failure authority="swe-test-runner">`，只加入本次用户输入，不改写题目 prompt，不形成 Observation 或决策工具前置条件。题目红态不是修改 tests/ 的授权。

## 正式判题记录

run_pytest 使用实际虚拟环境 Python 和 argv 测试路径，保留 pytest 原文、JUnit 与 `agentgo.pytest-phase-report/v2` sidecar。插件按 nodeid 和阶段记录失败，允许 call failure 与 teardown error 重叠；不从 JUnit 总数相减猜 passed。

每次正式 pytest 另保存 `agentgo.swe-test-execution/v1`：

- 题目、Run（准备/候选自检阶段为空）、阶段、测试执行身份；
- Git 基线提交、argv、cwd、Python 路径、时间与退出状态；
- 测试前后源码/测试/资源/配置的文件集合与实际字节摘要，包括未提交增删；
- 实际 Flask 导入路径、Python/依赖环境、收集 nodeid；
- 完成、被中断或无法可靠归属的状态。

正式测试要求 Flask 从被测 `src/flask` 导入；测试期间输入发生变化时拒绝归属结果。测试保护基线 v2 覆盖测试集合及 pytest 配置的增删/修改。Judge 对比具体失败项与阶段；失败总数相同不代表没有新增破坏。无测试收集、计数冲突、错误 schema 和缺失报告不能通过。

## 运行事实与结果

`runtime_audit.py` 读取新版运行记录，不根据旧轮次阈值或固定工具序列判分。工具请求、Registry 派发、Shell 实际执行、模型输出和 Python pytest 是不同事实。非零 Shell 退出本身不等于架构错误；取消、超时和启动失败不伪造 exit=0。

新目录位于 `.agentgo/state`：`context-snapshots-v2`、`task-outcomes-v2`、`run-usage-v2`、`loop-facts-v2`、`graph-authoring-v2`、`graphs-v5`、`deliveries-v2`。Session 模型完整输出仍在各会话 turns.jsonl。目录版本与内部记录 schema 分别校验，不向旧目录 fallback。

Context 本体没有 RunID，通过本 Run 的 InvocationID 关联。工具按 Run/Task/Attempt/Invocation/CallID 对账；重复相同记录不增加计数，冲突、缺失及损坏记录报告为证据问题。未知值不当作有效的零值，截断日志保留可读前缀且不修改原文件。

`result.json` 使用 `agentgo.swe-result/v4`；`judge.json` 使用 `agentgo.swe-judge/v2`。分别保留 Graph/Delivery 结果、执行是否收口、架构检查、provider 兼容性、基础设施状态和 pytest verdict。`architecture_ok=null` 表示当前证据或执行状态不足以完成审计，不能显示为架构通过。最终 resolved 还要求真实测试身份、有补丁、无测试篡改、Graph success 和执行收口。

退出码：0 表示通过；1 为基础设施/结果证据问题或批次未完整执行；2 为已判定的运行契约失败；3 为未修复或执行未完成；4 为 provider 工具/协议兼容性失败。保留具体 reason code 和各项结果，不能只展示一个总分。

## 进程、批次与跨平台约束

monitor 持有 Popen 并用 poll 查询生命周期。Graph terminal 后继续等待当前 Run 的在途任务和 final-report，不能仅因一段时间没有活动便终止。外部超时保留最后快照与未完成事实，再终止并回收进程；不伪造 Observation/no-progress 失败。

每题用跨平台独占锁保护 worktree 与运行产物。batch summary 只消费 `.batch_start` 标记后的当前事务，区分 completed、completed_with_infrastructure_error、infrastructure_error 和 not_run，旧结果不能补齐本批次覆盖率。provider 配额耗尽保留 provider_quota_exhausted 并停止批次。

Windows 路径占位符统一使用正斜杠；只转换 Path，不改写 URL/model/token。清理 disposable worktree 时仅对 Windows ReadOnly PermissionError 清除属性并重试一次，不吞掉占用或其它 I/O 错误。所有长生命周期文件与进程句柄在临时目录清理前关闭。

## 后续验证

离线 Python 回归位于 `*_test.py`；通用双协议二进制冒烟位于 `../local_fake_provider_smoke.py`，不使用 Flask 或真实 provider。CI 保留 Windows/Linux/macOS。Python 离线单元测试与 Windows/Linux 本地二进制已通过；它们与真实 SWE 是不同证据，验证明细以工具契约第 13 章为准。
