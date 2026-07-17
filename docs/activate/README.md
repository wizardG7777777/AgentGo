# docs/activate/ 升级文档状态总览

> 最后更新：2026-07-16
> 说明：本文档基于项目当前（v5 终态）实际代码实现情况，将所有激活状态的升级文档分类为「已实现」和「未实现」两类。

---

## ✅ 已实现

### 1. DynamicDAG.md — Scheduler + Reactor 动态任务图

- **文档状态**：设计决议完成 + 实施落地
- **关联代码**：
  - `internal/model/plan.go` / `internal/plan/` — Plan 模型、版本、原子持久化、信号聚合、正式验收、预算与无进展检测
  - `internal/bootstrap/plan_runtime.go` — TaskStore 事实与 PlanCoordinator 的桥接
  - `internal/scheduler/` — 逐 Task 终态唤醒、PlanSignal 注入和决策确认
  - `internal/tools/plan_control.go` — Scheduler / acceptance 受控工具面
- **落地方案**：一个执行节点对应一个 Task；Scheduler 持有计划内 DAG 修改权；Reactor 只提交 `request_replan`；最新 Plan scope 正式验收是完成门
- **恢复与审计**：PlanStore 和 Task snapshot v2 共同保存图身份、pending 信号、终态依赖、ReAct 历史、工具调用事实和验收事实

### 2. AgentTemplate.md — Scheduler 按需组建 Agent Team

- **文档状态**：v1 设计与实现落地
- **关联代码**：
  - `internal/agenttemplate/` — 内置/外部模板 Catalog、身份 digest、工具与模板边界校验
  - `internal/team/` — 从模板 provision Plan 级 Team、运行期容量、真实 route 和持久化恢复
  - `internal/tools/` — Scheduler 模板发现与 provision 工具
  - `internal/config/config.go` — `agent_templates.user_dirs/project_dirs/max_runtime_agents`
- **落地方案**：只有有效 `llm:` 也可 Scheduler-only 启动；`agents:` 变为可选预热容量；内置 `generalist`、`explorer`、`verifier` 三模板可直接使用
- **核心边界**：Template 描述能力，Team 表示运行实例，Task 才是 DAG 执行节点；Scheduler 必须先 provision 获得真实 route，再发布下一轮 Task
- **恢复与验收**：TeamSpec 持久化模板 ref+digest 与稳定 route；正式验收可按需 provision verifier，仍服从最新 Plan scope 的统一验收门

### 3. ReactiveSystem.md — Reactor 驱动的状态响应架构

- **文档状态**：设计决议完成 + 实施全部落地
- **关联代码**：
  - `internal/reactor/` — Reactor 接口、Registry、builtin 实现（record-artifact / task-end-callback / trace-history-event / read-set-write）、userdef YAML 加载器
  - `internal/gate/` — Gate 接口、Registry、Phase 路由（Tool/Mailbox 域）、Decision 机制
  - `internal/agent/state.go` — Agent 四状态枚举 + SetState + 合法性校验
  - `config.example.yaml` — reactors_file 配置字段
- **落地方案**：Phase 1-7 全部完成；v5.x 增量（`call:`、per-kind reactors、`request_replan`）也已落地
- **已废弃的关联方案**：原 Provider 抽象 → 由 MemoryManageSystem 承接；原 Aggregator → 下放 Mailbox 子系统

### 4. TraceUpgrade.md — v5 trace 事件结构化升级

- **文档状态**：Spec 定稿（2026-05-01），所有内容已实现
- **关联代码**：`internal/trace/event.go`
- **已实现内容**：
  - 4 个新 EventKind：`KindAgentStateChanged` / `KindShellExecuted` / `KindShellTimeoutPending` / `KindShellTimeoutResolved`
  - `Transition` 子结构体（Task/Agent 双状态机 + Cause + CancelSource + RetryCount）
  - `ShellExec` 子结构体（Command / ExitCode / DurationMS / Outcome）
  - `ShellTimeout` 子结构体（Command / ElapsedSec / Decision / ExtraSeconds）
  - Schema B 方案（嵌套子结构体指针，omitempty 向前兼容）
  - CLI viewer 适配（formatEventDetails + detectAnomalies 启发式规则）
- **向前兼容**：新字段全部 `omitempty`，旧 v4 jsonl 可正常读取

### 5. InterfaceDesign.md — 接口与结构体设计

- **文档状态**：反映当前已实现架构的接口参考文档
- **关联代码**：
  - `internal/agent/` 的 Agent/AgentRuntimeState 结构体
  - `internal/store/` 的 TaskStore 接口
  - `internal/roster/` 的 Roster 接口
  - `internal/trace/` 的 Event 结构体
  - `internal/tui/` 的 Bubble Tea TUI 实现
- **待升级方向**（已标注为「后续升级」）：
  - 多面板布局（alt-screen）
  - trace stream 旁路（Subscriber 接口）
  - 输入体验增强（多行 / 历史回溯 / 命令补全）
  - 命令扩展（`/replay /trace /snapshot /readset /memory`）
  - 上述均已在接口文档中明确标记为「已知方向，按需排期」

---

## ❌ 未实现（含部分实现和待推进项）

### 6. MemoryManageSystem.md — v5 记忆管理系统

