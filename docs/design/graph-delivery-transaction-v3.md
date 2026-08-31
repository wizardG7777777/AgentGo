# Graph Delivery Transaction v3

`agentgo.graph/v3` 把 mutating Graph 的候选 workspace、L4 `TaskOutcome`、Acceptance verdict 和最终 Graph success 绑定为一条 L5 Delivery Transaction。

## 不变量

- Delivery ID 由 Run、Graph 与首次 producer activation 稳定导出；repair generation 递增但不换 ID。
- workspace 必须持久化 `agentgo.workspace-owner/v1`；物理 `delivery-*` 目录名
  不是 TaskID。每个 Agent Activation 使用期间持有活动租约，Watchdog 不得
  清理活动租约或非终态 Graph 的候选。
- mutating producer 必须 `isolation: workspace`，只可使用 `write_file`、`edit_file` 与 `run_check`；禁止 raw `run_shell`。
- worker completed 只冻结 candidate，不能合并主 workspace。blocked/failed 候选保留供恢复或隔离审计。
- Candidate 必须由 L3 对非空 dirty manifest 与文件内容计算稳定 digest，不能
  由 TaskOutcome 拼接字符串。业务工具不得写 `.agentgo/**` 控制面。
- 稀疏 COW 根不是可执行项目树。`run_check`/Shell 使用排除
  `.agentgo`/`.git` 的可丢弃完整快照，每次运行前以 dirty manifest
  覆盖；快照本身不得进入 candidate/merge。
- `TaskOutcome v3` 必带 `delivery_id`；有 workspace fulfillment 时还必须有 `candidate_ref`。
- Acceptance 冻结同一 `delivery_id` 并以只读能力进入 producer
  workspace，不得在主根核验旧版本。`pass` 是唯一 promotion
  入口；L5 先重算 candidate digest 排除验收期 TOCTOU，再写
  Effect Journal prepared、合并 candidate，settle 后才允许 Graph success。
- `GraphOutcomeRecord{outcome:success}` 必带 `delivery_commit_ref`；没有已确认 promotion 的成功 outcome fail-closed。
- prepared/unknown promotion 不自动重跑。恢复只能返回同一 settled commit ref，或将图阻断并要求人工裁决。
- DeliveryStore 持久化 open→prepared→verifying→repairing/commit_prepared→
  committed|quarantined|commit_unknown。Effect settled 后若 transaction commit
  落盘失败，恢复只允许用同一 EffectRef 补齐，不重新执行 merge。
- Watchdog 只可清理由 success GraphOutcome 的 `delivery_commit_ref` 证明已经
  promotion 的残留目录；blocked/failed/cancelled candidate 保留并在
  Transaction 中记录 quarantine reason。

read-only v3 图不创建空 Delivery，也不要求 `delivery_commit_ref`；只有包含
`progress:code-change/*` mutating producer 的图进入上述事务。

v1/v2 Graph、TaskOutcome 与快照继续按旧语义恢复，不迁移、不改写。v3 首版每张图只接受一个 mutating producer；需要并行修改时必须拆为独立 Delivery Graph 后再由上层协调汇合。
