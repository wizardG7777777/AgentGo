# 按任务写时复制执行隔离（workspace isolation）设计

> 状态：已实现并完成真实运行验证（2026-07-27）
>
> 冻结契约：[`internal/workspace/types.go`](../../internal/workspace/types.go)（Manager / View / Swapper / MergeResult）
>
> 发布面：[`internal/tools/meta.go`](../../internal/tools/meta.go)（publish_task `isolation` 参数）
>
> 执行面：[`internal/runner`](../../internal/runner)、[`internal/tools`](../../internal/tools)（overlay 换入，B 线）
>
> 控制面：[`internal/bootstrap/bootstrap.go`](../../internal/bootstrap/bootstrap.go)（Manager 装配）、[`internal/watchdog`](../../internal/watchdog)（孤儿清扫）、[`internal/reactor/builtin/record_artifact.go`](../../internal/reactor/builtin/record_artifact.go)（产物元数据路径解析）
>
> 姊妹篇：[`per-node-capability.md`](per-node-capability.md)（节点能力声明的 tools/model 维度）

## 1. 动机

### 1.1 并行 fan-out 的写冲突

Scheduler 把一个目标拆成多个并行节点时，节点之间可能写同一批文件（两个节点各自改 `main.go` 的不同函数、批量改写共享的配置文件）。无隔离时后写覆盖先写，且双方都在读-改-写中途看到对方的半成品——失败难以归因，重试互相踩踏。

### 1.2 git worktree 方案的失败教训

2026-04-08 删除的 git worktree 方案证明了什么不能做：

- **git 的锁模型是单用户串行设计**。index.lock / refs 锁假设同一时刻一个操作者；多只 agent 并发操作时锁竞争退化为互相阻塞甚至损坏 index，恰恰是并行场景最不该有的耦合。
- **整树 checkout 摧毁 mtime 观测层**。FileStateCache、L1 历史压缩的变更检测、验收的 file_hash 证据都以主根文件的 mtime+size 为再验证锚点；checkout 整树重写 mtime，观测层集体假阳性失效。
- 隔离要的是**命名空间**（同一文件的 N 份并行副本各归其主），不是**版本控制**（历史、分支、提交语义）。overlay 只复制被写的文件，不动主根一个字节的 mtime。

## 2. 四点决策

1. **声明式触发**：隔离不是全局模式，而是 DAG 节点级声明——Scheduler 在 publish_task 时传 `isolation: "workspace"`，落在 `model.NodeCapability.Isolation`，与 tools/model 同一容器、同一守卫（仅 Scheduler 计划控制面可写）。不声明的节点零开销，行为与引入前完全一致。
2. **自动合并优先，Scheduler 裁决兜底**：任务成功终态由控制面（不经 LLM）把 dirty set 合并回主根——fast-forward 与三路自动合并覆盖绝大多数情形；无法自动解决的冲突不落地、任务置 failed 并自动 RequestReplan，由 Scheduler 看到冲突证据后裁决（改派、串行化或人工上报）。LLM 不参与合并动作本身。
3. **shell 尽力隔离**：`run_shell` 默认工作目录切到 workspace 根，但命令里写主根绝对路径不可完全阻止——**工具写全隔离，shell 尽力隔离**。这是有意接受的残余风险（§7），不为它引入沙箱依赖。
4. **workspace 在 projectRoot 内**：目录固定在 `<projectRoot>/.agentgo/workspaces/<taskID>/`。在根内则路径边界校验（pathutil）、trace、会话归档的天然覆盖范围内，无需第二套边界规则；`.agentgo/` 本就是系统运行目录，与 sessions/traces/state 同级。

## 3. overlay 语义

认领隔离任务的 Runner 在执行期换入 overlay 视图（`workspace.View`，经 per-Runner `Swapper` 换入/恢复）：

- **读穿透主根**：workspace 中已有副本（本任务先前写过）读副本；未命中读主根实时内容——主根在任务执行期的新写入对该任务可见，不存在"快照过期"。
- **写落副本**：`write_file` 的新文件直接落 workspace；`edit_file` 对已有文件先 copy-on-write——从主根复制基线进 workspace，并在 manifest（`.workspace-manifest.json`）记录基线 SHA256，作为合并时的三方之一。
- **路径边界不变**：`pathutil.ValidatePath` 永远面对主根逻辑路径（`Swapper.Get()` 恒返回主根），overlay 解析发生在校验之后——隔离不放宽任何边界。

## 4. 合并协议

任务成功终态，`Manager.MergeTask` 对 dirty set 逐文件合并（合并写入经 roster 逐文件 `TryClaim`，与工具写路径同一占用协议）。单文件结果五种 `FileOutcome`：

