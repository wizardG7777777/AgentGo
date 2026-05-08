# ToolUpgradePlan：v5 工具系统升级规划

> **状态**：📋 起草中（2026-04-30 创建，首批承接从 ReactiveSystem.md §7.4 迁移的 Shell 工具改造内容）
> **优先级**：P1（v5 工具层重要重构，Shell 是首个重点工具）
> **关联文档**：
> - [ReactiveSystem.md](ReactiveSystem.md)（Gate / KindShellExecuted 事件 / WaitingApproval 状态等基础设施由其定义；本文档是它在工具层的具体落地）
> - [nextUpgrade_v4.md](nextUpgrade_v4.md) §7 Hashline / §10 Did-You-Mean
> - [TraceUpgrade.md](TraceUpgrade.md)（事件 payload 结构化升级，本文档涉及的新事件 schema 由其定稿）

---

## 1. 背景与边界

v5 阶段除了 ReactiveSystem 核心重构外，工具层也需要配套升级以适配新的 Gate / Reactor 抽象。本文档承接所有 v5 工具升级工作，**首批重点是 Shell 工具**——它是 AgentGo 与现实世界交互的核心入口、是当前唯一会触发 `WaitingApproval` 状态的工具，其在 ReactiveSystem 体系下的设计需要专门的规格定稿。

**与 ReactiveSystem.md 的分工**：
- **ReactiveSystem.md** 定义 Gate / Reactor 两类核心抽象本身，以及 Agent 状态机、事件流、Reactor 注册机制等**基础设施**（注：原计划的 Provider 抽象已废弃由 [MemoryManageSystem.md](MemoryManageSystem.md) 承接；Aggregator 已下放为 Mailbox 子系统内部固定机制不立顶层）
- **本文档（ToolUpgradePlan.md）** 定义具体工具如何嵌入这套基础设施——Shell 工具的命令名单、approval UI 形态、超时策略、持久化等**工具侧实施细节**

**未来扩展位**（本文档可承接的其他工具升级，按需添加新章节）：
- 文件类工具（write_file / edit_file / read_file 等）的 Gate 集成
- 网络类工具（如未来引入 http_request）的安全治理
- MCP 工具集的统一管理

---

## 2. Shell 工具升级（首批重点）

Shell 工具（`run_shell`）的完整改造规格在 2026-04-30 经多轮讨论后定稿，集中记录于本节。决议来源：ReactiveSystem.md Q11 / Q12 / Q13 / Q11.r。

### 2.1 决议汇总

| 议题 | 决议 |
|---|---|
| **Q11** shell 命令名单结构 | 单一文件 `shell_commands.yaml`，扁平的 `blacklist` / `whitelist` 两个字符串数组；灰色地带（未列入任一）走 4 选项 approval；**没有"系统硬编码不可移除"层**——文件即真相源 |
| **Q11** shell.CommandFilter 重构 | 从工具内部抽出，重构为正式 Gate（与 path-boundary 同档），ReactiveSystem Phase 1 命名空间清理时一并完成 |
| **Q11** approval 4 选项 | Approve Once / Approve All / Reject Once / Reject All（详见 §2.4）|
| **Q11** 持久化 | Approve All / Reject All 直接写回 shell_commands.yaml，使用 yaml.v3 Node API + atomic write + backup |
| **Q11** 匹配语法 | 前缀匹配；通配符 `*` 仅末尾使用；**整体匹配**（不拆分管道/分号）；shell-aware tokenization（识别引号和转义）|
| **Q11** 初始化 | 启动时若 `shell_commands.yaml` 不存在，从 AgentGo 内置模板自动生成 |
| **Q11.r** reject 后处理机制 | reject 作为工具调用错误返还（与 path-boundary Abort 同档）+ system prompt 补充处理纪律 |
| **Q12** KindShellExecuted 事件 | 进 v5 首批触发事件清单，但**仅内置 Reactor 可订阅**，用户 YAML schema 暂不开放（占位预留）|
| **Q13** shell 超时机制 | 用户配置 timeout 阈值（单位秒）即用户容忍上限；超时拆为「事件 + TimeoutHandler」二段式抽象（详见 §2.8）：v5 仅内置 `truncate` handler（行为不变），但接口层 + YAML 占位 schema 一开始就立住，未来 `wait` / `consult_llm` / `message_agent` 等 handler 可增量加而无需重构 |

