# 契约冻结与 Recovery 首动作收口

> 日期：2026-08-30
> 状态：实现完成，模型行为验证待授权
> 范围：SWE-096～SWE-099

> 后续修订：真实批次发现多代 directive 与单首动作缺口后，code-change recovery
> 已升级到 v3；本文件的 v2 描述保留为当时实施事实。当前语义见
> [`2026-08-30-1630-recovery-handoff-v3.md`](2026-08-30-1630-recovery-handoff-v3.md)。

## 1. 结论

剩余失败更像频繁架构演进留下的跨层接缝，而不是单一模型能力结论：提示词仍声明
租约已经移除的工具，no-progress 计数被压缩成容易误读的总量名称，L5 Recovery
虽然给出“下一动作”，L3 却没有机械执行它。小模型更容易放大这些歧义，但这些
歧义本身属于架构责任，必须先修正后才能继续讨论先天能力下限。

本轮先冻结当前版本，再只修复接缝；不引入 provider/model 分支，不改变模型分配。

## 2. 问题分层

| ID | 层级 | 问题 | 收口 |
|---|---|---|---|
| SWE-096 | L1 Prompt | SWE Worker 声明 `run_shell`，与 Graph v3 mutating Lease 不一致；活动提示词还含经验调用次数、等待时间等固定数字 | Prompt 以当前 ToolRouter 为权威，删除经验数字与冲突工具 |
| SWE-097 | L2 Context | 终止原因把 no-progress 窗口渲染为 `turns/model_calls`，Recovery 容易误读成累计总量 | 保留 `no_progress_*` 和 `exploration_turns_since_deliverable` 权威名称 |
| SWE-098 | L3/L5 Harness/Graph | Recovery v1 的自由文本 `first_required_action` 只是建议，下一 Worker 仍看到完整工具面 | 新图发布 RecoveryDelta v2；类型化 `first_action` 进入首轮 ToolRouter gate，路径冻结为 schema const |
| SWE-099 | 横切契约 | 多个 schema/policy/prompt 版本并行演进，缺少明确冻结基线与升级纪律 | 新增冻结文档；改变语义必须升版本并定义旧快照恢复/拒绝规则 |

## 3. 冻结版本

权威基线见 [`contract-freeze-2026-08-30.md`](../design/contract-freeze-2026-08-30.md)。关键点：

- 新 simple Graph 写 `agentgo.recovery-delta/v2`；v1 只恢复历史数据。
- v2 schema、handler、Graph replay、Task Context 与 L3 ToolRouter 使用同一
  `first_action={tool,path?}`。
- v2 节点不能提交 v1 delta；未知字段、非法工具、工具与 path 组合错误均 fail-closed。
- Prompt 不承载经验轮数、token、秒数或候选数量；机械阈值留在冻结 policy/schema。

## 4. 验证边界

用户明确要求本轮不启动新模型验收、不运行普通测试。允许的验证只有：

- Go benchmark，使用 `-run '^$'` 跳过普通测试；
- 编译；
- `-skip-startup-probe` 的无模型二进制启动冒烟。

因此 benchmark 只证明 v2 decode 与首动作 ToolRouter 接缝可执行，不证明 Flask
业务题正确率已经改善。下一次真实 SWE 批测必须把模型行为结果与架构门分别报告。

## 5. 本轮证据

- `go test -run '^$' -bench 'Benchmark(DecodeRecoveryDeltaV2|RecoveryFirstActionToolPolicy)' -benchmem ./internal/graph ./internal/agent`
  通过；普通测试被明确跳过。Windows/amd64 结果：
  - `BenchmarkDecodeRecoveryDeltaV2`：`5528 ns/op`；
  - `BenchmarkRecoveryFirstActionToolPolicy`：`5499 ns/op`；
  - `BenchmarkRecoveryFirstActionToolPolicyAttempted`：`2351 ns/op`。
- `go build ./...` 通过。
- 临时 main binary 使用 `setting.v4.yaml -skip-startup-probe` 启动，真实完成
  Bootstrap/Runner 装配并输出“系统就绪”；stdin EOF 后丢弃空 Session、正常关闭，
  退出码为零。该过程没有用户任务、没有 provider probe、没有模型调用。
