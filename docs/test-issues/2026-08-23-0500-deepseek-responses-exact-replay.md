# DeepSeek Responses 精确重放事故与单题回归

> 时间：2026-08-23 05:00 AWST
> Provider/模型：DeepSeek `/responses`，`deepseek-v4-flash`
> 范围：Model Invocation、L2 Context、SWE harness

## 1. 批次现象

DeepSeek 8 题批次全部形成合法 Graph 并进入 typed failed 终态，但任务正确率为
`0/8`、补丁均为 0。八题共同出现 provider HTTP 400：

```text
input: missing field `content`
failure_kind=invalid_request
```

旧 harness 只检查 Graph/Outcome/ACK 等终态事实，没有把 provider
`invalid_request` 纳入架构事故，因此错误报告为 `architecture_ok 8/8`。

## 2. SWE-029：Responses output item 二次序列化丢失 required 空字段

### 2.1 权威原因

DeepSeek 官方 Responses 文档说明该端点是无状态 API，后续请求必须把
`message`、`function_call`、`function_call_output` 与 `reasoning` 等 input item
显式重放。AgentGo 的 L2 Replay v3 已把服务端 output item 标成
`RequiredExact`，但 Model Invocation 实际执行了：

1. 保存服务端 output item 原始 JSON；
2. 下一轮把原始 JSON 反序列化成 OpenAI Go SDK request union；
3. 再由 SDK 序列化 request union。

OpenAI Go SDK v3.30.0 的 request param 对 required 集合仍使用 `omitzero`。
DeepSeek 流式响应中的空 assistant message 原本含 `content: []`；经过第 2～3 步后
该字段被删除。脱敏请求摘要确认失败项为：

```text
type=message role=assistant content=false
```

紧随其后的两个 `function_call` 与两个 `function_call_output` 均完整。直接调用
DeepSeek 也证明：同一个 message 缺少 `content` 返回 400，显式传 `content: []`
则请求成功。因此这不是模型不支持 Responses，而是 AgentGo 违反自身
RequiredExact 契约。

### 2.2 正式修复

- replay item 仍先反序列化成 SDK typed union，并按 AgentGo profile 校验
  `message` / `reasoning` / `function_call`，未知类型继续 fail-closed；
- 校验通过后使用 SDK `param.Override` 携带可信 raw JSON 原样重放，不再把服务端
  output struct 二次序列化；
- 单测钉住空 `message.content=[]` 与空 `reasoning.summary/content=[]` 不丢失；
- 增加显式 opt-in 的 DeepSeek live test，真实执行
  `function_call → function_call_output → message` 两轮；
- SWE harness 把 provider `invalid_request` 纳入 known incident，Graph 正常失败不再
  掩盖 Invocation wire 事故。

该修复不按 provider/model 分支，属于 Responses exact replay 的通用机械契约，
因此没有新增 DeepSeek V4 硬编码。后续 SWE-030/031 改变了跨层契约，
已同步更新 `AGENTS.md`。

### 2.3 验证证据

- `go test ./...`、`go test -race ./...`、`go vet ./...`、`go build` 通过；
- `AGENTGO_LIVE_DEEPSEEK=1` 的真实 `deepseek-v4-flash` 两轮协议测试通过；
- `automatic-options` Run
  `run-swe-automatic-options-5be75090-a81c-4a4b-b75a-58cbb20f665f` 中：
  - `provider_invalid_request=false`，原错误文本出现次数为 0；
  - Worker 连续完成 8 轮调用、写入 1 个源码文件并提交 typed result；
  - Judge 为 resolved：`494 passed / 0 failed / 0 errors`，补丁 22 行；
  - 无外部 hard kill。

据此，SWE-029 的“多轮 Responses 重放返回 missing content”事故实现与外部证据均已
关闭。

## 3. SWE-030：DeepSeek 工具 fan-out 与单动作阶段冲突

SWE-029 修复后，单题暴露了下一项独立事故：DeepSeek 文档明确说明
`parallel_tool_calls` 参数会被忽略、并行工具调用始终开启；Verifier 在一轮中返回
3 个只读工具调用，而当前 Invocation OutputBudget 的 `MaxToolCalls=1` 连续六次
拒绝响应，最终 Acceptance 以 `loop_intervention_required` blocked。代码补丁和
Flask judge 已成功，但 Graph outcome 为 blocked。

该问题不是 replay 回归，归属 Model Invocation 输出预算与 L3 Tool dispatch 契约的
能力不匹配。正式修复为：

- 机械 singleton 阶段的 provider 响应上限保持 16，不再在 SSE 第二个
  function call item 刚出现时拒绝整个响应；