### 2.2 shell_commands.yaml 配置形态

```yaml
# shell_commands.yaml —— shell 命令名单单一真相源
# 扁平结构：仅 blacklist 与 whitelist 两个字符串数组

blacklist:
  - "rm -rf /"
  - "rm -rf /*"
  - "dd if=* of=/dev/*"
  - "mkfs *"
  - "mkfs.* *"
  - "git push --force *"
  - "git push -f *"
  - "npm publish *"
  - "kubectl delete *"

whitelist:
  - "git status"
  - "git log *"
  - "git diff *"
  - "git branch"
  - "ls *"
  - "pwd"
  - "cat *"
  - "head *"
  - "tail *"
  - "grep *"
  - "echo *"
  - "go test *"
  - "go build *"
  - "go vet *"
```

**初始化机制**：启动时若文件不存在，从 AgentGo 仓库内置的 `internal/tools/shell/default_shell_commands.yaml` 复制一份到项目根，并打印 warning 提示用户审视。文件存在则直接读取。用户可自由编辑、版本控制、删除任何条目（**包括内置模板里的危险命令条目**——AgentGo 不强制 enforce）。

### 2.3 Gate 决策流程

shell 命令进入 Gate 后的处理顺序：

```
1. 命中 blacklist  → Abort (Gate 拦截，命令不执行)
2. 命中 whitelist  → Continue (needs_approval=false，自由执行)
3. 都不命中        → Continue (needs_approval=true，触发 4 选项 UI)
```

**Q9 措辞精修**：ReactiveSystem.md §7.2 中"waiting_approval 仅在 needs-approval 工具调用时触发"的 needs-approval **不再是工具属性**，而是 **Gate 计算结果**。run_shell 工具本身不再硬编码 `needs_approval=true`，由 ShellCommandGate 根据查询黑白名单后给出。这是 Q9 决议的措辞延伸，本质语义不变但精度更高。

### 2.4 4 选项 Approval UI 与 agent 通知

灰色地带命令进入 approval 流程时，用户面对 4 选项：

| 选项 | 行为 | 给 agent 的工具结果 |
|---|---|---|
| **Approve Once** | 仅放行这一次，不修改任何配置 | 命令执行结果（正常 string）|
| **Approve All** | 命令模式进白名单（用户可在 UI 编辑前缀模式）+ 立即放行 | 命令执行结果（正常 string）|
| **Reject Once** | 仅拒绝这一次，不修改配置 | **作为工具调用错误返还** —— `Error: shell command denied by user (rejected this attempt; pattern still available for retry with variations).` |
| **Reject All** | 命令模式进黑名单 + 立即拒绝 | **作为工具调用错误返还** —— `Error: shell command denied by user (pattern permanently blacklisted; do not retry similar commands).` |

**Approve All 的模式生成**：UI 显示当前命令 + 输入框预填精确命令字符串（如 `git push origin main`），用户可改成更宽泛的前缀模式（如 `git push *`）后确认。系统不自动猜测前缀粒度——用户拥有最终决定权。

**reject 作为错误返还的设计意图（Q11.r 决议 2026-04-30）**：

reject 的工具结果不是"成功 + 解释字符串"，而是**作为工具调用错误（与 path-boundary Abort、validate-line-anchors 失败同档）**返还给 agent。这与 v4 §7 / §10 现有的 Gate Abort 错误处理路径完全一致——agent 看到的就是一个明确的"工具调用失败"信号。

工具结果文本短小、精确，不包含冗长的"应当如何"指导。**详细的处理纪律由 system prompt 约束**（详见 §2.5），让两类信息各居其位：

- **工具结果**：每次调用都附带，token 开销敏感 → 仅表达"权限被拒"事实
- **System prompt**：每个 LLM 请求注入一次，承担"如何处理被拒"的纪律说明

**两类 reject 的语义差异保留**：
- Reject Once：暗示"这次不行，但模式没禁"——agent 可试不同变体
- Reject All：明示"模式被永久禁了"——agent 必须避开整个模式

### 2.5 System prompt 处理纪律（Q11.r 配套）

为让 agent 在收到 reject 错误后做出可预期的决策，所有 kind 的 system prompt（worker / explorer / 用户自定义 kind）需补充以下处理纪律：