- **文档状态**：📋 起草中（2026-04-30），优先级 P0（v5 关键架构）
- **已实现部分**：
  - `internal/memory/` 包 — `Store` 接口（Put / Query / Delete / Clear）+ `Entry` / `Scope` / `Kind` 类型定义
  - `ProcessStore`（ScopeProcess 内存实现）— 已完成可用
  - Agent 集成 — `Agent.Memory` 字段已就位（nil 安全）
  - `injectMemoryContext` 方法已替代旧 AgentHook 功能
  - AgentHook 子系统已整体删除（MM6/MM7 对应）
- **未实现部分**：
  - MM3: TeamSnapshot 写入侧（scheduler/worker → Memory.Put）
  - MM4: FileAwareness 写入侧（Roster 监听 → Memory.Put）
  - MM8: ScopeSession 文件存储后端（`.agentgo/sessions/*/memory.jsonl`）
  - MM9: ScopeProject 持久化后端（`.agentgo/memory/`）
  - 向量检索（QueryByVector 接口已预留，实现返回 ErrNotImplemented）
- **实施建议**：MM1-MM7（仅 Process 作用域 + Agent 集成）已基本完成；MM8-MM9 作为 v5 增量可按需推进

### 7. ToolUpgradePlan.md — v5 Shell 工具升级规划

- **文档状态**：📋 起草中（2026-04-30），优先级 P1
- **未实现关键项**：
  - `shell_commands.yaml` 配置文件 → ❌ 不存在（既无默认模板也无运行时自动生成）
  - `ShellCommandGate`（shell.CommandFilter 重构为正式 Gate）→ ❌ 不存在
  - `TimeoutHandler` 接口 + `TruncateHandler` 实现 → ❌ `internal/shell/timeout.go` 不存在
  - YAML 占位 schema（`shell.timeout_handler`）→ ❌ 未代码化
  - System prompt 处理纪律（worker/explorer 模板更新）→ ❌ 待落地
  - T3: 4 选项 Approval UI 持久化（yaml.v3 Node API）→ ❌ 待落地
  - T5: TimeoutHandler 抽象 + YAML 占位 schema 解析 → ❌ 待落地
  - T6: KindShellExecuted / KindShellTimeoutPending / KindShellTimeoutResolved 事件 emit → ❌ 待落地
- **当前已有**：`internal/shell/` 下的命令拦截（intercept.go）和规则引擎（rules.go）已有基础审批能力，但尚未按本文档规格重构
- **依赖**：T1 依赖 ReactiveSystem Phase 1（已完成），其余 T2-T6 均未启动

### 8. HALLUCINATION_ACCEPTANCE_AUDIT.md — 幻觉引用验收审计报告

- **文档状态**：审计报告已完成（2026-05-01），但审计结论为「有条件不通过」
- **已完成**：审计报告的撰写与发布
- **未实现的改进建议**（P0 — 必须实施）：
  - 引用验证 Hook（CitationVerifierHook）→ ❌ 不存在
  - 强制检索决策门（RetrievalGate）→ ❌ 不存在
  - E2E 幻觉测试基线 → ❌ 零个自动化测试
- **未实现的改进建议**（P1 — 强烈建议）：
  - Trace 富化（可选记录完整工具结果）→ ❌ 未引入 ResultExcerpt 或 KindToolResultFull
  - web_fetch 截断显式标注 → ❌ 未添加
  - SearXNG Source 字段修复 → ❌ 未修复
- **未实现的改进建议**（P2 — 长期优化）：
  - 检索结果结构化引用 ID → ❌ 未实现
  - 模型级引用对齐 → ❌ 未实现

### 9. KNOWN_ISSUES.md — 已知问题清单

- **文档状态**：已删除
- **说明**：历史问题清单已清理。相关背景仍保留在 `docs/archived/` 的各阶段设计文档中。

---

## 汇总视图

| 文档 | 实现状态 | 关键代码位置 | 备注 |
|------|---------|------------|------|
| DynamicDAG.md | ✅ 已实现 | `internal/plan/`, `internal/scheduler/`, `internal/tools/plan_control.go` | 动态图 + 正式验收 + 防失控 + 恢复 |
| AgentTemplate.md | ✅ 已实现 | `internal/agenttemplate/`, `internal/team/`, `internal/tools/` | Scheduler-only 启动 + 按需团队 + ref/digest 恢复 |
| ReactiveSystem.md | ✅ 已实现 | `internal/reactor/`, `internal/gate/`, `internal/agent/state.go` | Phase 1-7 + v5.x 增量全部完成 |
| TraceUpgrade.md | ✅ 已实现 | `internal/trace/event.go` | TraceUpgrade 本身已落地 |
| InterfaceDesign.md | ✅ 已实现 | 全项目 | 反映当前已实现架构 |
| MemoryManageSystem.md | ⚠️ 部分实现 | `internal/memory/` | 接口 + ProcessStore 完成，Session/Project 后端未实现 |
| ToolUpgradePlan.md | ❌ 未实现 | `internal/shell/`（仅基础审批）| 核心组件（shell_commands.yaml, TimeoutHandler, ShellCommandGate）均不存在 |
| HALLUCINATION_ACCEPTANCE_AUDIT.md | ❌ 未实现 | — | P0/P1 改进建议均未落地 |
| KNOWN_ISSUES.md | ✅ 已删除 | — | 历史问题清单已清理，背景见 `docs/archived/` |
