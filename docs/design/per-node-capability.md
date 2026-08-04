# Per-node 能力覆盖（publish_task tools / model）设计

> 状态：已实现（2026-07）
>
> 发布面：[`internal/tools/meta.go`](../../internal/tools/meta.go)（publish_task）
>
> 认领面：[`internal/bootstrap/capability_registry.go`](../../internal/bootstrap/capability_registry.go)、[`internal/store`](../../internal/store)（QueryAvailable / CanClaim）
>
> 执行面：[`internal/agent`](../../internal/agent)（processTask / ToolSwapper）
>
> 观测面：[`internal/scheduler/snapshot.go`](../../internal/scheduler/snapshot.go)、[`internal/plan/digest.go`](../../internal/plan/digest.go)、[`internal/trace`](../../internal/trace)
>
> 对照设计：[`interaction.md`](interaction.md)（plan_review 呈现契约）

## 1. 动机

### 1.1 爆炸半径：不要给每个节点火箭筒

kind 级白名单回答的是"这只 agent 一生能用什么工具"；但一次 DAG 执行里，大多数节点只需要其中一小部分。把 `write_file` / `edit_file` / `run_shell` 发给一个只负责读网页汇总的节点，等于给每个节点都配火箭筒（Prefect 的 "bazooka" 论点）：任何一次 prompt 注入或模型误判，爆炸半径都是整个 kind 白名单。节点级 tools 子集把爆炸半径收窄到该节点真正需要的工具。

### 1.2 成本杠杆：机械节点用便宜模型

同一条 DAG 里，"逐文件格式转换"与"裁决两个方案孰优"对模型能力的要求差一个数量级。kind 级模型配置只能整条路由一刀切；节点级 model 覆盖让机械重复节点跑便宜模型、判断密集节点跑旗舰模型，按节点分配 token 预算。

### 1.3 plan_review 从看拓扑升级为看爆炸半径

gate=plan 模式下，用户审批的不再只是"做哪几步、什么顺序"，还包括"每一步拿什么工具、用什么模型"。审批语义从拓扑审查升级为爆炸半径审查，这也是节点能力必须进 board snapshot 与 plan_review 呈现的原因。

## 2. 核心不变式

1. **子集不变式**：节点声明的 tools 必须 ⊆ 认领方 runner 的生效白名单；越界的任务对该 runner 不可认领。
2. **兼容不变式**：不声明 tools/model 的任务（`Capability` 为 nil 或两字段皆空），行为与引入本机制前完全一致，各消费方零开销短路。
3. **当次生效**：覆盖只在认领 Runner 执行该次任务期间生效，不改写 Runner 常驻的工具注册表与模型配置；任务进入终态即恢复。
4. **只缩不扩**：节点级声明永远不能获得 kind 白名单之外的工具——provision 时的 kind 天花板仍是上限。
5. **fail-closed**：任何一环无法验证"⊆"（认领方身份解析不出白名单、executor 不支持按任务过滤、子集 ⊄ executor 注册全集）时拒绝认领或直接失败任务，绝不降级为超集执行。

## 3. 五面架构

| 面 | 机制 |
|---|---|
| 发布 | `publish_task` 增加可选参数 `tools`（逗号分隔工具名子集）与 `model`（模型名），**仅 Scheduler 计划控制面可设置**（`PlanMutationSource=scheduler`，Worker/Reactor 携带即拒绝）；工具名经 `AllToolNames` 静态校验，明显不自洽的伴生组合（写不带读、无执行类工具）只在返回文本里软警告；声明随 Task 持久化为 `model.NodeCapability{Tools, Model}`，并克隆投影到 PlanNode |
| 模型 | Runner 认领后当次以 `Capability.Model` 替换执行模型，任务终态恢复原值；发布面不维护模型清单，模型名是否可用由 LLM 端点决定 |
| 路由 | `QueryAvailable(eventType, agentID)` 按认领方过滤——子集越界的任务对该 runner 不可见；`ClaimTask` 落锁前的 `CanClaim` 守卫叠加同一检查，双保险。白名单事实源：静态 runner 的生效 `AllowedTools` / spawn ad-hoc 继承的 base kind / 动态 Team route 全部 ready listener 的能力交集；identity 无法解析时 fail-closed 拒绝；Scheduler 自身跳过（topo=solo 亲自执行的语义） |
| 执行 | `processTask` 经 `ToolSwapper` 把 executor 的工具注册表换入过滤视图，保证"LLM 只见子集"；executor 不支持按任务过滤或子集 ⊄ 注册全集时任务直接失败（`capability_violation`），不降级执行——这是 Store 预过滤之后的第二道防线 |
| 观测 | Capability 进 board snapshot 的 PlanNode 摘要与 plan_review 呈现；**纳入 GraphDigest**（tools 顺序无关、自动去重，空声明 ≡ 无声明）——能力变化视同图变更，会使旧验收 PASS 作废；发布事件的 trace 记录携带能力声明 |