```
工具权限处理纪律：
- 如果工具调用因权限被拒（错误信息含 "shell command denied by user"），
  优先转向侵入性更小的替代方案（例如：用 read 类命令代替 write 类、
  改用 git diff 代替 git push、用 dry-run 模式验证而非真实执行等）。

- 如果缺少权限就无法完成任务，必须在任务报告中明确说明因果关系：
  "由于用户拒绝了 [命令 X] 的执行权限，[子任务 Y] 无法完成。"
  这种汇报让用户可追溯任务失败的根本原因。

- 不要在短时间内反复试探已被拒绝的命令模式。特别是被永久 blacklist 的模式
  （错误信息含 "pattern permanently blacklisted"），必须立刻转向根本不同的方案。
```

**这条纪律的设计哲学**（用户决议 2026-04-30）：

接受 reject 可能让 agent 最终选择**提前终止任务**，但这是**用户可感知的失败**——任务报告里会明确写"因为我无法执行 X 命令导致 Y 子任务失败"。用户回看任务报告时能立即识别"哦，是我刚才拒绝了那个命令导致的"——失败因果链对用户透明、可追溯。

这与 OpenCode 的"reject 后终止 agent + 用户输入新指令"模式完全不同：

| 维度 | OpenCode 模式 | AgentGo 模式 |
|---|---|---|
| reject 后处理 | 终止 agent，等用户输入新指令 | 作为错误返还，agent 自适应 |
| 用户介入次数 | 每次 reject 都需要用户继续打字 | 仅 approval UI 一次，之后 agent 自主 |
| 多 agent 场景 | 不可行（用户无法对单个 agent 单独下指令）| 自然适配 |
| 失败可追溯性 | 用户记得自己拒了什么 | agent 在汇报中显式说明因果，用户事后可查 |
| 适用场景 | 单 agent 1:1 对话 | 多 agent + scheduler 编排 |

**实施位置**：v5 默认提供的 `prompts/worker.md` / `prompts/explorer.md` 模板（v4 §11.8 已落地）追加上述纪律段落。用户自定义 kind 的 system_prompt_file 由用户自行决定是否包含——AgentGo 不强制注入。

**Phase 1 命名空间清理时同步处理**：worker / explorer 模板更新与 shell.CommandFilter 重构为 Gate 同 PR 上线，避免出现"Gate 已经返回 error 但 prompt 还没教 agent 怎么处理"的中间态。

### 2.6 匹配语法详细规则

**Tokenization**（shell-aware）：

```
"git status"             → tokens: [git, status]
"echo hello world"       → tokens: [echo, hello, world]
"echo 'hello world'"     → tokens: [echo, "hello world"]   # 引号内视为单 token
"git push --force main"  → tokens: [git, push, --force, main]
```

**通配符 `*`**：仅允许出现在**末尾**作为前缀通配。中间通配（如 `git * --force`）**不支持**——用户需细粒度控制时写多条规则。

| 名单条目 | 匹配 | 不匹配 |
|---|---|---|
| `git *` | `git status` / `git push origin main` / `git` | `gitlab status` / `make git` |
| `git push *` | `git push origin main` | `git status` / `git pull` |
| `git pull -X theirs *` | `git pull -X theirs main` | `git pull origin main` / `git pull -X ours main` |
| `git`（无 *）| `git`（精确）| `git status` |

**整体匹配**：管道 / 分号 / 连接符**不拆分子命令**——整个命令字符串作为一个整体查名单。

```
"ls | grep foo" 整体不匹配 "ls *"     ← 因为 token 流里有 "|"
"ls | grep foo" 需单独加白名单条目     ← 例如 "ls * | grep *"
```

**用户体验提示**：spec 阶段需在文档明文标注此简化的代价——使用管道的命令需要显式加入名单，避免用户踩坑。

### 2.7 持久化机制（写回 shell_commands.yaml）

Approve All / Reject All 触发时，AgentGo 直接修改 `shell_commands.yaml`。实施纪律 3 条：

1. **使用 `yaml.v3` Node API**（而非裸 Marshal/Unmarshal）——保留用户写的注释与键序
2. **Atomic write**：写到 `shell_commands.yaml.tmp` → fsync → `os.Rename` 替换。避免写一半进程被杀导致文件损坏
3. **写入前 backup**：保留 `shell_commands.yaml.bak`，万一 round-trip 出意外用户能恢复（Phase 7 端到端测试覆盖几个 round-trip 用例）

