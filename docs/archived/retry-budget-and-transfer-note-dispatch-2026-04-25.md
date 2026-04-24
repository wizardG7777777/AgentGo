# 重试预算角色语义化 + TransferNote 分类分派（2026-04-25）

> 状态：✅ **已完成**（2026-04-25）
> 关联 issue：[KNOWN_ISSUES.md §1655 "Scheduler LLM 连续失败时无限重试"](../activate/KNOWN_ISSUES.md)
> 触发事件：2026-04-20 LLM 服务器宕机时 scheduler 进入 166+ 次无限重试空转

---

## 1. 背景与触发

### 1.1 2026-04-20 事故现象

手工验证寄生唤醒修复时，LLM 服务器 `192.168.1.117:8080` 恰好关机。启动 agentgo
并提交任何 prompt 后 scheduler 立即进入无限重试：

```
19:34:56  scheduler task a0af6130 发布
19:35:13  重试 #2，恢复  1 条历史
19:35:22  重试 #3，恢复  2 条历史     ← 每次 +1 条
...
19:59:03  重试 #166，恢复 165 条历史   ← 仍在增长，直到 Ctrl+C
```

**全部 99 份 retry trace 文件里 0 次 LLM 成功调用**，统一是 `dial tcp: connect: connection refused`。
用户手动 Ctrl+C 终止，wall-clock ~25 分钟。

### 1.2 表面归因（事后发现是错的）

`internal/scheduler/scheduler.go` 历史注释：

```go
a.MaxRetries = 0  // 不限制——scheduler task 在等待 worker 时不应被 retry 上限杀掉
```

这条注释让"为什么 scheduler 可以无限重试"看起来有合理的 workflow 原因：
scheduler 要等 worker 执行完成才 `report_done`，retry 上限会错误地把"等待时间长"
识别为"失败"。

KNOWN_ISSUES §1655 第一版修复方向据此写成："不触碰 `MaxRetries=0`，另加一个
`MaxLLMFailures` 软上限专门管 LLM 调用层面的失败分类"。

---

## 2. 诊断翻转

### 2.1 系统性核查发现

在准备 `MaxLLMFailures` 实施方案之前做了一次代码通读：