## 4. 路由饥饿语义

发布面**刻意不做** ⊆ 路由校验：route 集合随 Team provision 动态变化，发布时"没有路由容纳"可能是暂时的。判定全部压在认领侧：

1. 子集越界的任务在 `QueryAvailable` 按认领方过滤时对所有 runner 不可见；即使漏放，`ClaimTask` 落锁前的 `CanClaim` 检查仍 fail-closed 拦下。
2. 任务因此在公告板上滞留。Watchdog 的探测查询无认领方身份（agentID 传空串，跳过能力过滤），路由存在性检查只认 event_type——于是任务落入"有路由但长期无人认领"路径，超过宽限期后发出 `claim_starvation` 告警（**只告警，不 block**）。
3. 告警经事件总线唤醒 Scheduler，由控制面修复：放宽 tools 子集、改投白名单更大的现存 route，或先 provision 能力匹配的 Team 再重新发布。

即能力越界不是一个新的终态失败类别，而是"可被控制面修复的排队饥饿"——这与 event_type 不存在时的 `no_compatible_route` block 刻意区分：后者没有修复路径必须终态化，前者有。

## 5. 与 spawn.RuntimeOverride 的组合关系

两者是正交的两层覆盖：

- `spawn.RuntimeOverride`（`internal/spawn`）是**整只 ad-hoc agent** 的覆盖：SystemPrompt / Model / 重试与上下文预算等，刻意**不可**覆盖 AllowedTools / Profile——保持路由与工具集闭合。认领面因此可以把 spawn ad-hoc 的白名单直接解析为 base kind 的白名单（capability registry 的事实源之一）。
- 节点能力声明是**单个任务节点**的覆盖：tools 只缩（子集语义），model 只换当次。

组合顺序：先由 spawn 从 base_kind 物化一只 ad-hoc Runner（AllowedTools 继承 base_kind），它认领的节点再按 tools 子集当次裁剪。模型维度的生效遵循"就近原则"：节点声明 > spawn override > kind 配置 > 全局默认——每层只在比自己更靠近任务的声明缺省时生效。

## 6. Isolation 字段：挂载点已落地

§6（初版"为 worktree 隔离预留的挂载点"）的预判已实现：`model.NodeCapability` 追加第三个字段 `Isolation *IsolationSpec`，与 Tools/Model 同一容器、同一发布守卫（仅 Scheduler 计划控制面可写）、同一 digest 口径（Mode 空串 ≡ nil）。"子集不变式""当次生效""fail-closed"三条不变式原样平移——隔离声明同样只随当次任务生效，不声明的节点零开销。

目前唯一合法值 `Mode: "workspace"`：认领后该节点在写时复制 overlay 中执行（读穿透主根、写落 `.agentgo/workspaces/<taskID>/`），成功终态由控制面自动合并回主根，冲突 → 任务 failed + 自动 replan 交 Scheduler 裁决。完整设计见 [`workspace-isolation.md`](workspace-isolation.md)。

## 7. 明确不做

- **跨 endpoint 模型覆盖**：`model` 只是模型名，不切换 base_url / api_key——这些在 bootstrap 期与 LLM client 绑定，当次替换只改请求中的 model 字段。需要另一家 endpoint 的模型时，应配置独立 kind 路由，而不是节点覆盖。
- **验收节点 override**：正式验收 Runner 的能力由 verifier route 的 kind 配置决定，不接受节点级 tools/model 覆盖。验收的公正性依赖稳定、可复现的执行环境；"验收该有什么工具"是 AcceptanceSpec 语义的一部分，不应随节点漂移。
- **发布时 ⊆ 路由校验**：见 §4——route 集合动态变化，发布时校验会误拒合法的先发布后 provision 流程；判定集中在认领侧并 fail-closed。
