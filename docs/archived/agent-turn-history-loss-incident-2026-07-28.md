# Agent 轮次输出覆盖事故（2026-07-28）

## 现象

Agent 工作台只稳定展示一个 `Current Turn`。同一轮的流式 delta 会原位更新是正确的，但下一次 LLM 调用使用新的 `stream_id` 后，界面仍只选择最新一项；旧轮次即使短暂存在于有界 feed，也会被后续输出淘汰。Scheduler 与普通 Runner 共用这条 UI 链路，因此两者都会发生，只是 Scheduler 的循环更长、更容易观察到。

## 根因

- `output.KindStream` 只表达在途累积快照，没有“本次 LLM 调用已经完成”的不可变事件。
- UI Hub 的 `FeedSnapshot.Outputs` 是最多 200 条的重连窗口，不是历史账本。
- Agent 卡片只有截断的 `LastModelText`，TUI/Web 工作台只投影最新流。
- Session 没有单独保存公开模型轮次；刷新、重连和切换 Session 后无法恢复。

## 修复契约

1. 每次 `TaskExecutor` 调用生成稳定轮次 ID；流式 delta 继续以同 ID 原位更新。
2. 调用返回后发布恰好一个 `output.KindTurn`。正文只取 assistant 对外可见文本；工具轮只附工具名，不复制参数、结果或 reasoning。
3. UI Hub 收到完成事件后把轮次冻结为 `completed` / `failed`，并追加到当前 Session 的 `turns.jsonl`。每轮只执行一次 append + fsync；失败留在内存队列，由轮询重试。
4. `turns.jsonl` 是 Session 级追加账本。读取时保持文件顺序，跳过崩溃半行或损坏行；Session 切换加载目标账本。迟到的旧 Session 完成事件仍写回原 Session，不污染当前视图。
5. TUI 与 Web 使用同一 `Snapshot.Turns` 投影。普通 Agent 和 Scheduler 不再分叉：都按 Loop 展示全部轮次。TUI 以相对底部偏移滚动，`0` 自动跟随最新，`Home` 到最早，`End` 恢复跟随。

## 兼容边界

升级前已经丢失的旧轮次无法从 `LastModelText` 或有界 feed 反向恢复。升级后的新 Session 会按需创建 `turns.jsonl`；没有轮次的历史 Session 读取为空，不需要迁移。

## 回归证据

- `internal/agent/turn_output_test.go`：每次调用唯一完成事件、失败时保留部分正文、工具参数不进入账本。
- `internal/session/turns_test.go`：多行正文往返、显式 Session 归属、损坏行恢复。
- `internal/ui/service_test.go`：同轮 upsert、跨轮追加、feed 淘汰隔离、持久化重试、迟到旧 Session 事件。
- `internal/bootstrap/ui_hub_test.go`：真实 Hub 装配把完成轮次落到 Session 账本。
- `internal/tui/feed_test.go`：普通 Agent/Scheduler 全轮次展示、终态冻结、最早/最新滚动边界。
- `internal/dashboard/server_test.go`：Snapshot、`OutputTurn` 与 `TurnsChanged` 的 Web 协议。