1. `handleFailure` 只在 2 个调用点被触发 —— [agent.go:485](../../internal/agent/agent.go#L485)（execErr != nil）+ [agent.go:523](../../internal/agent/agent.go#L523)（ExpectedArtifacts 校验失败）。scheduler 不用 ExpectedArtifacts，所以它的 handleFailure **只会被真实错误触发**。
2. `waitForBatchTerminal`（[scheduler/executor.go:157](../../internal/scheduler/executor.go#L157)）在**单个 Execute 调用内部**做同步阻塞，`select` 在 `BatchUpdateCh` / `time.After(30s)` / `ctx.Done()` 之间循环。只返回 `nil` 或 `ctx.Err()`，从不返回 `ErrRecoverable`。
3. 也就是说：**scheduler"等 worker"根本不跨 retry 发生，从 Phase 3 引入 waitForBatchTerminal 起就不依赖 MaxRetries=0**。

原注释描述的工作流程 —— "LLM 返回 → 发现 worker 还没完 → 算失败 → RetryRollback 等待" —— 在 Phase 3 重构后已经不存在。

### 2.2 真实根因

**`MaxRetries = 0` 是一条过时的防御代码**。它保护的场景已经不存在，但它顺带把另一个重要场景的防线也关了 —— 网络错误走 `ErrRecoverable` 路径（[llm/client.go:319](../../internal/llm/client.go#L319) 默认 fallback）→ `handleFailure` recoverable 分支 → `if a.MaxRetries > 0 && RetryCount >= MaxRetries` 因 `MaxRetries=0` 短路为 false → 永远不 `terminateTask`。

166 次重试正是这条短路的直接产物。

### 2.3 设计层启示

用户（项目维护者）明确了两个决策：

1. **重试上限是"角色语义"而非"用户偏好"** —— 每种 agent（scheduler / worker / explorer）因为职责不同，合理的重试次数也不同；这不该是 yaml 配置里让用户调的东西。
2. **所有 agent 类型都已经统一在单个 `agent.Agent` 结构体下**，worker / explorer / scheduler 只是包装壳（`*Agent` + 附加字段），所以"每类 agent 自己声明重试预算"的自然实现形态就是**各壳包内部声明常量**。

---

## 3. 修复方案

### 3.1 Phase A：重试预算常量化

| 位置 | 新增常量 | 值 | 替换了什么 |
|---|---|---|---|
| [`internal/worker/worker.go`](../../internal/worker/worker.go) | `workerMaxRetries` | 3 | 原 `a.MaxRetries = cfg.MaxRetry` |
| [`internal/explorer/explorer.go`](../../internal/explorer/explorer.go) | `explorerMaxRetries` | 3 | 原 `a.MaxRetries = cfg.MaxRetry` |
| [`internal/scheduler/scheduler.go`](../../internal/scheduler/scheduler.go) | `schedulerMaxRetries` | 5 | 原硬编码 `a.MaxRetries = 0` |
| [`internal/config/config.go`](../../internal/config/config.go) | — | — | 删除 `MaxRetry` 字段 + DefaultConfig 初始化 |

**保留的契约**：`Agent.MaxRetries = 0` 仍然表示"无限重试"（agent.go:75 注释的既有语义）。scheduler 只是不再使用这个约定。测试文件 `retry_budget_test.go::TestAgent_RecoverableError_MaxRetriesZeroStillRetries` 显式把该契约写入回归锁，防止未来有人把"0 = 零次"的错误语义改进来。

**yaml 侧影响**：`setting.yaml` / `config.example.yaml` 删除 `max_retry: 3` 行；旧 yaml 里若仍有此字段，yaml.Unmarshal / json.Unmarshal 的宽容解析会默默忽略，不影响启动。

### 3.2 Phase B：TransferNote L1 分类分派

代码实施过程中发现 `handleFailure` recoverable 分支有另一个不自知的成本 ——
每次失败**额外调一次 LLM**（[transfer_note.go:82](../../internal/agent/transfer_note.go#L82) 的 L1
`generateTransferNote`）用于生成交接备忘 note。

这在 2026-04-20 场景里意味着：166 次重试 × 每次 1 次 L1 = **332 次无效 network dial**
（而非看上去的 166 次）。

#### 下游 note 读取语义核查

两条读取路径都对空 note 宽容：

| 读路径 | 行为 |
|---|---|
| 下游依赖任务 `GetDependencyTransferNotes` | [memory.go:576](../../internal/store/memory.go#L576) 主动过滤空 note |
| 重试接手者自读 `task.TransferNote` | [agent.go:368](../../internal/agent/agent.go#L368) `if RetryCount > 0 && TransferNote != ""` 跳过 |

所以"某次失败不写 note"不破坏任何下游契约。

#### 按场景分派

| 失败场景 | history 状态 | L1 价值 | L1 成功率 | 决定 |
|---|---|---|---|---|
| Context overflow | 激进压缩剩 1 条 | 极高（唯一保留 reasoning 链的路径）| 高（压缩后能过）| **调 L1** |
| Terminal failure | 不会被重放 | 高（下游 + crashReport 会读）| 中 | **调 L1** |
| 其他 transient（network / 5xx / rate limit / ExpectedArtifacts 失败）| 完整保留 | 低（与 history 高度重复）| 低（LLM 本身就断了）| **只走 L3 mechanical** |

L3 `mechanicalTransferNote` 是纯 Go，零 LLM 调用。transient 路径走 L3 **保留了"永远有 note"的不变量**，只是 note 内容从自然语言降级为结构化 `<transfer-note level="raw">` 标签。

#### 修改后的 handleFailure 骨架

```go
if errors.As(execErr, &recoverable) {
    overflow := isContextOverflow(execErr)
    if overflow { /* aggressive compress */ }

    willTerminate := a.MaxRetries > 0 && task.RetryCount >= a.MaxRetries

    var note string
    if overflow || willTerminate {
        note = a.buildTransferNote(tnCtx, ...)  // L1 → L3 降级链
    } else {
        note = mechanicalTransferNote(...)      // 纯 L3，零 LLM 调用
    }
    if note != "" { a.Store.SetTransferNote(taskID, note) }

    if willTerminate { a.terminateTask(...); return }
    // ... RetryRollback
}
```

---

## 4. 测试改动

### 4.1 新增

| 文件 | 作用 |
|---|---|
| [`internal/agent/retry_budget_test.go`](../../internal/agent/retry_budget_test.go) | `TestAgent_RecoverableError_BoundedByMaxRetries`（2 subtest：scheduler-like 5 / worker-like 3）锁定"MaxRetries>0 必须在有限次后 terminateTask"；`TestAgent_RecoverableError_MaxRetriesZeroStillRetries` 显式文档化 "0=无限" 契约依然合法 |
| [`internal/agent/transfer_note_dispatch_test.go`](../../internal/agent/transfer_note_dispatch_test.go) | 4 个子测试锁定 transient / terminal / overflow / unrecoverable 四条路径的 executor 调用次数契约 |

### 4.2 更新

| 文件 | 改动 |
|---|---|
| [`internal/scheduler/integration_test.go`](../../internal/scheduler/integration_test.go) | 装配断言 `bundle.Agent.MaxRetries == 0` → `== schedulerMaxRetries && > 0` |
| [`internal/agent/p1_fixes_test.go`](../../internal/agent/p1_fixes_test.go) | `TestP1_TransferNoteCtxCarriesAgentMetadata_HandleFailurePath` 改用 overflow 错误触发 L1（普通 transient 不再调 L1） |
| [`internal/config/config_test.go`](../../internal/config/config_test.go) | 清理 8 处 MaxRetry 断言 |
| [`internal/bootstrap/bootstrap_test.go`](../../internal/bootstrap/bootstrap_test.go) / `internal/watchdog/*_test.go` | 清理 3 处 `cfg.MaxRetry = 3` 赋值 |

### 4.3 验证

落地后全量 23 包测试绿：

```
ok  agentgo
ok  agentgo/internal/agent       (含 4 个新分派测试 + 3 个新重试预算测试)
ok  agentgo/internal/bootstrap
ok  agentgo/internal/cli
ok  agentgo/internal/config
ok  agentgo/internal/explorer
ok  agentgo/internal/hook
ok  agentgo/internal/hook/builtin
ok  agentgo/internal/llm
ok  agentgo/internal/mailbox
ok  agentgo/internal/pathutil
ok  agentgo/internal/probe
ok  agentgo/internal/roster
ok  agentgo/internal/scheduler   (integration_test 装配断言更新)
ok  agentgo/internal/session
ok  agentgo/internal/shell
ok  agentgo/internal/store
ok  agentgo/internal/tools
ok  agentgo/internal/tools/schema
ok  agentgo/internal/trace
ok  agentgo/internal/watchdog
ok  agentgo/internal/webtool
ok  agentgo/internal/worker
```

---

## 5. 文档同步

| 文件 | 改动 |
|---|---|
| [`docs/activate/KNOWN_ISSUES.md`](../activate/KNOWN_ISSUES.md) §1655 | 条目状态 `🟡 P1 待修复` → `✅ 已修复`；补修复摘要、回归锁索引、与 v4 §9 的互补关系说明；总览表同步；P1 列表 42/52 计数 +1 |
| [`Archtechture.md`](../../Archtechture.md) | 删除配置项表 `max_retry` 行，补注指向三个壳包常量；`terminateTask` 段 `RetryCount >= MaxRetry` → `>= a.MaxRetries` 并说明语义来源 |
| [`internal/agent/transfer_note.go`](../../internal/agent/transfer_note.go) | 头部文档注释补充 2026-04-25 分派策略 |
| `setting.yaml` / `config.example.yaml` | 删除 `max_retry: 3` 行 |

---

## 6. 影响度量

### 6.1 LLM 调用次数（2026-04-20 场景重放）

| 阶段 | 外层重试次数 | 每次额外 L1 | 总 LLM dial |
|---|---|---|---|
| 修复前 | 166（直到 Ctrl+C） | 1 | **332 次无效调用** |
| 修复后（Phase A 单独） | 5（MaxRetries=5） | 1 | 12 次调用 |
| 修复后（Phase A + B） | 5 | 仅最后 1 次 | **6 次调用** |

**降低约 99% 的无效 LLM 请求**。对按量计费 provider 直接转为成本节约；对 wall-clock 从 ~25 分钟降到秒级。

### 6.2 对健康路径的影响

**零影响**。健康路径（LLM 正常响应、工具调用成功、最终 report_done）不走 `handleFailure`，任何一条新增的常量 / 分派逻辑都不会被触达。

### 6.3 对 overflow 路径的影响

**零影响**。overflow 分支的行为与修复前完全一致：aggressive compress → L1 → L3 降级链。

### 6.4 对终态路径的影响

**note 质量不变**，仍然调 L1 产出自然语言总结。下游依赖任务和 crash report 拿到的内容与修复前一致。

---

## 7. 经验教训

### 7.1 "半硬编码 + 半全局"是一个反模式

修复前的 `cfg.MaxRetry`（可 yaml 配置）+ scheduler `a.MaxRetries = 0`（硬编码）是典型的
"对用户半真半假"配置 —— 用户改 yaml 后只影响 worker/explorer，scheduler 偷偷无视。
修复后三个角色的重试预算都在各壳包常量里，**代码是配置的单一事实源**。

### 7.2 防御式代码需要写为什么，还要写"为什么依然成立"

原 `MaxRetries = 0` 注释写了"为什么这么设"但没写"什么前提下这条还对"。Phase 3 引入
`waitForBatchTerminal` 时没有人重访这条防御。新常量的注释显式写明：
> 历史上此处硬编码为 0 ... 但 Phase 3 引入 `SchedulerExecutor.waitForBatchTerminal` 之后，等 worker 发生在单个 Execute 调用内部的同步阻塞里，不跨 retry——原始理由已过时。

把"依赖条件"写进注释是让未来的人能判断是否还适用的唯一办法。

### 7.3 成本型 bug 容易被功能正确性掩盖

166 次重试在功能上是"安全"的 —— 任务不会被错误地标为完成、数据不会不一致。所以它被归类为 P1 而非 P0。
但在按量计费 LLM 下，这是个钱包漏洞；在交互式使用中，这是个用户体验灾难（25 分钟没反馈）。
**"功能正确但资源浪费"是一类独立维度的 bug**，需要有观测手段（LLM 调用计数 metric / wall-clock 预算）
能单独监控。

### 7.4 分类分派通常比单一兜底更合适

`buildTransferNote` 原来用"先试 L1，不行降级 L3"的**单链降级**思路，看起来很 robust。
但它假设"L1 总是有机会成功" —— 一旦场景明确（LLM 宕机时 L1 一定失败），单链降级就变成"注定的浪费"。
按失败性质分派（而不是按 fallback 顺序执行）是更准确的成本控制工具。

---

## 8. 未来延伸方向（留档，不立项）

- **L2 transfer note 层**：参考 `transfer_note.go` 头部注释里提到的"L2 压缩"占位。目前 L1/L3 二层够用；若未来出现"mechanical 不够精炼、L1 太贵"的中间场景再考虑。
- **跨调用的失败类型 metric**：把 ErrRecoverable 按 cause（network / 5xx / 429 / timeout / context overflow）分类打点。当前诊断 LLM 问题主要靠事后翻 trace，有 metric 能更早发现。
- **Per-agent 失败预算的动态调整**：如连续 3 次 network recoverable 后自动缩短下次的重试间隔，进一步减少"注定失败的重试"的 wall-clock 浪费。目前常量化后调整成本已经很低，按实测压力再说。

---

## 附录：相关文件索引

实施过程中涉及的全部代码文件：

**业务代码**
- [`internal/worker/worker.go`](../../internal/worker/worker.go)
- [`internal/explorer/explorer.go`](../../internal/explorer/explorer.go)
- [`internal/scheduler/scheduler.go`](../../internal/scheduler/scheduler.go)
- [`internal/agent/agent.go`](../../internal/agent/agent.go)（handleFailure）
- [`internal/agent/transfer_note.go`](../../internal/agent/transfer_note.go)（文档）
- [`internal/config/config.go`](../../internal/config/config.go)

**测试**
- [`internal/agent/retry_budget_test.go`](../../internal/agent/retry_budget_test.go)（新）
- [`internal/agent/transfer_note_dispatch_test.go`](../../internal/agent/transfer_note_dispatch_test.go)（新）
- [`internal/scheduler/integration_test.go`](../../internal/scheduler/integration_test.go)
- [`internal/agent/p1_fixes_test.go`](../../internal/agent/p1_fixes_test.go)
- [`internal/config/config_test.go`](../../internal/config/config_test.go)
- [`internal/bootstrap/bootstrap_test.go`](../../internal/bootstrap/bootstrap_test.go)
- [`internal/watchdog/watchdog_test.go`](../../internal/watchdog/watchdog_test.go)
- [`internal/watchdog/crash_report_test.go`](../../internal/watchdog/crash_report_test.go)

**配置与文档**
- `setting.yaml`
- `config.example.yaml`
- [`Archtechture.md`](../../Archtechture.md)
- [`docs/activate/KNOWN_ISSUES.md`](../activate/KNOWN_ISSUES.md)
