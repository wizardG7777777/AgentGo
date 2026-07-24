# 测试提示词 3：动态 Team provision + 重启恢复

> 用途：回归检查「AgentTemplate Team 的 provision / 模型回落 / 重启恢复」。动态
> provision 的 team 不声明 model，回落到 `llm.default_model`——配置写错时只有这条
> 路径会炸；模板或默认模型变化后重启，digest 失配的旧 team 应被停用而不是中止启动。

## 提示词原文（直接粘贴给 AgentGo）

```text
调查 internal/team 包的 Team 生命周期管理：provision、路由注册、停止、重启恢复
各自是怎么工作的，给出带源码引用的总结。这是纯调查，不需要落盘文件。
```

这个任务会触发 verifier team 的 provision（正式验收需要），然后按下面步骤制造
重启恢复场景。

## 操作步骤

1. 让任务跑到 verifier team 被 provision（AGENTS 面板出现 verifier-team-*）。
2. 在 Plan 未终态时退出 AgentGo（/quit 或 Ctrl+C）。
3. 重新启动：`./agentgo --config setting.yaml`。

## 预期行为

- 步骤 1 后 verifier 的 LLM 调用成功（模型名回落正确，无 400 Bad Request）。
- 步骤 3 启动成功，日志出现 `[team] Team ... 已标记 stopped` 的告警而不是
  `启动失败: ... digest does not match`；恢复 Plan 后 scheduler 会重新 provision
  新的 verifier team。

## 检查方法

```bash
# 启动不应报 digest 失配；旧 team 应被持久化停用
grep -E 'stopped|digest' .agentgo/sessions/<sess>/agent-teams.json | head

# verifier 的 LLM 调用不应有 400
grep -l '400 Bad Request' .agentgo/sessions/<sess>/logs/*.jsonl
```

## 异常信号（说明回归了）

- 启动失败并提示 `agent template digest does not match durable team spec`
  → `internal/team/manager.go` 恢复容错被改回 fail-closed；
- verifier 任务 trace 出现 `400 Bad Request` 且模型名带 `deepseek/` 前缀
  → `setting.yaml` 的 `llm.default_model` 又写成了 OpenRouter 风格（应为裸模型名）。
