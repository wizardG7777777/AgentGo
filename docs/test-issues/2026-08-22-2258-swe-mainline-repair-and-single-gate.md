# SWE 五层主链修复实施与单题门禁 — 2026-08-22 22:58

> 状态：IMPLEMENTATION LANDED / REPOSITORY VERIFIED / SINGLE TASK ARCHITECTURE PASS / TASK FAILED<br>
> 本文只保存脱敏计数与契约结论；API key、Flask 源码、完整 reasoning、工具参数和原始日志继续留在 `/tmp/agentgo-swe`。<br>
> 关联路线图：[`SWE 架构修复统一实施路线图`](../design/swe-architecture-repair-roadmap.md)

## 1. 最终门禁结论

按批准顺序先运行 `automatic-options`。最新可归档 Run 的双指标为：

```text
architecture_ok = true
task_resolved   = false
judge_verdict   = failed
graph_outcome   = blocked（typed）
external_hard_kill = false
```

因此本轮**没有运行 8 题批测**。这符合停止规则：单题必须同时满足架构门槛和
任务 resolved，才能进入一批 8 题。

最新 Run 的脱敏关键计数：

| 指标 | 值 |
|---|---:|
| 首轮 Scheduler prompt | 2,463 tokens |
| 首个 GraphDraft 调用序号 | 1 |
| model calls | 12 |
| prompt / completion tokens | 115,841 / 44,791 |
| Graph Definition revision / activation | 1 / 2 |
| Graph task Outcome ACK pending | 0 |
| patch lines | 0 |
| known architecture incidents | 0 |

架构侧通过项包括真实启动 function-call probe、RunID 可见、首轮 Prompt ≤8K、
GraphDraft 五次调用内、无外部 hard kill、Graph typed terminal、Graph activation
tasks 全终态、TaskOutcome commit 完整且 delivery 全 ACK。任务侧失败是 Worker 在
code-change 探索额度内没有产生 edit/write，最终按冻结 ProgressContract 诚实
blocked；Judge 红态与 baseline 一致。

## 2. 本轮新增问题编号与归层

| ID | 层级 | 实施状态 | 问题与正式处置 |
|---|---|---|---|
| SWE-020 | 外部 Harness / L3 观测边界 | landed | `/tmp` 脚本是唯一契约、文本探针、quiet 终态和成功折叠；改为仓库内 versioned harness、真实 tool probe、RunContract、typed terminal 与双指标 |
| SWE-021 | L2 Context | landed | runtime board 被误当 mailbox、可丢快照以 RequiredExact 进入 Context；改为 typed RuntimeSnapshot、8KiB bounded board、optional drop 与安全诊断 |
| SWE-022 | L2 Context + Invocation | landed | v3 tokenizer、RequiredExact replay、16K completion 与 128KiB optional reasoning 边界连续成为瓶颈；冻结历史 v1–v6，新 Run 使用 Context v7 |
| SWE-023 | L4 Loop | landed | Run 总预算误用 no-progress budget、Attempts=2 抢先耗尽、Invocation failure 被计为 Agent 空转；分离总预算，SWE 6 Attempts，failure 只扣 Run/Attempt 而暂停 no-progress 三轴 |
| SWE-024 | L5 Graph authoring | landed | 模型必须填写底层 Graph AST 并搬运 proposal/revision/report/digest，正常回复也易构图失败；新增 simple-task compiler 与 current-transaction 零参数 validate/commit/start |
| SWE-025 | L5 Proposal Acceptance | landed | 自由文本 JSON 与宽 `issues[]` schema 反复产生前后缀、未闭合参数和类型错误；改为 exact typed verdict tool，最终 wire 只含 verdict/issue_code/message 三个标量 |
| SWE-026 | L4→L5 intervention | landed | Graph 节点 intervention wake 与非图新请求共用恢复分支，曾错误重开 Draft；Task 冻结独立 Intervention Graph/Node/Activation scope，Scheduler 机械路由到 Graph recovery |

这些编号记录本轮新发现的跨层残余，不改写 SWE-011～019 的历史含义。旧问题仍按
各自 closure matrix 管理；一次单题架构通过不足以替代约定的 8 题证据。