**估计代码量**：约 100-150 行 + 几个测试用例（注释保留 / 空文件 / 缺一个字段 / 重复条目去重 / 损坏文件恢复）。

### 2.8 Shell 超时机制

用户在配置中声明 shell 超时阈值（单位秒，沿用 v4 已有的 `shell_timeout_sec` 字段），**该阈值即用户的容忍上限**。

**设计哲学**：用户配的阈值就是用户的最终决定，AgentGo 不二次猜测、不做 LLM 智能分诊、不引入软超时与硬超时分级。**v5 行为**就是到点直接 truncate。

但当前讨论意识到一件事：v5 行为虽然简单，**架构不应该把行为焊死**。否则未来任何超时策略变体（让 LLM 判进展、让另一个 agent 决定、让用户弹窗拍板等）都需要重写整套超时机制。

#### 2.8.1 二段式抽象：事件 + TimeoutHandler

把「超时事件」和「超时行为」解耦：

```
shell 命令运行时长达到 timeout 阈值
   ↓
emit KindShellTimeoutPending 事件（事实记录，不决策）
   ↓
TimeoutHandler.OnTimeout(ctx) → 返回 TimeoutDecision
   ↓
按 decision 执行（v5 只走 Truncate 一档）
   ↓
emit KindShellTimeoutResolved 事件（携带 decision）
   ↓
emit KindShellExecuted（outcome=timeout）
```

**这是哪一类抽象**——既不是 Reactor 也不是 Gate：

| 抽象 | 决策权 | 触发时机 | 适合超时？ |
|---|---|---|---|
| Reactor | ❌ 无（ReactiveSystem 原则 4：不能驱动状态转换）| 状态变化**之后** | ❌ 超时必须做决策 |
| Gate | ✅ 有（Continue/Abort）| 动作**之前** | ❌ 超时是动作**进行中** |
| **TimeoutHandler**（新）| ✅ 有 | 动作**进行中** | ✅ 这个生态位 |

它是**第三类决策点**——跟 Gate 同档但触发时机不同。Gate 决定"要不要让命令开始跑"；TimeoutHandler 决定"命令跑了 N 秒还没完，怎么办"。架构上独立于 Reactor / Gate 单列。

#### 2.8.2 接口形态（v5 立住）

```go
// internal/shell/timeout.go（新增）

type TimeoutDecision int

const (
    TimeoutDecisionTruncate    TimeoutDecision = iota // SIGKILL + 部分输出 + 错误返还（v5 唯一内置）
    TimeoutDecisionWait                                // 再给 N 秒，到点重新进 OnTimeout（v5 接口预留，handler 不实现）
    TimeoutDecisionContinue                            // 不限时跑完（v5 接口预留，危险但允许）
)

type TimeoutHandlerResult struct {
    Decision     TimeoutDecision
    ExtraSeconds int    // 仅 Wait 决策有效
    Reason       string // 写入 trace，便于事后审计
}

type TimeoutHandler interface {
    OnTimeout(ctx context.Context, info TimeoutInfo) TimeoutHandlerResult
}

type TimeoutInfo struct {
    AgentID         string
    Command         string
    ElapsedSec      int       // 已运行时长
    StdoutSoFar     string    // 截至触发时刻的累积输出
    StderrSoFar     string
    PreviousWaits   int       // 已经 Wait 续命过几次（防 handler 无限续命）
}
```

#### 2.8.3 v5 唯一内置实现：TruncateHandler

```go
type TruncateHandler struct{}

func (TruncateHandler) OnTimeout(ctx context.Context, info TimeoutInfo) TimeoutHandlerResult {
    return TimeoutHandlerResult{
        Decision: TimeoutDecisionTruncate,
        Reason:   fmt.Sprintf("default truncate after %ds", info.ElapsedSec),
    }
}
```

执行 Truncate 决策的具体动作（与原 §2.8 一致，行为零变化）：

```
TimeoutDecisionTruncate
   ↓
SIGKILL 子进程
   ↓
当前累积的 stdout/stderr → 作为工具结果返回 agent
工具结果末尾追加："[ERROR] command timed out after Ns"
   ↓
agent 在下一轮 ReactLoop 自然处理（重试 / 改命令 / 放弃，由 LLM 决定）
```

