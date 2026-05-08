# v4 实现偏差修复计划

> 生成时间：2026-04-28
> 来源：nextUpgrade_v4.md 实现完成度核查报告
> 修复策略：按章节分组，由近及远（§11 → §10 → §9）逐章修复，每章一个 commit。

---

## 修复总览

| 优先级 | 章节 | 问题数 | 类型分布 |
|--------|------|--------|----------|
| P3 | §11 | 8 | 2 未实现 + 6 偏差 |
| P3 | §10 | 3 | 3 偏差 |
| P3 | §9 | 2 | 1 未实现 + 1 偏差 |

---

## §11 统一 Agent 声明式配置

### 11.1 未实现项

#### [§11-1] `TokenStats` 累计统计（未实现）
- **文档位置**：nextUpgrade_v4.md §11.7.3
- **问题**：`Agent` 结构体缺少 `TokenStats` 字段及累加逻辑
- **修复内容**：
  1. `internal/agent/agent.go` 新增 `TokenStats struct`（`TotalPromptTokens int64`, `TotalCompletionTokens int64`, `CallCount int`）
  2. `Agent` 结构体追加 `TokenStats TokenStats` 字段
  3. `processTask` 中 LLM 调用返回后累加 `resp.Usage.PromptTokens` / `CompletionTokens`
  4. 输出运行时日志：`[worker-1] 本轮: prompt=4213, completion=892 | 累计: prompt=38721, completion=8402, 调用=12`
- **新增测试**：`token_stats_test.go` 验证累加逻辑

#### [§11-2] `dependency_map.go` 独立映射表（未实现）
- **文档位置**：nextUpgrade_v4.md §11.6.2
- **问题**：工具→依赖项映射当前内联在 `runner.go:82-104`，未作为独立文件存在
- **修复内容**：
  1. 新建 `internal/bootstrap/dependency_map.go`
  2. 将 runner.go 中 ToolGroup 注册逻辑抽象为 `resolveDependencies(allowedTools []string, deps RunnerDeps) runner.RunnerDeps`
  3. 注释说明：每新增工具同步更新此映射
  4. `runner.go` 改为调用 `resolveDependencies`

### 11.2 偏差项

#### [§11-3] `keepRecentForTruncate` 文档同步（偏差）
- **文档位置**：nextUpgrade_v4.md §11.7.4
- **问题**：文档和 §11.7.4 伪代码写 6，实际代码为 3（2026-04-27 紧急修复）
- **修复内容**：
  - **仅修改文档** `nextUpgrade_v4.md` §11.7.4：将 `keepRecentForTruncate = 6` 改为 `= 3`，补充说明 2026-04-27 修复原因（32K 上限下 6 条 tail 必爆）
  - 不修改代码（代码已正确）

#### [§11-4] `Validate()` 反斜杠检查覆盖不全（偏差）
- **文档位置**：nextUpgrade_v4.md §11.5.3 规则 9
- **问题**：仅检查了 `agents[*].system_prompt_file`，未检查 `ProjectRoot`、`llm.base_url` 等路径字段
- **修复内容**：
  1. `config.go` `Validate()` 中新增对所有路径字段的反斜杠扫描
  2. 覆盖字段：`ProjectRoot`、`LLM.BaseURL`（？）、`agents[*].system_prompt_file`
  3. 注意：`BaseURL` 是 URL 不是文件路径，需确认是否纳入

#### [§11-5] `schedulerMaxLoops` 文档同步（偏差）
- **文档位置**：nextUpgrade_v4.md §11.5.4 / §11.6.6
- **问题**：文档要求 =10，实际代码 =30
- **修复内容**：
  - **方案 A**：改代码回 10（可能影响调度器行为）
  - **方案 B**：修改文档为 30，补充说明上调原因
  - **建议**：选 B，因为 30 是运行时调优后的值，改回 10 可能导致复杂任务 loops 耗尽

#### [§11-6] `filepath.FromSlash` 决策记录（偏差）
- **文档位置**：nextUpgrade_v4.md §11.5.2
- **问题**：文档要求做 `filepath.FromSlash`，代码注释明确说"不做"（避免 Windows 上反斜杠被 Validate 拒绝）
- **修复内容**：
  - **仅修改文档** `nextUpgrade_v4.md` §11.5.2：删除"代码侧做 filepath.FromSlash"要求，补充说明 Windows 兼容性原因
  - 不修改代码（代码已正确）

#### [§11-7] v3 遗留类型清理（偏差）
- **文档位置**：nextUpgrade_v4.md §11.4 / Phase D
- **问题**：`config.go` 中仍保留 `AgentDeclaration` / `defaultCapabilities` / `defaultDescriptions`
- **修复内容**：
  1. 确认 `AgentDeclaration` 是否被任何代码引用
  2. 如无引用，删除 `AgentDeclaration` 类型、`defaultCapabilities`、`defaultDescriptions`
  3. 如有引用（如某些测试），先迁移再删除

