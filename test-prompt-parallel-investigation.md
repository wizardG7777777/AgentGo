# 测试提示词 1：并行调查 + 阶段性唤醒 + 验收一次通过

> 用途：回归检查「scheduler 唤醒门控」与「验收证据契约」。并行阶段中间完成不应唤醒
> scheduler（无 continue_waiting 空转），正式验收应一次通过，不出现证据格式 fail。

## 提示词原文（直接粘贴给 AgentGo）

```text
并行调查这个项目的 internal/store、internal/plan、internal/scheduler 三个包：
每个包一个调查任务，说明其核心职责、关键数据结构和与其他包的交互。
这是纯调查，不需要落盘文件。全部完成后做正式验收并汇报结论。
```

## 预期行为

- scheduler 在调查阶段中间（单个 explorer 完成时）**不被唤醒**；只有全部调查节点
  终态后才行动（定义验收标准 → 启动验收）。
- 验收一次通过，verdict=pass；不出现 `external acceptance fact verification failed`。

## 检查方法

```bash
# 找到 scheduler 任务（第一行，EventType=__scheduler__ 的那个）
./agentgo trace list

# 检查 scheduler 任务 loop 数与 token 消耗：loop 数应远小于"任务数 × 2"，
# 且不应出现连续多轮 continue_waiting
./agentgo trace show <scheduler-task-id>

# 验收事件：verdict 应为 pass
grep acceptance_completed .agentgo/sessions/<sess>/logs/*.jsonl
```

## 异常信号（说明回归了）

- scheduler trace 中出现多个 `replan_decided` reason=`continue_waiting` → 唤醒门控失效；
- `acceptance_completed` 的 reason 以 `external acceptance fact verification failed` 开头
  → 证据契约失效（检查 verifier 提示词与 formalContext 是否被改动）。