#### 2.8.4 YAML 占位 schema

```yaml
shell:
  timeout_sec: 60              # 沿用 v4 字段
  timeout_handler: truncate    # v5 唯一合法值
  # 未来允许：wait_then_truncate / consult_llm / message_agent / escalate_to_user
  # max_wait_chains: 3         # 防 Wait handler 无限续命（接口已预留 PreviousWaits）
```

**v5 校验纪律**：
- `timeout_handler` 字段缺省即 `truncate`（向后兼容用户没配的情况）
- 任何非 `truncate` 的值在配置加载阶段 fail-fast，错误信息明确指出"v5 仅支持 truncate，X 拟在 v5.x 增量"
- 与 §6.1.6（ReactiveSystem.md 的占位 schema vs 真实实现边界）同一种"占位"哲学

#### 2.8.5 trace 事件配套

`KindShellTimeoutPending` / `KindShellTimeoutResolved` 进 ReactiveSystem.md §6.4 首批 trace 事件清单（与 `KindShellExecuted` 同档），仅向**内置 Reactor** 开放订阅，用户 YAML schema 暂不开放。

```
shell_timeout_pending:    {task_id, agent_id, command, elapsed_sec, stdout_excerpt, stderr_excerpt, triggered_at}
shell_timeout_resolved:   {task_id, agent_id, command, decision, extra_seconds, reason, resolved_at}
                          # decision 枚举：truncate / wait / continue
                          # 即使 v5 只产出 truncate，schema 一开始就支持三档
```

**事件设计的好处**：未来内置 Reactor 也能监听超时事件做日志/metric/告警/批量分析，不必走 TimeoutHandler 链——事件订阅与决策处理解耦。

#### 2.8.6 v5 不内置但架构允许的 handler 形态（未来扩展位）

| 候选 handler | 行为 | 何时引入 |
|---|---|---|
| `wait_then_truncate` | 第一次超时 Wait(extra_sec) 续命，再次超时则 Truncate | 实战出现"长跑命令偶尔卡顿"需求时 |
| `consult_llm` | 调用 ReactiveSystem 的 isolated LLM client（原则 5）评估 stdout 进展，决定 Truncate / Wait | LLM 成本下降 + prompt 工程成熟时 |
| `message_agent` | 通过 mailbox 询问监督 agent，等回复决定动作 | 引入 supervisor agent 模式时 |
| `escalate_to_user` | 弹 approval-like UI 让用户选 Truncate / Wait / Continue | CLI 前端单键提示能力到位后（详见 CLIUpgrade.md 拟新建）|

**关键约束（继承 ReactiveSystem 原则 5）**：`consult_llm` handler 的 LLM 调用必须是上下文隔离的纯文本生成器——无工具、无 history、无运行时上下文注入。绝不能让"超时判断器"演变成无监督的影子 agent。

#### 2.8.7 与原 §2.8 "显式不做"的关系

原 §2.8 列了三条"v5 拒绝路径"——`forward_to_agent` / `llm_check` / 软硬超时分级。这三条**所表达的 v5 行为决议依然成立**（v5 不会真的实现它们），但**架构语义需要重新解读**：

| 原"显式不做" | v5 当前状态 | 未来语义 |
|---|---|---|
| `forward_to_agent`（partial 输出推流给 agent）| ❌ 不实现 | 仍然不打算做——这需要"分批工具结果"模型，与同步工具调用范式根本冲突，属于架构级翻新而非 handler 增量 |
| `llm_check` | ❌ 不实现 | ✅ 架构层允许，未来作为 `consult_llm` handler 加入 |
| 软超时/硬超时分级 | ❌ 不实现 | 仍然不打算做——`wait_then_truncate` 已经覆盖该场景的实质需求，且更符合"用户阈值即容忍上限"哲学（用户配单一阈值，handler 内部决定如何分配） |

#### 2.8.8 实施工作量估算