- L3 仍只 dispatch provider 顺序中的首个调用；其余 call_id 不执行、
  不产生副作用，但写入 typed skipped result，使下一轮 Responses 重放不会
  因缺 `function_call_output` 被 provider 拒绝；
- phase 冻结时校验“phase → 唯一工具名”精确映射，不能因 wire 使用
  `auto` 而扩大行动权；零 tool call 在 L3 response gate 被拒绝，不得以
  自然正文退出机械阶段；
- Proposal Acceptance 无后续 provider replay，对同一 singleton schema 的
  fan-out 按 provider 顺序消费首个 verdict，不 dispatch 其余调用；
- 启动 probe 允许一个以上同名、同 required-nonce 的 typed calls，但任一名称/
  参数不一致仍 fail-closed；
- harness 继续将 `output_limit_exceeded` 计为 architecture incident，防止未来回归。

相关回归已覆盖 Scheduler 重复批次只 dispatch 首个、错误 singleton Router
fail-closed、Proposal verdict fan-out 和启动 probe fan-out。真实 DeepSeek thinking
单题、8 题批次与受影响路径复跑均已执行；provider fan-out 未再导致
`output_limit_exceeded` 或重复副作用。

## 4. SWE-031：thinking + exact named function choice 兼容性

Scheduler create/configure/validate/commit/start、Graph deliverable submit 与 Proposal
Acceptance 都曾向 provider 发送 exact named function
`tool_choice`。DeepSeek 原生 Responses 的真实矩阵证明 thinking 会同时
拒绝 exact 与 `required`；这是 wire
兼容问题，不能用 Prompt 修复。

常规机械场景的 ToolRouter 本来就只包含一个工具，因此改为：

```text
logical exact(F) + frozen singleton ToolRouter[F]
    → provider wire tool_choice=auto
    → L3 强制至少一个 F，并只 dispatch 首个 F
```

这不是降低约束：`ToolRouterSnapshot` 冻结时会根据 phase 校验唯一工具名，
response 仍只能使用该 Registry，且机械阶段只有首个调用获得 dispatch 权。
SWE 配置与双层 probe 已从 `reasoning_effort=none` 改为 `low`，使验证
真正覆盖 thinking 路径。

真实 `https://api.deepseek.com/responses` + `deepseek-v4-flash` 最小矩阵：

| reasoning | tool_choice | strict | HTTP / 结果 |
|---|---|---:|---|
| high | required | true/false | 400 `Thinking mode does not support this tool_choice` |
| 缺省（thinking default） | required | false | 同样 400 |
| none | required | false | 200 function_call |
| high | exact function | false | 同样 400 |
| high | auto | false | 200 function_call |
| none | exact function | false | 200 function_call |

因此常规正式兼容契约必须是 **auto wire + local required-action
authority**，而不是 required wire。Graph 最终交付另有一个经真实 API
复现的粘滞历史问题：即使 ToolRouter 已经只剩 `submit_task_result`，长历史会
诱导模型继续重放旧 `read_file`/`grep_search` 调用。该阶段因此使用严格收窄的
通用路径：

```text
deliverable history projection + authoritative phase contract
    → per-invocation reasoning_effort=none
    → exact submit_task_result
```

这不修改全局 thinking 设置，也不检查 provider/model 名称；它是由 L3 phase
语义触发的 Model Invocation 覆盖。真实 opt-in live test 已验证“先用 low-thinking
调查，再用 none+exact 交付”的两轮 Responses 重放成功。

协议故障注入二进制冒烟 Run
`run-24b24338-5227-432a-a63a-338d9d3b1d21` 使用 Responses streaming，
其 mock provider 对每次请求执行三项硬校验：

1. `reasoning.effort` 必须为 `low`（thinking enabled）；
2. 任何 object 形式的 exact named `tool_choice` 直接 HTTP 400；
3. 所有 auto-singleton 调用都返回两个 typed function calls。

真实 `./agentgo` 二进制完成 startup probe、Draft create/configure/validate/
commit/start、Proposal Acceptance、Graph worker、Graph acceptance 和 final report；
Graph `graph-63dbad59-2710-4213-adcd-47660077e237` 终态为
`completed/success`。Authoring journal 按序含
`draft_created/draft_patched/draft_validated/draft_committed/start_requested/start_updated`，
Invocation failure 为 0；Trace 记录 5 个 `auto_singleton_duplicate` 与 2 个
`task_finalizing` skipped call，无第二个副作用。

