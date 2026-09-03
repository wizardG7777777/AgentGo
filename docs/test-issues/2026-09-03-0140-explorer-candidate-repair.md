# Explorer 证据交接与 Candidate Repair

> 日期：2026-09-03  
> 状态：Implemented / targeted live verification; business closure open  
> 问题编号：SWE-120

## 事故

SWE-119 后完整 mixed-model Flask-8 为 business 4/8。四道失败统一经历
decision stall → retry → deadline；三题零 mutation，session-access-tracking 虽有
edit/check，却没有完成候选交付。

首轮 Explorer 路由实测又发现 investigation/v2 可把 novel evidence 误记为
deliverable：pass-context-dispatch 的 Explorer 使用 52 次调用、50 次 grep、31 次
read，旗舰 Worker只剩7次调用。forced submit 还清空工具正文，使 Explorer明确报告
“看不到已读内容”，handoff 基于记忆推断。

## L1-L5 修复

- L1：Explorer 输出增加 evidence_ranges；Worker/recovery Prompt 区分 v4 全文件与
  v5 bounded focus，禁止顺序翻完整文件。最新 session-access 实测又证明快速
  Explorer 会把公开 proxy / framework 内部访问边界误诊为叶子容器覆写；Prompt
  现要求从失败断言分别追踪公开入口、状态背板和内部 lifecycle consumer，并做反证。
- L2：investigation exact submit 使用单一≤8KiB settled-evidence notice，移除历史
  ToolCall但保留当前 Task 源码片段；v4 进一步按 read/read_content_ref、grep、
  list/glob 分层选择，同层 newest-first，防止十轮末尾 grep 挤掉早期失败断言。
  SWE Test Runner 还把 Agent 启动前的 targeted 红态归一为≤6000字符 authority
  输入块，避免只读 Explorer 依据 issue 示例猜测第一失败。
- L3：RecoveryDelta v5 / ChangeDecision v2 绑定 candidate_state；focus page 支持
  path/offset/limit、未读 edit target 拒绝、resume_candidate 与冻结 check。
- L3：simple-task/v4 使用 `agentgo.investigation-boundary/v2`，除三段边界对象、
  evidence_ranges 与 rejected_alternative 外，还必须先冻结第一条具体
  failure_observation；TaskOutcome authority 从 ProjectRoot 重读范围，拒绝不存在的
  候选 symbol。相同校验前移到 submit_task_result finalizing 之前，错误可修正重交，
  durable commit 再校验防绕过。v1 / simple-task/v2-v3 不迁移。
- L4：investigation/v6 六轮有界交付并为下游预留8分钟（v3-v5 历史不迁移）；code-change/v12 一轮 decision checkpoint，
  dirty candidate 在 execution deadline 前预留3分钟交 L5，
  最终 repair 固定使用 v10；typed change decision 重置 decision/cadence；v5 分
  stage completion budget。
- L5：Graph v4 simple-task/v4 路由 Explorer → work，并用唯一 candidate-repair
  producer继续同一 Delivery。SWE 的 Explorer 业务模型改为旗舰档，机械 Observation
  保持显式快速档；路由只依赖 YAML role，不按 provider/model 名称分支。

## 验证证据

- `go test ./...`、`go vet ./...`、Windows build、Python SWE Test Runner 单测；
- fake-provider 真实二进制：Graph success、v5 mutation/check、Acceptance、Delivery
  committed；v12 current 下 ledger/trace 29 calls 对账、active reservation=0；
- Observation v7/v8/v9 × empty/populated × fast/flagship 实际兼容矩阵通过；
  configured flagship 的 v10/v11 empty/populated preflight 在定向题前通过；
- automatic-options：resolved、architecture/model contract true；
- pass-context-dispatch：Explorer 从52 calls 降至8–10；真实进入 focus/typed edit，
  但多方法重构仍未在 execution window 内完成；
- session-access-tracking：repair 成功 edit sessions.py、targeted check 后按失败结果
  need_context(ctx.py)，在下一修正前 deadline；business 仍 failed。

### 2026-09-03 最终定向复测

- 当前冻结组合：simple-task/v4、investigation/v6、code-change/v12、RecoveryDelta v5、
  investigation-boundary/v2；v12 已同步 Graph fulfillment 正向闭集，fake 与真实
  Run 均证明 DeliveryID 可在单 decision-turn handoff 前绑定。
- `session-access-tracking` 最终 Run
  `run-swe-session-access-tracking-3cd188be-4f4d-4131-a93a-6be0e215fcdf`：47 calls、
  948,141 prompt tokens、31,246 completion tokens；`architecture_ok=true`、
  `model_contract_compatible=true`、infra=0、hard kill=false、active reservations=0。
- Observation provider invalid=0，Observation checkpoint failure=0；一次结构化 handoff
  预检错误可在 finalizing 前修正重交，L5 recovery 与 first-action gate 均完成。
- Repair 形成 workspace candidate 并运行两次真实 targeted check，但第二次检查失败后在下一
  修正前 `invocation_deadline`；candidate 未 promotion，最终 Judge 仍 489 passed / 1
  failed、patch_lines=0、tests tampered=false。

因此控制链、证据门和时间分配相较基线显著收敛，但该题业务未闭合；完整 Flask-8
batch 仍不得启动，避免把已知门槛失败扩散为高成本批次。

investigation/v7 的10分钟 reserve 实验 Run
`run-swe-session-access-tracking-85318e6c-bee2-4dba-8428-a306a0e8d116`
进一步证明：pre-dispatch 后单次 provider correction 从 Invocation 开始到完整返回
接近300秒并穿透 reserve；旧 trace 不含首 SSE/首 delta，不能把该数字解释为 TTFT。
2026-09-03 后新增的 `agentgo.llm-invocation-timing/v1` 才能在未来运行中拆分
transport、首 SSE、首个 model delta、完整输出与最大事件间隔；
Work 最终 candidate handoff 时 Recovery 已无法再启动 Repair。v7 因此保持非 current，
current 回到 v6。该瓶颈需要更低延迟/更强模型或新的 provider-call cancellation/SLA
契约，不能继续靠缩短业务 turn 或放宽 L3 解决。

因此本阶段证明控制/交接修复有效，但没有达到 business 6/8 或完整 batch closure。
