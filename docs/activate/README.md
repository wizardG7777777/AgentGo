# docs/activate 文档状态

最后核对：2026-07-19。这里保存仍会影响当前实现的设计约束、尚未完成的路线图，以及它们的历史归档入口；它不是日常启动手册。

## 当前应优先使用的文档

| 问题 | 权威入口 |
| --- | --- |
| 如何启动、TUI/Web Dashboard 与安全边界 | [README.md](../../README.md) |
| YAML 字段、默认值和校验 | [YAML 配置指南](../yaml-config-guide.md) 与 [config.example.yaml](../../config.example.yaml) |
| 当前组件关系、启动和关停流程 | [Archtechture.md](../../Archtechture.md) |
| Trace 字段、CLI 与排错 | [TraceGuide.md](../../TraceGuide.md) |
| 当前限制、验证缺口和运行注意事项 | [KNOWN_ISSUES.md](KNOWN_ISSUES.md) |

代码与测试优先于所有设计文档；当设计记录和 `internal/` 当前实现冲突时，以代码、配置校验和测试为准。

## 仍在使用的设计记录

| 文档 | 状态 | 使用方式 |
| --- | --- | --- |
| [DynamicDAG.md](DynamicDAG.md) | 已实现，仍是 Plan 不变量参考 | 修改 Plan、验收、暂停/恢复或重规划前阅读 |
| [AgentTemplate.md](AgentTemplate.md) | 已实现，仍是按需 Team 契约 | 修改模板 catalog、provision 或 runtime route 前阅读 |
| [ReactiveSystem.md](ReactiveSystem.md) | 已实现，保留 Gate/Reactor 决策背景 | 修改 Gate、Reactor、状态事件或用户 Reactor schema 前阅读 |
| [MemoryManageSystem.md](MemoryManageSystem.md) | 部分实现 | 仅用于 Memory 后续工作；现状见架构文档和 [KNOWN_ISSUES.md](KNOWN_ISSUES.md) |
| [ToolUpgradePlan.md](ToolUpgradePlan.md) | 未实施的 Shell 路线图 | 不是当前 Shell 行为说明；当前配置以 YAML 指南为准 |

## 已归档的完成或历史记录

| 历史文档 | 归档位置 | 原因 |
| --- | --- | --- |
| Trace schema Phase 2 升级设计 | [trace-upgrade-design-2026-05.md](../archived/trace-upgrade-design-2026-05.md) | 实现已超出原 2026-05 范围；现行字段以 TraceGuide 和代码为准 |
| 旧 TUI 接口设计 | [interface-design-tui-2026-05.md](../archived/interface-design-tui-2026-05.md) | 多前端 UI Hub/Web Dashboard 已替代其前端边界假设 |
| 幻觉引用验收审计 | [hallucination-acceptance-audit-2026-05.md](../archived/hallucination-acceptance-audit-2026-05.md) | 它是一次历史审计；其中尚未落实的建议已转入当前已知限制 |
| UI Hub 改造问题与修复总账 | [ui-hub-remediation-2026-07-18.md](../archived/ui-hub-remediation-2026-07-18.md) | 所列修复已完成；活动问题清单不再重复历史账目 |

归档文档用于追溯设计决策，不能作为当前配置、接口或测试行为的唯一依据。