该冒烟证明本地协议与跨层装配。随后的真实 DeepSeek provider SWE
单题、8 题批次与受影响路径复跑已补齐外部证据，见第 6 节。

## 5. SWE-032：code-change rollover 用尽后提前 intervention

真实 thinking 单题在无 API/Invocation failure 时仍于 7 轮 blocked。权威
checkpoint 证明 v1 声明 `rollover=6 / intervention=9`，但实现把
`AttemptRolloverCount >= MaxAttemptRollovers` 与 intervention 做了 OR，导致 rollover
后第一轮就介入。

正式修复：

- intervention 只在 `NoProgressTurns >= InterventionAfterTurns` 时生效；
- rollover 用尽但未到 intervention 阈值时继续 reminder/当前 Attempt；
- 保留 `progress:code-change/v1` 原始 3/6/9/12 与 digest；
- v2 冻结 4/8/12/16；真实批次又观测到同题成功需 14 calls 的尾部，
  因此新增 `progress:code-change/v3` 作为 Current，冻结 4/10/18/24 和
  24 model calls，simple Graph/新 mutating Task 使用 v3；v1/v2 均保留。

回归同时钉住 v1/v2 未改写、v3 独立 digest 与“rollover 用尽不提前
intervention”。真实 DeepSeek 单题与批次已运行 v3，不再复现第 7 轮提前介入。

## 6. 真实 SWE 闭环证据

全局 `reasoning_effort=low`、Responses streaming、无 API/系统代理拦截的真实运行中：

- 最终冻结二进制的 8 题完整批次为 `architecture_ok=8/8`、Judge resolved
  5/8；全批 provider `invalid_request=0`、`output_limit_exceeded=0`、
  `fragment_limit_exceeded=0`、无 external hard kill；
- 批次中 `session-access-tracking`、`teardown-callbacks` 与
  `pass-context-dispatch` 均为 `architecture_ok=true` 且 known incident=0，但当次
  模型未产出补丁，属业务求解失败；
- `automatic-options` Run
  `run-swe-automatic-options-efc4f5c1-74b6-42db-b6ca-11bfa0c52085` 为
  `completed/success`、Judge 494/494；
- `ipv6-server-name` Run
  `run-swe-ipv6-server-name-b24f9e0e-eba5-4b5e-9019-663e542b963f` 为
  `completed/success`、Judge 493/493；
- `teardown-callbacks` Run
  `run-swe-teardown-callbacks-24e81d70-9353-4fc3-a1ce-46ec64722f73` 为
  `completed/success`、Judge 495/495。
- `session-access-tracking` 用最终二进制复跑 Run
  `run-swe-session-access-tracking-69dc03f0-6019-41fa-b437-c0216a048ac4` 为
  `completed/success`、Judge 490/490、补丁 90 行，证明前次零补丁不是架构拦截。

因此，最终批次直接关闭了“DeepSeek thinking 会被 API/系统机制拦截”的
架构问题；独立复跑又证明了 8 题中 7 题至少有一次业务 resolved，尚未
resolved 的 `pass-context-dispatch` 是模型求解稳定性问题。

批次还暴露并关闭了三个模型无关的跨层问题：startup probe 未标
`strict=true`；外部化 ToolResult 的 16 KiB raw preview 经 JSON 转义后击穿 48 KiB L2
fragment 上限；final-report/recovery 响应上限错设为单调用。现在 probe 使用
strict schema，ToolResult preview 收窄为 4 KiB 但完整内容仍入 ContentRef，只读
final-report 串行 dispatch 全部调用，有状态 recovery 只 dispatch 首个并为余下
call_id 写 skipped result。Harness 的 `scheduler_tool_batch_exceeded` 也改为检查冻结
phase 中的实际 dispatch：非 final-report 单动作阶段实际执行多个工具才是
incident；provider fan-out 已被 skipped fence 吸收，或 final-report 串行多个只读
工具，均不再被误报。

## 7. 五层归属

主修改不在 L1 Prompt，而是 **Model Invocation + L3 Harness**：wire choice/
reasoning override 由 Model Invocation 负责，singleton authority、phase gate、fan-out dispatch
与交付历史投影由 L3 负责。L4 只承担后续发现的进展阈值/提前介入修复，
L2 只承担 exact replay 和 ToolResult envelope 有界化；Graph 业务状态机未因此改写。

## 8. 参考

- DeepSeek 官方：[使用 Responses API](https://api-docs.deepseek.com/zh-cn/guides/responses_api)
- 既有主链记录：[Responses typed-item 主链与单题成功回归](2026-08-23-0109-responses-mainline-and-single-task-success.md)