## 3. 主要实现事实

### 3.1 仓库内权威 Harness

通用契约落在 `scripts/swe_harness/harness.py`；2026-08-23 起 prepare/run/judge
只作为 `task` / `batch` 的内部固定阶段，正式入口收敛为 Python 子命令，仓库与
`/tmp/agentgo-swe` 均不再保留 Bash 测试脚本或包装器。

- preflight 与每实例 startup probe 都发送真实 function schema；工具名随机化，
  只接受唯一 exact tool call、空参数和 `finish_reason=tool_calls`；
- startup probe 在同一 45 秒总 deadline 内只对 typed 瞬时/截断失败有界重采样，
  文本、错误工具名和错误参数立即 fail-closed；
- `/api/input.run_contract` 注入 `agentgo.run-contract/v1`、唯一 RunID、`swe/v1`、
  1140 秒内部 deadline、90 秒 recovery reserve 和30秒 finalization reserve；
- 删除 quiet 成功判断，分别记录 process、Graph lifecycle、typed outcome、
  Graph TaskOutcome/ACK、Judge 与 hard kill；
- Dashboard snapshot 只新增 Run/Attempt/Outcome/GraphNodeKind 身份，不暴露结果正文、
  reasoning、工具参数或原始错误正文；
- `architecture_ok` 与 `task_resolved` 完全分离。Graph final-report/intervention 等
  非 Graph 控制任务不属于 activation TaskOutcome barrier。

### 3.2 Simple Graph 与 current transaction

新请求的简单图主链为：

```text
create_graph_draft({})
→ configure_simple_graph_draft({execution_class})
→ validate_current_graph_draft({})
→ commit_current_graph_draft({})
→ start_current_graph({})
```

framework 生成 work Agent、独立 acceptance、单赋值输入端口、typed ends、
OutputContract、Context/Progress refs 与 GraphContract bindings。模型只判断
`answer/read_only/mutating`；请求要求修改文件、代码、配置、实现或修复测试时必须
是 mutating。proposal/revision/report/digest/StartIntent 均从 task/session 绑定的
durable authority 解析，禁止模型搬运。复杂拓扑仍保留通用 CAS patch 工具。

### 3.3 Context v7 与 Loop

- v1–v3 保留历史 tokenizer/预算语义；v4 修正 mixed estimator；v5 扩大
  RequiredExact reasoning；v6 将 completion reserve 提到32K；v7 保持128K总窗口
  分配并放宽 optional reasoning 字节容器；
- Snapshot input 92K、completion 32K、protocol overhead 4K，仍满足128K窗口；
- `swe/v1` Run 总预算为19分钟、64 model calls、400K completion、6 Attempts；
  `task_max_retries=5` 与其对齐；
- output_truncated/malformed 等 Invocation failure 形成
  `ProgressInvocationFailure`：累计 Run usage、触发 Attempt recovery，但不增加
  no-progress turns/duration/usage；
- Graph intervention wake 冻结独立控制 scope，wake 自身仍保持 GraphID 为空，
  不会被 terminal feed 误认成 activation Task。

## 4. 验证证据

本轮最终代码状态已通过：

- `go test ./...`；
- `go test -race ./...`（默认沙箱禁止 `httptest` bind，已在批准的非沙箱环境运行）；
- `go vet ./...`；
- `go build -o agentgo .`；
- harness Python 单测 13 项；
- `git diff --check`；
- 真实二进制 startup function-call probe、Dashboard/Run 注入、durable
  GraphDraft/Definition/StartIntent/Graph outcome/TaskOutcome ACK。

`AGENTS.md` 未修改。

## 5. 仍开放

1. `automatic-options` 尚未 resolved；最新失败属于模型业务行为，不是已知
   SWE-011～019 架构事故复发。
2. 8 题批测未运行；旧问题不能据单题关闭。
3. 更多 provider 的 SSE/usage/structured tool fixture 与真实 tokenizer 仍开放。
4. L5 legacy `submit_graph` / direct `patch_graph` 按计划继续登记，不在本轮删除。
5. 三平台 CI/真实终端验证仍是发布层证据缺口。
