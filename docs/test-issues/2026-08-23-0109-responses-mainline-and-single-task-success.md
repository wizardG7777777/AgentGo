# Responses typed-item 主链与单题成功回归

> 时间：2026-08-23 01:09 AWST
> 范围：Model Invocation、L2 Context、L3 Harness、L4 Loop、L5 Graph、SWE harness
> 任务：`automatic-options`
> Provider/模型：OpenRouter `/responses`，`openai/gpt-5.6-luna`

## 1. 结论

AgentGo 新主链已从仅有 Chat Completions 扩展为显式协议双轨：

- `llm.protocol=responses` 是生产默认与 SWE 主链；
- `llm.protocol=chat_completions` 只保留显式兼容，不按 provider 名称推断；
- 工具行动只来自 typed `function_call` output item；message 中的 DSML/XML/
  Markdown 永远不提升为行动；
- Responses 的完成 output items 作为 RequiredExact carrier 经 L2 replay，工具结果
  以匹配 `call_id` 的 `function_call_output` 回传；
- partial SSE 只计预算/显示，不在 `output_item.done` 前 dispatch。

修复后的真实单题同时通过架构门和正确率门：

```text
architecture_ok=true
task_resolved=true
process_terminal=graph_terminal
graph_status=completed
graph_outcome=success
judge=494 passed / 0 failed / 0 errors
external_hard_kill=false
```

## 2. SWE-027：DSML/思维内容与工具信封混淆

### 2.1 事故事实

旧 DeepSeek-V4-Flash Chat Completions 运行中，模型/兼容网关会把原生 DSML
片段放入 `content` 或污染 `tool_calls[].name`。AgentGo 没有把
`reasoning_content` 拼成正文，但空参数 capability probe 只能证明 provider 能
返回一个空 `{}` 调用，不能证明 required arguments、多轮 tool output replay 或
message/reasoning/tool-call 分型稳定。

### 2.2 正式修复

1. Model Invocation 新增 `responses` / `chat_completions` 冻结 protocol；不自动回退。
2. 新增有序 `OutputItem` 类型：message、reasoning、function_call。
3. Responses SSE 只消费显式事件/item type；未知 output item、重复 done、delta/done
   arguments 不一致、incomplete/failed 均在 dispatch 前形成 typed failure。
4. L2 通过保留字段 `agentgo_responses_output_items` 原样 replay typed items；该
   carrier 进入 RequiredExact representability gate，不作为 assistant 自由扩展发送。
5. 产品启动 probe 和外部 harness probe 均改为随机工具名 + 必填随机 nonce，并
   精确验证 call identity、argument object 与 typed finish。
6. SWE 默认使用 OpenRouter Responses + Luna；旧 provider 必须显式声明
   `chat_completions`。

### 2.3 测试

- typed function call；
- streaming reasoning/function-call 分离；
- DSML message 不形成工具调用；
- typed output item + function_call_output replay；
- unknown item fail-closed；
- Responses 产品 startup probe 与 required nonce；
- harness Responses/Chat compatibility probe。

## 3. SWE-028：Verifier 新证据被探索预算误杀

### 3.1 首次真实运行

首次 Luna Responses 运行中：

- Scheduler 正确完成 create/configure/validate/commit/start；
- Worker 完成 grep/read/edit、两次 pytest 与 `submit_task_result`；
- 源码补丁实际使 Flask 全量测试成为 `494 passed`；
- Verifier 连续三轮 read/grep/read，每轮都被 ProgressEvaluator 判为
  `knowledge_progress`，`no_progress_turns` 始终为 0；
- `ExplorationTurnsSinceDeliverable=3` 超过 verification policy 的 2 后，却被
  `no_progress_budget_exhausted` 直接 blocked。

这是 L4 机械契约错误：真实新证据不是 no-progress，探索额度只能触发交付阶段，
不能直接宣告任务阻塞。

### 3.2 正式修复

- `MaxExplorationTurns` 从 hard-block 条件移出；
- 超额后生成结构化 delivery-required control notice；
- 下一 Invocation 的 ToolRouter 进入 `agent:deliverable-submit`，只暴露
  `submit_task_result`。当时 wire 冻结 exact function `tool_choice`；SWE-030/031
  后已替换为 auto + singleton + L3 required-action gate；
- 因此 Agent 必须提交 pass/fixable/blocked，不能继续无限读取，也不会因真实
  knowledge progress 被误杀。

### 3.3 第二次真实运行

脱敏权威结果：

| 证据 | 值 |
|---|---|
| Run ID | `run-swe-automatic-options-b1fd6e2b-6302-4c40-879b-e5de8be8e401` |
| 墙钟时间 | 94 秒 |
| Model calls | 23 |
| Prompt / completion tokens | 171270 / 2021 |
| 首个 Scheduler prompt | 2205 tokens |
| 首个 GraphDraft | 第 1 次调用 |
| Graph revisions / activations | `[1]` / 3 |
| Task 状态 | 全部 completed |
| Outcome delivery pending | 0 |
| Effect / Artifact records | 6 / 1 |
| Invocation failures | 0 |
| 已知事故形状 | 0 |
| Judge | resolved，494 passed，tests 未篡改 |
| Context / typed item manifest | `context:default/v8` / `assistant_response_items`×73 |

## 4. 五层归类

| 范围 | 本次权威变化 |
|---|---|
| Model Invocation | Responses typed SSE/item、协议冻结、required-argument probe |
| L1 Prompt | 无提示词止血；任务和角色 Prompt 未承担 wire 修复 |
| L2 Context | `context:default/v8` + Replay v3 typed output items RequiredExact replay，reasoning/message 不混写；v1–v7 digest 不改写 |
| L3 Harness | 工具只收 typed call；`list_dir({})` 由声明默认值规范化为 `path=.`；缺参为 retryable invalid arguments |
| L4 Loop | exploration 超额进入强制 deliverable phase，不再假冒 no-progress block |
| L5 Graph | Draft/commit/start、Worker、Acceptance、typed success outcome 全链通过 |

## 5. 仍开放

- 尚未运行本阶段的一批 8 题，不能把单次 Luna 成功外推为跨题稳定性；
- `chat_completions` compatibility adapter 尚未删除；
- Responses 当前 profile 只允许 message/reasoning/function_call，未来新增 output
  item 必须显式设计后开放，不能静默忽略；
- tokenizer/provider/cross-platform matrix 仍需持续补证。