| Outcome | 条件 | 动作 |
|---|---|---|
| `fast_forward` | 基线 hash == 主根当前 hash（主根未变） | 直接覆盖主根 |
| `auto_merged` | 双方都变，行区间不相交（Myers 三路合并干净） | 写入合并结果 |
| `new_file` | workspace 新建，主根无同名文件 | 直接落盘 |
| `identical` | 双方内容一致（含同时新建同内容） | 不写入 |
| `conflict` | 行区间相交 / 删除-vs-修改 / 双方新建不同内容 | 不落地 |

`conflict` 的语义是**可裁决的失败**，不是系统故障：`MergeResult.Conflicted=true` 时无冲突文件已落地、冲突文件保证未部分写入；执行面把任务终止为 failed 并自动 RequestReplan（冲突文件、区域、三方哈希随 `workspace_merge_conflict` trace 事件落盘），workspace 保留供排查；Scheduler 在 replan 决策中裁决——典型动作是把冲突节点串行化重派。重试复用既有 workspace（`Materialize` 幂等），不从头再来。

生命周期事件全部落 trace：`workspace_materialized`（认领物化）→ `workspace_merged` / `workspace_merge_conflict`（终态合并）→ `workspace_cleaned`（清理，含 watchdog 孤儿清扫）。

## 5. 逐触点表

| 触点 | 隔离语义 |
|---|---|
| publish_task | 新增可选参数 `isolation`，唯一合法值 `"workspace"`；守卫/校验与 tools/model 同款（仅 Scheduler 计划控制面可写，非法值报错），落 `Task.Capability.Isolation` 并克隆投影到 PlanNode |
| GraphDigest | `Isolation.Mode` 纳入 digest（nil ≡ 空 ≡ Mode 空串）——隔离改变执行边界，视同图变更，旧验收随之失效 |
| pathutil | 边界校验永远面对主根逻辑路径，overlay 解析在校验之后；workspace 目录本身在 projectRoot 内，天然不越界 |
| roster | 执行期写 workspace 副本按 taskID 目录天然互斥，不占主根声明；**合并期**逐文件 `TryClaim` 主根路径，与其他写入方同一协议 |
| FileStateCache | 缓存键为主根逻辑路径，写入后失效语义不变；跨代理写仍靠 mtime+size 再验证兜底（隔离不引入新的失效通道） |
| 验收（file_hash） | 正式验收在合并完成后运行，核验的是主根合并后内容，口径不变 |
| record-artifact reactor | `file_written` 事件 Path 恒为主根逻辑路径；落盘重算 sha256/bytes 前经 `Manager.ResolveForTask(taskID, path)` 解析到物理位置（nil-safe，非隔离任务行为不变） |
| runtime-anomaly reactor | 凭空写入 / 工具错误率检测只消费 ToolCallRecord（工具名+Success），不做文件系统 stat，对隔离与非隔离任务行为一致 |
| trace | 四个新 kind（materialized/merged/merge_conflict/cleaned），`agentgo trace show` 直接可见；publish_task 的 task_published 事件维持 tools/model override 投影 |
| watchdog | 每个巡检周期 `ListOrphans()` 扫描，任务不存在或已终态（`model.IsTerminal`）的目录 `Cleanup` 兜底——崩溃/取消残留不会泄漏磁盘 |
| scheduler prompt | fan-out 并行节点可能写同一批文件时声明 `isolation:"workspace"`；合并自动进行，冲突 replan 回来裁决 |

## 6. 合并冲突的 Scheduler 裁决面

冲突不是终点而是控制面输入。Scheduler 看到的证据链：任务 failed（reason 含冲突文件清单）→ `workspace_merge_conflict` trace（区域数、基线/主根/workspace 三方哈希）→ 自动产生的 ReplanRequest。典型裁决动作按序：把冲突节点改为串行依赖重派（最常用）、缩小节点职责使写集不相交、或 `request_user_input` 上报用户。workspace 在任务终态后保留至 watchdog 清扫窗口，供排查取证。

## 7. 残余风险与后续方向

- **shell 写主根绝对路径**（有意接受）：`run_shell` 默认 cwd 切到 workspace 根，相对路径写自然落副本；但命令显式写主根绝对路径会穿透隔离。缓解只到"尽力"：提示词引导相对路径、合并协议兜住工具写。要彻底封堵需 OS 级沙箱（namespace/容器），当前不引入。
- **读穿透的可见性不对称**：隔离任务读得到主根执行期新内容（含其他并行节点已合并的产出），但其他任务读不到它未合并的副本——这是隔离的本义，但意味着"先产出中间文件供同伴消费"的协作模式必须经合并点，不能靠执行期偷看。
- **后续方向**：workspace 差分随任务结果上报（board snapshot 展示 dirty set 摘要）；冲突区域的 LLM 辅助裁决建议（作为 replan 证据的附件，仍由 Scheduler 决策）；`IsolationSpec` 预留扩展位（如 `Mode: "snapshot"` 只读快照），容器类型无需变更。