| 项 | 估算 |
|---|---|
| `TimeoutHandler` interface + `TimeoutDecision` 枚举 + `TimeoutInfo` struct | ~30 行 |
| `TruncateHandler` 实现 + 现有 SIGKILL 逻辑迁入 | ~50 行（大半是迁移） |
| YAML schema 解析 + fail-fast 校验非 `truncate` 值 | ~20 行 |
| trace 事件 emit（pending + resolved）| ~15 行 |
| 单元测试（TruncateHandler 行为不变 + 非 `truncate` 值报错 + Wait/Continue 决策的占位 fail-fast）| ~80 行 |
| **总计** | **~195 行** |

比原 §2.8 直接硬编码 truncate 多约 100 行——这 100 行就是"未来扩展位"的前期投入。考虑到避免后续重构的代价，这笔投入合理。

### 2.9 KindShellExecuted 事件的开放策略

按 Q12 决议，`KindShellExecuted` 进 ReactiveSystem v5 首批 trace 事件清单，但**用户 YAML schema 暂不开放**——即用户不能在 `reactors:` 块里写 `on: shell_executed`。仅开放给：

- **内置 Reactor**（开发者注册的 Go 代码）
- 用例：审计日志、metric 记录、shell 失败兜底等系统级 Reactor

**为什么折中而非完全开放**：
- shell 命令频次可能极高（特别是用户场景"shell 占比大"），用户 reactor 的 `when:` 条件设计错误容易引发性能问题或循环触发
- 完全开放需要为 shell_executed 撰写一套用户文档、教学示例、错误恢复指南，v5 首版不打算吃这部分文档撰写成本
- 等内置 Reactor 落地后实战观察具体需求，再决定如何向用户开放

**何时开放给用户**（触发条件）：
- 实战出现 1-2 个内置 Reactor 解决不了、用户必须自己挂 shell_executed 才能解决的具体场景
- 或 v5.x 增量需求驱动

文档侧 spec 阶段会明文标注此限制，避免用户误用。

**事件 payload 草案**（与 ReactiveSystem.md §6.4.5 对齐，最终由 TraceUpgrade.md Phase 2 定稿）：

```
shell_executed:  {task_id, agent_id, kind, command, exit_code,
                  stdout_excerpt, stderr_excerpt, duration_ms,
                  outcome, executed_at}
                 # outcome 枚举：success / failure / timeout
                 # stdout_excerpt / stderr_excerpt 截断（前后各 N 字节），完整内容仍在 trace 文件
```

---

## 3. 不在本文档范围

- **Gate / Reactor 两类核心抽象本身的设计**：归 [ReactiveSystem.md](ReactiveSystem.md)（原 Provider 抽象已废弃由 [MemoryManageSystem.md](MemoryManageSystem.md) 承接；原 Aggregator 已下放为 Mailbox 子系统内部固定机制）
- **Agent 实例状态机（idle / processing / waiting_approval / terminating）**：归 [ReactiveSystem.md §7.1-§7.3](ReactiveSystem.md)
- **trace 事件 payload 结构化升级**：归 [TraceUpgrade.md](TraceUpgrade.md)（Phase 2 落地）
- **其他工具的 Gate 集成**：未来按需在本文档新增章节，当前不展开
- **Shell 命令的"系统硬编码不可移除黑名单"层**：按 Q11 决议，shell_commands.yaml 是单一真相源，**没有用户不可移除的硬编码黑名单层**
- **WaitingApproval 的"持续时长触发"超时**：reactor 不支持"状态持续超过 N 分钟自动触发"——独立模块（需要定时器调度），留作 v5.x

---

## 4. 后续计划

Shell 工具升级与 ReactiveSystem.md Phase 1（命名空间清理）配套上线：

| 步骤 | 工作 | 配套 ReactiveSystem 阶段 |
|---|---|---|
| T1 | shell.CommandFilter 重构为 ShellCommandGate | Phase 1 |
| T2 | shell_commands.yaml schema + 加载器 + 默认模板 | Phase 1 |
| T3 | 4 选项 approval UI + 持久化（yaml.v3 Node API）| Phase 1 |
| T4 | worker.md / explorer.md prompt 模板更新（system prompt 处理纪律）| Phase 1（与 T1 同 PR）|
| T5 | TimeoutHandler 抽象 + TruncateHandler 内置实现 + YAML 占位 schema | Phase 1 |
| T6 | KindShellExecuted / KindShellTimeoutPending / KindShellTimeoutResolved 事件 emit（仅内置 Reactor 订阅）| Phase 2-3 |

各步骤的详细 spec 在 Phase 1 启动时定稿。
