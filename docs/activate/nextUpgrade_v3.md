# nextUpgrade v3 — 行哈希增强（Hashline Read Enhancer）

> 状态：📝 待实现（2026-04-09 记录）
> 依赖：v2 工具系统重构稳定后实施（`read_file` 工具接口需先确定）

---

## 1. 背景与问题

当前 `edit_file` 工具使用行号定位编辑目标。`expected_hash` 校验的是整个文件的哈希，能防止"写入错误内容"，但不能防止"LLM 基于错误行号构造了错误的编辑意图"。

典型失败场景：

```
T0: LLM 读取文件，记住 "function App() {" 在第 2 行
T1: 另一个 agent 在文件头部插入了一行注释
T2: LLM 调用 edit_file 编辑"第 2 行"，实际编辑了错误的行
    （expected_hash 此时会拦截，但 LLM 已经基于错误行号构造了意图）
```

行哈希增强通过为每行绑定基于内容的短哈希，让 LLM 用哈希而不是行号定位编辑目标，从根本上消除行号漂移问题。

---

## 2. 实现方案

行哈希增强直接集成到 `read_file` 工具内部，不走 Hook System，不需要额外的 postCall hook。

### 2.1 `read_file` 输出格式变更

启用行哈希时，输出格式从：

```
1: import React from "react"
2: function App() {
3:   return <div>Hello</div>
```

变为：

```
1#a1b2|import React from "react"
2#c3d4|function App() {
3#e5f6|  return <div>Hello</div>
```

格式：`行号#哈希|原始内容`，行号保留作为参考，哈希绑定行内容。

### 2.2 哈希计算

- 对行内容规范化（去除 `\r`，trim 尾部空格）后计算哈希
- 取短哈希（4 字符），从固定字典映射，避免 0/O、1/l 等易混淆字符
- 示例代码待补充

### 2.3 配套的 `edit_file` 校验

`edit_file` 工具在执行前校验 LLM 传入的行哈希是否仍然匹配当前文件：

- 匹配：执行编辑
- 不匹配：返回错误，提示 LLM 重新 `read_file` 获取最新哈希

这与现有 `expected_hash` 机制互补：`expected_hash` 防整文件级冲突，行哈希防行级漂移。

---

## 3. 与 Hook System 的关系

行哈希增强不走 Hook System，原因：

- postCall hook 的职责是纯观察，不能改写工具输出
- 行哈希是工具本身输出格式的一部分，属于工具层的能力，不是横切关注点
- 在工具层实现更简单、更直接，无需引入 hook 的注册/调度开销

§6 的"行哈希增强"占位节已确认为本方案，可关闭该 placeholder。

---

## 4. 待补充

- 哈希计算的具体算法和字典定义（示例代码待补充）
- `read_file` 工具的参数设计：是默认启用还是通过参数 `hashline=true` 按需启用
- `edit_file` 工具接受行哈希的参数格式定义

---

## 5. Hook System 阶段 1 延期项（2026-04-09 记录）

以下能力在 `hookSystem.md` 阶段 1 中**故意不实现**，避免重蹈 worktree 覆辙（一次性建框架时塞入未经验证需求）。每项都有明确的触发条件，等条件满足后再实现。

### 5.1 Roster.HookView 接口

**当前状态**：阶段 1 的 4 个迁移 hook（RecordArtifact / RequireReadBeforeWrite / ValidateExpectedHash / PathBoundary）都不依赖 roster，因此 `internal/roster/` 暂时不需要 `HookView` 子接口。

**触发条件**：阶段 2 之后某个 hook 真正需要"查询某个文件当前是否被某 agent 占用"或"列出所有活跃 agent"——届时再设计 `RosterHookView` 接口，按"hook 端最小必要集"原则推导方法。

### 5.2 StoreHookView 的额外方法

阶段 1 只暴露 3 个方法（`GetTask` / `AppendArtifact` / `GetToolCallHistory`）。以下方法**虽然在 `TaskStore` 上存在，但故意不暴露给 hook**：

- `PublishTask` / `ClaimTask` / `SubmitResult` 等状态变更操作（hook 不能介入任务生命周期）
- `ScanAll` / `QueryAvailable`（hook 不应做全局扫描，会改变其纯局部判定的语义）
- `GetDependencyResults` / `GetDependencyArtifacts`（hook 视野应限于当前任务，跨任务上下文应通过 LLM prompt 注入）

**触发条件**：当某个具体 hook 提出明确需求且无法用现有 3 个方法满足时，单独评估是否暴露。

### 5.3 Hook 运行时配置加载

**当前状态**：阶段 1+2 全部走编译时注册（`bootstrap.go` 里显式 `hookReg.Register(...)`），简单、类型安全、无第三方插件安全顾虑。

**触发条件**：用户提出"想在不重新编译的情况下增减 hook"的具体场景——届时设计运行时配置 schema，并配套设计沙箱机制限制运行时 hook 的能力。

### 5.4 Hook 第三方插件机制

**当前状态**：阶段 1+2 不做。

**触发条件**：出现至少 2 个独立的第三方扩展案例（来自用户而非开发者自己），且每个案例都无法用编译时注册满足。

### 5.5 Hook 异步执行支持

**当前状态**：阶段 1+2 全部同步。Tool hook 必须同步（write_file 之前要校验），mailbox hook 也同步（chain_depth 校验在发送决策中即时返回）。

**触发条件**：出现"非阻塞观察类 hook"的具体需求——比如把 hook 触发事件异步发送到外部监控系统。届时新增 `AsyncHook` 子接口，与同步 hook 隔离。

### 5.6 Chathistory / Board / Session / Skill 四类 Hook

详见 `hookSystem.md` §4。每类的触发条件统一为：**必须能写出至少 2 个具体的、独立的、当前无法解决的痛点**，否则不动。这是防止"既然有框架了，加几个 hook 又何妨"诱惑的硬规则。

### 5.7 Hook 的 ToolHookContext 字段扩展

`ToolHookContext` 当前只包含 7 个字段（Ctx / Phase / AgentID / TaskID / ToolName / Args / Result / Err）。以下扩展不在阶段 1 范围：

- `Loop int`（当前 ReAct 循环轮次）—— 等需要"基于轮次决策"的 hook 时再加
- `Depth int`（子任务嵌套深度）—— 等阶段 3 Board hook 时再加
- `History HistoryView`（任务历史只读视图）—— 改用 store 查询代替（详见 hookSystem.md §11.1）

### 5.8 Hook Action 的扩展类型

阶段 1 的 `HookAction` 只有 `Continue` / `Abort` 两个值。以下扩展不在阶段 1+2 范围：

- `Replace`：改写工具参数或结果（已在 hookSystem.md §2.1 明确放弃，理由是会让 hook 与工具调用产生耦合）
- `Defer`：让 hook 决策延迟到任务结束时再生效（无具体需求，不实现）
- `Branch`：触发新的子任务路径（违背 hook 不能发起新工具调用的原则）

### 5.9 ReplyCooldownHook（邮件级联抑制的第 4 根因）

**当前状态**：阶段 2 放弃。精确哈希对 LLM 生成消息几乎无效（微小措辞差异即绕过），词嵌入模型成本过高。级联爆炸的抑制由 `ChainDepthLimitHook` 和 `PerAgentDedupHook` 承担。

**触发条件**：阶段 2 重新启用 `mail_notifier_enabled=true` 后实测，如果级联爆炸仍然出现，再单独立项设计 reply 抑制策略（可能不是 hook 形态，可能是 prompt 工程或其他机制）。
