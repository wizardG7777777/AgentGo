# SWE-045：Observation thinking replay 与有界失败

> 日期：2026-08-24
> 状态：closed / implementation-complete / local-and-external-validation-complete

## 事故事实

SWE-041～044 新主链的外部回归中，`automatic-options` 单题以
`architecture_ok=true / task_resolved=true / 494 passed / patch_lines=23` 通过。随后
8 题批次的第二题 `context-push-order`（Run
`run-swe-context-push-order-653067dc-c9cd-425b-b75d-fc517387022c`）触发架构门：

- 21 次 `malformed_response`，其中包含 Observation phase 返回旧
  `grep_search/read_file/run_shell` 工具名；
- 16 次 `output_limit_exceeded`，均为 `tool_calls.count actual=2 limit=1`；
- 1 次 provider 400：`reasoning_text in the thinking mode must be passed back`；
- Worker 最终 `non_recoverable_error`，零 Effect、零 Artifact、零补丁。

架构门将该题标记为 `architecture_ok=false` 并立即停止批次，没有把
Graph typed failed 误报为正常模型业务失败。

## 分层根因

| 层级 | 漂移 | 后果 |
|---|---|---|
| L3 Harness / Invocation | 非终态 Observation checkpoint 被临时切换为 `reasoning=none + exact tool_choice` | checkpoint 成功后恢复 thinking 时，上一轮没有可重放的 reasoning item，违反 DeepSeek Responses 协议 |
| L3 Harness / Invocation | singleton 的 response 接收上限也被缩为 1 | provider fan-out 在 L3 “只 dispatch 首个”之前就被 SSE accumulator 误杀 |
| L4 Loop | provider/格式/response gate 失败没有形成 ToolCall，原失败计数只统计失败的 `record_observation_delta` ToolResult | “允许一次修正”契约不可达，同一 checkpoint 空转数十轮 |

## 正式修复

- Observation 是机械 exact **action**，不是终态 protocol mode：wire 继续使用
  thinking + `tool_choice=auto` + singleton `record_observation_delta` schema，L3
  required-action gate 负责拒绝自然文本/错工具并仅 dispatch 授权首调用。
- checkpoint 本轮使用只保留 control notice 的 L2 机械历史投影，不让
  provider 沿用旧 grep/read/edit tool-call intent；Raw History 不改写，TaskMemory、
  任务契约与动态 evidence enum 由独立 authority 注入。checkpoint 产生的
  reasoning item 仍在后续普通业务轮重放。
- response 层仍允许最多 16 个 tool item；这只是协议可表示性上限，
  不扩大工具权限或实际 dispatch 数。
- 只有不再恢复业务 thinking 的终态 `submit_task_result/report_done`
  保留 `reasoning=none + exact` 狭义例外。
- 任何 checkpoint invocation/response/tool-call/参数失败都记入同一有界计数：
  首次允许修正；第二次周期 checkpoint 保留 Raw History 并恢复业务工具，
  强制 checkpoint 则产生 typed `observation_checkpoint_failed` 交 L5 recovery。
- SWE 架构门新增 `observation_checkpoint_retry_storm` 与
  `reasoning_mode_replay_break`，以及与成败无关的连续 phase 上限
  `observation_checkpoint_attempt_limit_exceeded`，防止同类事故被
  Graph failed/success 掩盖。回执识别同时覆盖 `错误:` / `错误：`
  和 `已跳过:` / `已跳过：`，不再让全角冒号绕过计数。

## 验证与 closure

本地 focused Go tests 与 Python harness 32 项已通过。真实二进制 fake-provider
smoke 已强制检查 Observation request 保持 `reasoning.effort=high` + `tool_choice=auto`，
并检查其 encrypted reasoning item 在下一业务 Invocation 原样重放；Graph
最终 success，Observation/Check/Recovery/final-report 均 durable。

SWE-045 的首次定向复跑还暴露“两次失败后下一知识 turn 重试”会形成
成组失败；运行已主动中止，上述机械历史投影是对该第二层事故的正式修复。
Observation 成功后的批次继续又发现全角 `错误：` 未被计为失败，使同一
checkpoint 出现第三次提交；该批次即使业务补丁已形成也被主动中止，
字符集兼容与独立 attempt-limit 架构门已补齐。
## 外部 closure 证据

- `context-push-order` 定向复跑实际覆盖两个 Observation 周期：
  首周期成功，次周期两次 evidence lineage 失败后正确放弃，
  没有第三次调用；最终 Graph success、Judge 487/487。
- 最终 8 题 cohort 中 `observation_checkpoint_retry_storm`、
  `observation_checkpoint_attempt_limit_exceeded` 与
  `reasoning_mode_replay_break` 均为零，`architecture_ok=8/8`。
- harness 曾把不同 checkpoint 周期的失败累加成 storm；现按连续
  Observation phase 分组，33 项 Python contract tests 覆盖跨周期不合并。

外部 closure 条件全部满足，SWE-045 关闭。