#### [§11-8] 过时注释清理（偏差）
- **文档位置**：无
- **问题**：
  - `runtime_builder.go` 头注释："尚未被 Bootstrap 主流程调用"（实际已调用）
  - `team_snapshot.go` 头注释："worker 包仍持有副本"（实际已删除）
- **修复内容**：
  1. `runtime_builder.go`：更新头注释，删除"尚未被调用"段落
  2. `team_snapshot.go`：更新头注释，删除"worker 包仍持有副本"段落

---

## §10 工具调用错误恢复（Did-You-Mean）

### 10.1 偏差项

#### [§10-1] 包路径偏差（偏差，不修复代码）
- **问题**：文档要求 `internal/tools/suggest/`，实际在 `internal/suggest/`
- **修复策略**：**不移动代码**（移动包会破坏所有 import，风险高）。仅修改文档，将 `internal/tools/suggest/` 改为 `internal/suggest/`。

#### [§10-2] 文档描述同步（偏差，不修复代码）
- **问题**：文档开头称"MVP 仅接入 2 处"，实际 5 处全部落地
- **修复策略**：**仅修改文档**，将 "MVP 已落地（`glob_search` / `read_file` 两处接入）" 改为 "MVP 已落地（5 处全部接入：glob_search / read_file / list_dir / grep_search / 工具调度层）"

#### [§10-3] 测试覆盖缺口（偏差，需补测试）
- **问题**：`read_file` 路径不存在、`glob_search` 空结果两处的 Did-You-Mean 缺少直接单测
- **修复内容**：
  1. `internal/tools/local_read_test.go` 新增 `TestReadFile_NotExist_DidYouMean`：构造不存在的路径，断言返回消息含 "Did you mean"
  2. `internal/tools/local_read_test.go` 新增 `TestGlobSearch_Empty_DidYouMean`：构造无匹配的 glob pattern，断言返回消息含 "Did you mean"

---

## §9 运行时致命错误快速失败

### 9.1 偏差项

#### [§9-1] `maxRecoverableRetries` 常量（偏差）
- **文档位置**：nextUpgrade_v4.md §9.6
- **问题**：文档要求包级常量 `maxRecoverableRetries = 3`，实际使用 `Agent.MaxRetries`
- **修复内容**：
  1. `internal/agent/agent.go` 新增常量 `maxRecoverableRetries = 3`
  2. `handleFailure` 中 `task.RetryCount >= a.MaxRetries` 改为 `>= maxRecoverableRetries`
  3. **注意**：这会改变行为——当前 `a.MaxRetries` 来自 YAML `task_max_retries`（per-kind 可配置），改硬编码 3 后用户失去 per-kind 调节能力
  4. **替代方案**：不改代码，仅修改文档，说明实际使用 `Agent.MaxRetries`（per-kind 可配置）比硬编码 3 更灵活
  5. **建议**：选替代方案（不改代码，改文档），因为 per-kind 可配置性比硬编码更有价值

#### [§9-2] `--skip-startup-probe` 命令行旗标（未实现）
- **文档位置**：nextUpgrade_v4.md §9.7
- **问题**：`main.go` 仅注册 `-config`，未实现 `--skip-startup-probe`
- **修复内容**：
  1. `main.go` 新增 `skipStartupProbe` bool flag
  2. `Bootstrap()` 签名从 `Bootstrap(configPath string, explicit bool)` 扩展为支持 skip probe
  3. 或更简洁：在 `Bootstrap()` 内部检查 flag，若设置则跳过 probe 调用
  4. 注意保持与 `startup_probe: off` 的等价性

---

## 修复执行记录

| # | 章节 | 修复内容 | 状态 |
|---|------|----------|------|
| 1 | §11-8 | 过时注释清理（runtime_builder.go + team_snapshot.go） | ✅ 已完成 |
| 2 | §11-3 | keepRecentForTruncate 文档同步（6→3） | ✅ 已完成 |
| 3 | §11-5 | schedulerMaxLoops 文档同步（10→30） | ✅ 已完成 |
| 4 | §11-6 | filepath.FromSlash 文档同步（删除要求） | ✅ 已完成 |
| 5 | §11-7 | v3 遗留类型清理（AgentDeclaration / defaultCapabilities / defaultDescriptions） | ✅ 已完成 |
| 6 | §11-4 | Validate 反斜杠覆盖扩展（增加 ProjectRoot） | ✅ 已完成 |
| 7 | §10-1/2 | 文档同步（suggest 包路径、MVP 接入点数） | ✅ 已完成 |
| 8 | §10-3 | 补测试（read_file / glob_search Did-You-Mean） | ✅ 已完成 |
| 9 | §9-1 | 文档同步（maxRecoverableRetries → Agent.MaxRetries） | ✅ 已完成 |
| 10 | §9-2 | `--skip-startup-probe` 命令行旗标 | ✅ 已完成 |
| 11 | §11-2 | dependency_map.go 提取 | ✅ 已完成 |
| 12 | §11-1 | TokenStats 实现 | ✅ 已完成 |

**验证结果**：`go build ./...` + `go test ./...` 全绿。
