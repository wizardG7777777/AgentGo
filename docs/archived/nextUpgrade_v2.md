# 下一阶段升级计划 v2

---

## 1. 工具系统（Tool System）

> 本章节汇总所有与工具相关的设计与升级计划，涵盖架构重构、核心工具标准化、各类工具的具体改进，以及工具集的管理与治理机制。

---

### 1.1 架构重构前置：ToolGroup 与 11 核心工具标准化

> **状态：✅ 已完成（2026-04-09 验证）**
>
> ToolGroup 接口与 5 个标准 Group 已全部实现，位于 `internal/tools/`：
> - `group.go`（36 行）— `ToolGroup` 接口 + `RegisterGroups` 函数
> - `local_read.go`（501 行）— LocalReadGroup
> - `local_write.go`（263 行）— LocalWriteGroup
> - `web.go`（73 行）— WebGroup
> - `shell.go`（131 行）— ShellGroup
> - `meta.go`（198 行）— MetaGroup
>
> Worker 通过 `tools.RegisterGroups(...)`（`worker.go:124-137`）组合 5 个 Group 完成工具注册。本节作为历史设计记录保留，**不再是待办项**。
>
> **Phase 1 Hook 系统迁移已完成**（2026-04-09，C1-C8 commits），详见 `hookSystem.md` 和 `KNOWN_ISSUES.md` "Hook System 阶段 1 完成"条目。

#### 背景：当前工具系统的问题

代码审计发现工具实现存在以下系统性问题：

- Worker 和 Explorer 对 `read_file / list_files / grep_search` 各自维护一份实现，已出现功能漂移（Explorer 的 `read_file` 缺少 `content_hash`）
- 工具注册逻辑零散分布在 `worker.go` 和 `explorer.go`，没有共享复用机制
- `ToolRegistry` 是纯平铺 `map[string]ToolFunc`，无工具分组、无能力标记，无法按组批量注册或过滤
- 工具 JSON Schema 全部是内联 `map[string]any` 裸字面量，每个工具重复着同样的样板
- 依赖通过闭包随机注入，每个 `make*Tool` 函数签名各不相同

#### ToolGroup 接口

引入 `ToolGroup` 概念，每组工具封装自己的依赖和注册逻辑，代理通过组合 Group 构建工具集，彻底消除当前的复制-粘贴式注册：

```go
// internal/tools/group.go
type ToolGroup interface {
    Register(r *agent.ToolRegistry)
}

func RegisterGroups(r *agent.ToolRegistry, groups ...ToolGroup) {
    for _, g := range groups {
        g.Register(r)
    }
}
```

#### 五个标准 Group

```go
// LocalReadGroup — Worker 和 Explorer 共享，消除当前重复实现
type LocalReadGroup struct {
    Workdir WorkdirProvider
    Cache   *agent.FileStateCache
}
// 包含：read_file（+行范围）、list_dir、grep_search、glob_search

// LocalWriteGroup — 仅 Worker，嵌入 LocalReadGroup 复用依赖
type LocalWriteGroup struct {
    LocalReadGroup                 // embed: 继承 Workdir + Cache
    Roster         roster.Roster   // required: 文件级 TryClaim/Release
    AgentID        string          // required
    Store          store.TaskStore // optional: 写文件成功后追加到 task.Artifacts，nil 时跳过
    ProjectRoot    string          // optional: 用于把绝对路径标准化为项目相对路径
}
// 包含：write_file、edit_file（当前含 pathutil 校验、Roster 文件锁、hash 校验、artifact 记录）
// Phase 1 hook 迁移后：pathutil 保留作标准化、Roster 锁保留、hash 与 artifact 移到 hook，详见 §1.1.5

// WebGroup — Worker 和 Explorer 共享（§1.2 重构后）
type WebGroup struct {
    Provider webtool.SearchProvider
}
// 包含：web_search（+SearchOptions）、web_fetch（+extract_mode）

// ShellGroup — 仅 Worker
type ShellGroup struct {
    TimeoutSec int
    Workdir    WorkdirProvider
    ApprovalCh chan<- shell.ApprovalRequest
    AgentID    string
}
// 包含：run_shell（含黑名单/灰名单拦截）

// MetaGroup — Worker、Explorer 各有变体，Holder=nil 时退化为仅 send_message
type MetaGroup struct {
    Store      store.TaskStore   // publish_task 需要，nil 时不注册
    Holder     TaskHolder        // 深度控制；nil = 无限制（Scheduler 语义）
    MaxDepth   int               // 子任务最大嵌套深度，从 cfg.MaxSubtaskDepth 注入
    MBRegistry *mailbox.Registry
    AgentID    string
}
// 包含：publish_task（统一后）、send_message
```

#### 代理组合示例

```go
// Worker：全量工具集
RegisterGroups(tools,
    LocalReadGroup{workdir, cache},
    LocalWriteGroup{..., roster, agentID},
    WebGroup{searchProvider},
    ShellGroup{cfg.ShellTimeoutSec, workdir, approvalCh, agentID},
    MetaGroup{store, holder, mbRegistry, agentID},
)

// Explorer（全能调查）：只读 + 网络，无写入和 shell
RegisterGroups(tools,
    LocalReadGroup{workdir, cache},
    WebGroup{searchProvider},
    MetaGroup{store, nil, mbRegistry, agentID},
)

// Explorer（纯代码库调查）：仅本地只读
RegisterGroups(tools,
    LocalReadGroup{workdir, cache},
    MetaGroup{nil, nil, mbRegistry, agentID},
)
```

Scheduler 的 `publish_task / cancel_task / report_done / send_message` 继续在 `scheduler.go` 的 `schedulerTools()` 中自行维护，不走 ToolGroup 体系——Scheduler 本身不是任务执行代理，其工具由 `dispatchTool` 直接 switch 处理。

---

#### 11 核心工具标准化定义

经代码审计和需求评估，V2 阶段的核心工具集共 11 个：

| 层 | 工具名 | 归属 Group | V2 变化 |
|----|--------|-----------|---------|
| 文件系统 | `read_file` | LocalReadGroup | 新增 `offset` / `limit` 行范围参数，解决大文件截断问题 |
| 文件系统 | `list_dir` | LocalReadGroup | 重命名自 `list_files`，新增可选 `depth` 参数（树形输出） |
| 文件系统 | `grep_search` | LocalReadGroup | 无变化 |
| 文件系统 | `glob_search` | LocalReadGroup | 无变化 |
| 文件系统 | `edit_file` | LocalWriteGroup | 无变化 |
| 文件系统 | `write_file` | LocalWriteGroup | 无变化 |
| 网络 | `web_search` | WebGroup | SearchOptions 升级，见 §1.2 |
| 网络 | `web_fetch` | WebGroup | extract_mode 升级，见 §1.2 |
| 协作 | `publish_task` | MetaGroup | Worker 的 `publish_subtask` 与 Scheduler 的 `publish_task` **正式合并**，统一工具名 |
| 协作 | `send_message` | MetaGroup | 无变化 |
| 执行 | `run_shell` | ShellGroup | 名称保留，功能无变化 |

**关于 `publish_task` 合并**

原设计 Scheduler 用 `publish_task`、Worker 用 `publish_subtask` 是上一阶段的权宜之策。V2 正式统一：
- LLM 视角：Worker 和 Scheduler 看到相同的工具名，行为描述一致
- 实现视角：深度限制（`MaxSubtaskDepth`）保留在 `MetaGroup.Holder` 逻辑里，`Holder == nil` 时退化为 Scheduler 语义（无深度限制）
- 注册视角：两者注册在各自独立的 `ToolRegistry` 实例上，同名不冲突

**关于特殊代理工具**

Scheduler 探针工具（§1.9）及未来特定代理的专属工具，不计入 11 个核心工具，不走 ToolGroup 体系，由各代理在自身工具注册方法中自行维护。

---

#### 1.1.5 Phase 1 Hook 系统迁移影响（2026-04-09 增补，✅ 已完成）

> **状态：✅ 已完成（2026-04-09，C1-C8 commits）**
>
> 详见 `hookSystem.md` 和 `KNOWN_ISSUES.md` "Hook System 阶段 1 完成"条目。
> 8 个 commits 落地：C1(Store.ToolCallRecord) → C8(RequireReadBeforeWriteHook)。

详细的 Hook System 设计见 `hookSystem.md`。本节只列出 Phase 1 实施时触动 ToolGroup 内工具实现的具体改动。

**ToolGroup 接口与 5 个 Group 的结构体定义不变**——hook 系统是运行时层，ToolGroup 是注册期层，两者正交。改动只发生在 Group 内的具体工具实现函数（如 `LocalWriteGroup.writeFile` 函数体）。

##### LocalWriteGroup 内的改动（已完成）

| 原有逻辑 | Phase 1 迁移后 | 对应 hook |
|---|---|---|
| `pathutil.ValidatePath(path, projectRoot)` 校验 + 标准化 | **保留**调用做标准化，校验由 hook 接管 | `PathBoundaryHook` |
| `Roster.TryClaim` + `defer Release` | **保留**，不在 Phase 1 迁移范围 | — |
| `expectedHash` SHA256 校验段 | **已迁移**到 `ValidateExpectedHashHook` | `ValidateExpectedHashHook` |
| `recordArtifact` 调用 | **已迁移**到 `RecordArtifactHook` | `RecordArtifactHook` |
| 实际 `os.WriteFile` / 缓存失效 | 保留 | — |

**字段变更**：

- `LocalWriteGroup.Store` 和 `LocalWriteGroup.ProjectRoot` 字段在迁移完成后**应当删除**（当前可能仍有引用，需 grep 确认）
- `LocalWriteGroup.Roster` 和 `LocalWriteGroup.AgentID` 字段保留（Roster 锁不在 Phase 1 迁移范围）

##### 其他 Group 内的改动（已完成）

- **LocalReadGroup**：`pathutil.ValidatePath` 保留标准化，校验由 `PathBoundaryHook` 接管
- **ShellGroup**：同上处理
- **WebGroup / MetaGroup**：不涉及文件路径，Phase 1 不触动

##### 关键决策记录（已在 hookSystem.md 中记录）

- **决策 A1（PathBoundaryHook 双重校验）**：Hook 做校验，工具内部保留标准化
- **决策 B1（ValidateExpectedHashHook 接受微秒级 TOCTOU 窗口）**：Hash 校验移到锁外

##### 不在 Phase 1 范围

- **Roster 文件锁不迁移**：保留在 `LocalWriteGroup.writeFile` 内
- **Scheduler 工具不被 hook 影响**：Scheduler 工具不走 `agent.NewLLMExecutor`

---

### 1.2 Web 工具重构与 Explorer 网络调查能力补全

> **状态：✅ 已完成（2026-04-08）**
>
> 4 个改动已全部实现：
> - SearchResult 扩展（PublishedAt、Source、Score）
> - SearchProvider 接口升级（SearchOptions 参数）
> - web_search 参数扩展（max_results、time_range）
> - web_fetch 提取模式（extract_mode: auto/article/full）
> - Explorer 通过 WebGroup 复用网络工具
>
> 各后端支持情况：Tavily/SearXNG 全功能支持；Serper 支持 NumResults/TimeRange；DuckDuckGo 仅支持 NumResults（TimeRange 有日志警告）。

#### 现状诊断

当前 `web_search` 和 `web_fetch` 的实现存在四个结构性缺陷，直接导致了网络调查中的幻觉问题：

| 缺陷 | 位置 | 影响 |
|------|------|------|
| `SearchResult` 缺少时效性元数据 | `webtool/webtool.go` | LLM 无法判断信息新旧，用"听起来合理"的旧信息填补空白 |
| `SearchProvider.Search()` 只接受 `query` 字符串 | `webtool/provider.go` | 参数扩展被截断在注册层，无法传递给后端 API |
| `web_fetch` 使用正则剥离 HTML | `webtool/webtool.go ExtractText` | 导航栏、广告、页脚噪音混入正文，10000 字符预算快速消耗 |
| web 工具注册耦合在 `worker.go` | `worker.go registerWorkerTools` | Explorer 无法复用，强行复制会带来同样的缺陷 |

---

**改动一：扩展 `SearchResult` 结构体**

```go
// webtool/webtool.go
type SearchResult struct {
    Title       string
    URL         string
    Snippet     string
    PublishedAt string  // 新增：发布时间（RFC3339 或空串，后端不支持时留空）
    Source      string  // 新增：来源域名（如 "techcrunch.com"）
    Score       float64 // 新增：相关性分数（0~1，后端不支持时为 0）
}
```

`FormatResults` 同步更新，当 `PublishedAt` 或 `Source` 非空时在输出中追加：

```
1. Claude Code 源代码泄漏事件分析
   https://example.com/article
   [来源: example.com | 2026-03-31]
   约 512,000 行 TypeScript 代码通过 npm source map 泄漏...
```

各后端按能力填充：Tavily / Serper 可返回发布日期，DuckDuckGo / SearXNG 通常返回空串。

---

**改动二：`SearchProvider` 接口升级 + `web_search` 工具参数扩展**

当前接口只接受 `query` 字符串，即使注册层加了参数也无法传递给后端。升级后：

```go
// webtool/provider.go
type SearchOptions struct {
    NumResults int            // 返回结果数，默认 5，最大 10
    TimeRange  string         // "any" | "day" | "week" | "month" | "year"，默认 "any"
    Extra      map[string]any // 后端特定扩展参数，向前兼容
}

type SearchProvider interface {
    Search(ctx context.Context, query string, opts *SearchOptions) ([]SearchResult, error)
    Name() string
}
```

`opts` 传 `nil` 时各后端使用自身默认值，保持向后兼容。

各后端参数映射：

| 后端 | NumResults | TimeRange |
|------|-----------|-----------|
| Tavily | `max_results` | `days`（day=1, week=7, month=30, year=365） |
| Serper | `num` | `tbs=qdr:d/w/m/y` |
| SearXNG | `pageno` 控制深度 | `time_range` |
| DuckDuckGo HTML | 截断结果列表 | 不支持（忽略并记录警告） |

`web_search` 工具参数同步扩展：

```go
"max_results": {"type": "integer", "description": "返回结果数量，默认 5，最大 10"}
"time_range":  {"type": "string", "enum": ["any", "day", "week", "month", "year"],
                "description": "时间范围过滤，默认 any。调查近期事件时建议设为 week 或 month"}
```

---

**改动三：`web_fetch` 提升内容提取质量**

新增 `extract_mode` 参数（可选，默认 `"auto"`）：

```go
"extract_mode": {"type": "string", "enum": ["auto", "article", "full"],
                 "description": "auto=智能判断；article=只提取正文（过滤导航/页脚噪音）；full=全页面文本"}
```

`article` 模式实现：优先提取 `<article>`、`<main>`、`role="main"` 区域内的文本；找不到语义标签时回退到 `full` 模式。同时返回结构化前缀：

```
[标题] Claude Code Source Code Leaked via npm
[发布] 2026-03-31
[来源] techcrunch.com
---
正文内容...
```

---

**改动四：提取 `RegisterWebTools` 为包级共享函数（对应 §1.1 WebGroup）**

将 `worker.go` 中内嵌的 web 工具注册逻辑提取到 `webtool` 包，作为 `WebGroup.Register()` 的实现基础：

```go
// webtool/register.go（新文件）
func RegisterWebTools(tools ToolRegistry, provider SearchProvider) {
    // 注册 web_search（含 SearchOptions 参数）
    // 注册 web_fetch（含 extract_mode）
}
```

Explorer system prompt 同步补充网络调查约束：
- 调查结论必须标注来源 URL `[来源: URL | 日期]`
- 无法找到来源的 claim 显式标注 `[未验证]`，不以确定口吻呈现
- 优先使用 `time_range: "week"` 或 `"month"` 搜索近期事件
- 至少执行 3 次不同关键词的独立 `web_search` 后再汇总

**注意**：Explorer 注册 web 工具后仍保持只读约束——不注册 `write_file / edit_file / run_shell`。

---

**远景备注：FileProvider 统一抽象**

未来可引入 `FileProvider` 接口，将本地文件系统实现为 `LocalFileProvider`，为 S3、SFTP 等远程存储留出扩展点。**暂不实施**：当前无远程文件存储需求，为单一实现做接口抽象是过度设计，待出现第二种文件后端时再引入。

---

### 1.3 工具集分层配置（Tool Set Profiles）

**现状**：工具集与代理类型硬绑定，无法按任务场景裁剪。

**改进方向**：

在 `Config` 中引入具名工具集配置，Bootstrap 按配置文件初始化各代理的 ToolRegistry：

```yaml
tool_profiles:
  worker_standard:        # 标准执行代理：代码修改 + 网络
    - read_file
    - list_dir
    - grep_search
    - glob_search
    - write_file
    - edit_file
    - run_shell
    - web_search
    - web_fetch
    - publish_task
    - send_message
  explorer_codebase:      # 代码库调查：本地只读
    - read_file
    - list_dir
    - grep_search
    - glob_search
    - send_message
  explorer_web:           # 网络调查：网络只读
    - web_search
    - web_fetch
    - send_message
  explorer_full:          # 全能调查：本地 + 网络只读
    - read_file
    - list_dir
    - grep_search
    - glob_search
    - web_search
    - web_fetch
    - send_message
```

**与 §1.4 分级权限模型的关系**：工具集配置文件是 PermissionMode 的前置基础设施——运行时动态裁剪需要先有静态的命名工具集，才能做"降级到 readonly profile"等操作。建议先实现工具集配置文件，再在此基础上实现 §1.4 的动态权限提升。

---

### 1.4 分级权限模型（PermissionMode）

**Why：** 对标 Claude Code 的 `permissionMode` 机制——不同任务的风险等级不同，"搜索调研"任务不应持有 `write_file / run_shell`，"代码修改"任务不应持有 `web_fetch`。工具集与任务风险不匹配会放大 LLM 幻觉导致的破坏面。

**改进方向**：
- **任务级工具裁剪**：`Task` 结构体新增 `AllowedTools []string` 和/或 `DisallowedTools []string` 字段，Scheduler 在 `publish_task` 时指定。Agent 在 `processTask` 开始时根据任务声明动态过滤 ToolRegistry
- **预设权限模板**：定义命名权限等级（如 `readonly`、`standard`、`privileged`），Scheduler 通过模板名快速指定，无需逐个列举工具
- **运行时权限提升**：Agent 执行中发现需要额外工具时，通过 `permission_request` 协议向 Scheduler 申请临时提权，Scheduler 审批后动态注入工具

**暂不实施的原因**：MVP 阶段 Worker 数量少，任务由 Scheduler 中心化分配，风险可控。Shell 命令拦截已提供基础安全屏障。依赖 §1.3 工具集配置化先落地。

---

### 1.5 Shell 命令拦截配置化

> **状态：✅ 已完成（2026-04-10）**
>
> 已实现配置化名单和项目级规则覆盖。会话级放行记忆和白名单模式未实现（按需求排除）。

**实现内容**：

1. **配置化名单**：`Config` 新增 `ShellBlacklist` 和 `ShellGreylist`，用户可在 `setting.yaml` 中追加自定义规则
2. **项目级规则覆盖**：支持 `.agentgo/project_rules.yaml`，通过 `shell_rules.blacklist.add/remove` 和 `shell_rules.greylist.add/remove` 语法覆盖全局配置
3. **规则合并逻辑**：默认规则 → 全局自定义 → 项目级 add → 项目级 remove

**配置示例**：

```yaml
# setting.yaml - 全局配置
shell_blacklist:
  - "dangerous_command.*"
shell_greylist:
  - "docker\\s+run"
  - "kubectl\\s+apply"
```

```yaml
# .agentgo/project_rules.yaml - 项目级覆盖
shell_rules:
  blacklist:
    add:
      - "npm\\s+publish"     # 本项目禁止直接发布
    remove:
      - "shutdown"            # 嵌入式项目可能需要关机
  greylist:
    add:
      - "make\\s+deploy"      # 本项目部署需要审批
    remove:
      - "git\\s+push"         # 本项目允许自由推送
```

**实现文件**：
- `internal/config/config.go` - 新增配置字段
- `internal/shell/rules.go` - 规则加载与合并逻辑（105 行）
- `internal/shell/rules_test.go` - 测试覆盖（94.4% 语句覆盖，14 个测试用例）
- `internal/worker/worker.go` - 使用 BuildFilter 替代硬编码
- `.agentgo/project_rules.yaml.example` - 示例配置文件

**测试覆盖**：
- `TestMergeRules_*` - 规则合并逻辑（基础、空值、去重、优先级）
- `TestLoadProjectRules_*` - 项目规则加载（文件不存在、有效文件、无效 YAML）
- `TestBuildFilter_*` - 完整过滤器构建（无规则、含项目规则、含灰名单、错误处理）

---

### 1.6 管理员信赖标记（SourceAdminTrusted）

**Why：** 对标 Claude Code 的 `isSourceAdminTrusted` 机制——当系统未来支持用户自定义代理或外部插件代理时，需要区分"可信来源"和"不可信来源"，限制不可信代理的工具访问范围。

**改进方向**：
- **代理来源标记**：Agent 结构体新增 `Source string`（如 `"system"`、`"user"`、`"plugin"`）和 `Trusted bool` 字段
- **信任级别与工具映射**：不可信代理自动降级为只读工具集，且不注入 mailbox 的 `send_message` 工具（防止向其他代理注入恶意指令）
- **配合分级权限模型**：信任标记作为权限模板选择的输入之一

**暂不实施的原因**：当前无外部代理接入机制，所有代理由 Bootstrap 内建创建。待引入插件体系或用户自定义 Agent 后再实施。

---

### 1.7 代理能力声明与 Scheduler 路由感知

**现状**：Scheduler 的 `publish_task` 工具只有 `event_type` 字段区分代理类型，无法知道"当前 Explorer 是否有 web 工具"、"Worker 是否适合某类任务"。

**阶段一：静态能力声明**

在 `boardSnapshot` 的 `resources` 字段中追加每类代理的能力标签：

```json
"resources": {
  "worker_count": 2,
  "busy_workers": 1,
  "available_workers": 1,
  "agent_capabilities": {
    "worker": ["code_edit", "shell_exec", "web_search", "subtask_publish"],
    "explorer": ["codebase_read", "web_search"]
  }
}
```

Scheduler system prompt 更新路由指引：发布任务时参考 `agent_capabilities`，按实际能力而非约定俗成的 `event_type` 语义做决策。

**阶段二：任务级能力需求声明**

`publish_task` 工具新增 `required_capabilities` 参数，由 `ClaimTask` 逻辑在代理认领时做能力匹配校验，不满足的代理无法认领该任务：

```go
if !agent.HasCapabilities(task.RequiredCapabilities) {
    return ErrCapabilityMismatch
}
```

---

### 1.8 工具可用性探针（Tool Health Check）

**现状**：系统启动时不验证工具是否可用。若 `search_api_provider` 配置为 `searxng` 但实例未启动，Worker 在执行 `web_search` 时才会报错，已经消耗了一轮 LLM 调用。

**改进方向**：

Bootstrap 阶段新增工具可用性探针，在代理启动前主动检测，失败时降级运行而非崩溃：

```go
checks := []ToolHealthCheck{
    {Name: "web_search", Check: probeSearchProvider(cfg)},
    {Name: "web_fetch",  Check: probeHTTPReachability()},
}
for _, c := range checks {
    if err := c.Check(); err != nil {
        log.Printf("[警告] 工具 %s 不可用: %v，相关代理将降级运行", c.Name, err)
        // 从对应工具集中移除该工具，而非直接启动失败
    }
}
```

探针结果写入 `boardSnapshot`，让 Scheduler 知道"当前 `web_search` 不可用，不要发布依赖网络搜索的任务"。

---

### 1.9 调度器专属探针工具（Scheduler Probe Tools）

**Why：** Scheduler 目前只有 4 个工具，对外部环境完全不可见。做任务分解时，依据的只有用户的自然语言描述——目标目录有多少个文件、规模有多大，一概不知。即便 system prompt 要求并行拆分，Scheduler 也因缺乏结构性信息而保守地发布一个覆盖全部的单任务。

这些工具属于 Scheduler 专属，不计入 §1.1 的 11 个核心工具，不走 ToolGroup 体系。

#### 轻量探针工具集

```go
// 仅在 Scheduler.schedulerTools() 中注册
{
    Name: "list_directory",
    Description: "列出指定目录下的文件和子目录，返回名称、类型、大小",
}
{
    Name: "count_files",
    Description: "统计指定目录中匹配 glob 模式的文件数量",
}
{
    Name: "file_summary",
    Description: "返回文件的元数据：大小、行数、最后修改时间",
}
```

**实现要点**：
- 所有工具严格只读，路径安全校验复用 `pathutil.ValidatePath`，限制在 `cfg.ProjectRoot` 内
- 工具调用结果只注入 Scheduler 的 `history`，不发布到公告板

#### "先侦查后分配"的调度模式

在 Scheduler system prompt 中追加：

> 在分配涉及文件/目录的任务前，优先使用 `list_directory` 或 `count_files` 工具感知目标规模：
> - 目标目录中有 N 个文件 → 发布 N 个并行 explore 任务，每个任务负责一个文件
> - 单个文件超过 500 行 → 考虑按章节/模块拆分为多个子任务
> - 目标不涉及本地文件（如纯网络调查）→ 跳过侦查，直接按语义子方向拆分

这与 §1.7 能力声明形成互补：能力声明解决"**知道谁能做**"，探针工具解决"**知道要做多少**"。

#### 实施优先级

| 优先级 | 内容 |
|--------|------|
| P0 | `list_directory`（最高频使用场景） |
| P1 | `count_files` |
| P2 | `file_summary` + "先侦查后分配"调度模式 |

**暂不实施的原因**：需先验证现有 prompt 修改的基线效果，再叠加探针工具，便于隔离变量判断每项改动的实际贡献。

---

### 工具系统实施优先级汇总

| 优先级 | 子项 | 依赖 | 状态 |
|--------|------|------|------|
| P0 | §1.1 ToolGroup 架构重构 | 无 | ✅ 已完成 |
| P0 | §1.2 Web 工具重构 + Explorer 补全 web 工具 | §1.1 | ✅ 已完成 |
| P0 | §1.9 `list_directory` 探针工具 | 无 | 📝 待实现 |
| P1 | §1.3 工具集分层配置（YAML profiles） | §1.1 | 📝 待实现 |
| P1 | §1.9 `count_files` 探针工具 | 无 | 📝 待实现 |
| P2 | §1.8 工具可用性探针 | §1.3 | 📝 待实现 |
| P2 | §1.7 能力声明阶段一（静态） | §1.3 | 📝 待实现 |
| P2 | §1.9 `file_summary` + 调度模式 | §1.9 P0/P1 | 📝 待实现 |
| P3 | §1.5 Shell 命令拦截配置化 | 无 | ✅ 已完成 |
| P3 | §1.4 分级权限模型 | §1.3 | 📝 待实现 |
| P4 | §1.7 能力声明阶段二（任务级匹配） | §1.7 阶段一 + §1.4 | 📝 待实现 |
| P4 | §1.6 管理员信赖标记 | 待引入外部代理后 | 📝 待实现 |

---

### 1.10 任务产出物（Artifacts）基础设施 ✅

> **状态：✅ 已完成（2026-04-08）**
>
> 基础设施（Task.Artifacts、ExpectedArtifacts、Store.AppendArtifact、自动注入下游 prompt、ExpectedArtifacts 校验）已在 KNOWN_ISSUES 中"Worker 凭空捏造"和"Worker 任务无文件产出"的修复中落地。
> 本节仅记录 **持久化未完成** 的工作。

**当前状态（in-memory）**：
- `model.Task.Artifacts []string`：任务执行期间所有 `write_file` / `edit_file` 写入的文件路径（去重，相对项目根）
- `model.Task.ExpectedArtifacts []string`：发布者声明的预期产出清单（任务结束时校验）
- 由 `MemoryTaskStore` 在内存里维护
- 进程重启后 artifacts 信息全部丢失

**为什么需要持久化**：
- 故障恢复：进程崩溃重启后，无法判断"上一次任务实际产出了什么文件"，依赖关系断裂
- 历史审计：长期运行系统需要查询"任务 X 在 N 天前产出了什么"
- 跨会话引用：用户在新会话中希望基于历史任务的产出做后续工作
- 与 trace 系统的关系：trace 已经记录了 `file_written` 事件，但 trace 是观察通道，不是权威状态

**实施方向**：

1. **方案 A：与 TaskStore 整体持久化绑定**
   - 当前 TaskStore 是纯 in-memory（`NewMemoryTaskStore`）
   - 引入 `PersistentTaskStore`（基于 SQLite 或 BoltDB），把所有 Task 字段（含 Artifacts、ExpectedArtifacts）一起持久化
   - 优点：架构干净，artifacts 跟随任务自然持久化
   - 缺点：需要先做 TaskStore 持久化整体改造，工作量大

2. **方案 B：Artifacts 单独持久化（轻量过渡）**
   - 在 `.agentgo/artifacts.jsonl` 写入 append-only 日志
   - 每次 `AppendArtifact` 同步写入一行 `{task_id, path, hash, ts}`
   - 启动时回放重建内存索引
   - 优点：实现简单、不依赖 TaskStore 改造
   - 缺点：与 Task 主存储分离，可能不一致

3. **方案 C：复用 trace 系统**
   - trace 已经持久化了 `file_written` 事件
   - 启动时扫描 `.agentgo/traces/*.jsonl` 重建 artifacts 索引
   - 优点：零新增基础设施
   - 缺点：trace 是 100 个任务滚动 GC 的，老任务的 artifacts 会丢失；语义混乱（trace 兼任了状态存储）

**推荐**：方案 A，与 TaskStore 整体持久化一并做。在那之前，方案 C 作为临时兜底（如果故障恢复需求紧急）。

**优先级**：P1。当前 in-memory 已经能解决"凭空捏造"和"report-only 失败"两大 P0 问题，持久化是体验优化层面的需求。

**关联工作**：TaskStore 持久化是更大的话题，应当统一考虑：
- Task 状态（pending/processing/completed/failed）
- Task 历史（含 RetryCount、LastHistory）
- Mailbox 邮件
- Roster 文件锁
- Artifacts（本节）

建议作为下一阶段的"持久化与故障恢复"专题统一规划。

---

## 2. Git Worktree 隔离设计考量

> **状态：❌ 已作废（2026-04-09）**
>
> Git worktree 子系统已于 2026-04-09 整体删除，详见 `KNOWN_ISSUES.md` "架构决策：删除 git 依赖"。
> 本节保留作为历史记录，**不再是待办项**。

### 2.1 Worktree 是否应纳入公告板机制

当前实现中，worktree 的创建和管理在 Agent 层（OnTaskStart/OnTaskEnd 回调），公告板（TaskStore）仅通过 `Task.WorktreePath` 记录路径。

**纳入公告板的潜在好处**：
- Scheduler 在 `boardSnapshot` 中看到每个任务的 worktree 路径，可以更智能地分配任务（如避免两个任务修改同一模块）
- Watchdog 可以监控 worktree 状态（是否存在残留、磁盘占用等）

**不纳入的理由**：
- worktree 是物理隔离的实现细节，对调度决策的影响有限
- 公告板接口（TaskStore）应保持简洁，不承担文件系统管理职责

**当前决策**：不纳入。如果未来证明 Scheduler 需要 worktree 信息来优化任务分配，可以在 `boardSnapshot` 中追加 `worktree_path` 字段，而无需修改 TaskStore 接口。

### 2.2 Per-Task vs Per-Agent 粒度评判标准

| 维度 | Per-Task | Per-Agent |
|------|----------|-----------|
| 隔离粒度 | 每个任务完全干净的环境 | 同一 Agent 的连续任务共享环境 |
| I/O 开销 | 每次任务创建/销毁 worktree | 仅 Bootstrap/Shutdown 时 |
| 任务间累积副作用 | 无（每次 clean slate） | 有（前一任务的临时文件影响后续） |
| 合并频率 | 每个任务完成后合并 | Agent 生命周期结束时合并 |
| 冲突概率 | 较高（频繁合并） | 较低（集中合并） |
| 适用场景 | 高并发、任务间无关联 | 低并发、任务间有顺序依赖 |

**当前决策**：Per-Task。原因：当前系统设计鼓励 Scheduler 发布独立无依赖的子任务，Per-Task 粒度与此理念一致。如果未来观察到 worktree 创建/销毁的 I/O 成为瓶颈，可回退到 Per-Agent。

---

## 3. 代理系统与稳定性改进

### 3.1 ConflictResolver 崩溃恢复

> **状态：❌ 已作废（2026-04-09）**
>
> Git worktree 删除后，ConflictResolver 子系统已不存在。多代理文件冲突解决需在新架构下重新设计。
> 本节保留作为历史记录，**不再是待办项**。

冲突处理代理有可能崩溃（LLM 超时、panic 等），这会导致：
- 冲突不能被正确解决
- 等待 DoneCh 的 Agent 永久阻塞（当前有 180s Resolver 侧超时保护，但超时后仅记录日志+放弃合并）

**改进方向**：
- 类似 Watchdog 的 `runWithRecover` 模式，ConflictResolver 崩溃后自动重启
- 未完成的 ConflictRequest 在重启后重新处理（需要持久化请求队列或重放机制）
- 连续崩溃 N 次后降级：放弃自动合并，通知用户手动处理

### 3.2 冲突代理间互相避免

> **状态：🔄 需重新设计（2026-04-09）**
>
> Git worktree 删除后，原有的冲突避免机制失效。
> 当前系统**故意暴露**以下退化："两个 worker 并发写同一文件 → 后写覆盖前写"。
> 详见 `KNOWN_ISSUES.md` "多代理协同重建"条目。

两个 Agent 发生 worktree 冲突说明它们的任务存在文件级竞争。事后解决不如事前避免：

**改进方向**：
- **基于 Roster 的预防**：扩展 Roster 从"文件写锁"升级为"文件意图声明"——Agent 在修改文件前声明意图，其他 Agent 的 Scheduler 可以看到声明并避免分配涉及同一文件的任务
- **基于 mailbox 的协调**：Agent 发现冲突风险时，通过 send_message 通知对方 Agent 协商分工
- **Scheduler 层面**：在 `boardSnapshot` 中暴露各 Agent 正在修改的文件列表（来自 Roster），让 LLM 在任务分配时主动避开冲突

### 3.3 Explorer 权限强化

> **状态：✅ 已完成（2026-04-08）**
>
> 双端硬约束已实现：
> - Explorer 不注册 write_file/edit_file/run_shell 工具
> - Scheduler 和 MetaGroup 双端拒绝 `event_type=explore && expected_artifacts != nil`
> 详见 `KNOWN_ISSUES.md` "Explorer 越权 expected_artifacts"条目。

当前 Explorer 在 worktree 中的"只读"限制仅靠 system prompt 提示，LLM 可能无视。

**改进方向**：
- **工具层面**：Explorer 不注册 write_file/edit_file/run_shell 工具（当前已是如此），但 worktree 本身不阻止 LLM 通过其他方式修改文件
- **文件系统层面**：将 Explorer 的 worktree 挂载为只读（`git worktree add` 后执行 `chmod -R a-w`），从 OS 层面阻止写入
- **沙箱机制**：未来引入容器级或 chroot 级沙箱，为不同权限等级的 Agent 提供硬隔离

### 3.4 ConflictResolver 独立模型配置

> **状态：❌ 已作废（2026-04-09）**
>
> ConflictResolver 随 worktree 一起删除。

当前 ConflictResolver 使用 `cfg.ExplorerModel`。未来应新增独立配置项：
```yaml
resolver_model: "gpt-4o"  # 冲突处理代理专用模型
```
冲突解决需要理解两方代码意图并做出正确取舍，可能需要比 Explorer 更强的推理能力。

### 3.5 Agent 休眠/唤醒优化（Suspend/Resume）

**当前状态**：已由现有机制部分覆盖，不构成当前规模下的实际问题。

**现有机制的局限**：
- Agent 空闲时仍在忙等待（每 500ms 扫描一次 store），在 1-3 个 Worker 的 MVP 规模下 CPU 开销可忽略
- 如果扩展到 20+ Worker，每个每 500ms 都扫描 store 并竞争 claim，会成为不必要的开销

**未来改进方向**（待规模增长后实施）：
- 用 `sync.Cond` 或专用 channel 替代定时轮询：TaskStore 在 `PublishTask` 时 broadcast 通知，空闲 Agent 立即唤醒
- 动态调整 PollInterval：空闲时逐步增大间隔（1s → 2s → 5s），有任务时重置为 500ms

**暂不实施的原因**：当前 WorkerCount 默认为 1，500ms 轮询的 CPU 开销在微秒级，远不构成瓶颈。

### 3.6 代理间通信防循环机制（Anti-Loop Guard）

> **状态：✅ 已完成（2026-04-09，Phase 2）**
>
> 4 项根因全部关闭：
> - 根因 #1：PerAgentDedupHook（D4 镜像防御）
> - 根因 #2：ChainDepthLimitHook + mailbox.Message.ChainDepth
> - 根因 #3：WakeContextExpandHook + MailboxHookView
> - 根因 #4：Worker/Explorer prompt 弱化"应回复"为"可以忽略"
> 详见 `KNOWN_ISSUES.md` "邮件级联爆炸"条目。

当前邮箱系统允许代理之间无限来回通信，`question → reply → question → reply` 循环是引入反问机制后的必然副作用。

**解决方案（分两个阶段）**：

#### 阶段一：硬性来回上限

在邮箱系统中引入会话追踪，按 `(agentA, agentB)` 配对记录来回次数，最多允许 2 个来回（共 4 条消息）。超限后强制收敛：代理 B 直接拒绝执行，向代理 A 发送拒绝原因，邮箱系统在代码层阻止后续消息（非 prompt 依赖）。

**实现要点**：
- `Registry` 维护 `conversationCount map[[2]string]int`，按发送方+接收方排序的配对键计数
- `Send()` 中检查计数，超限时返回错误
- 计数在任务边界重置

#### 阶段二：仲裁代理（进阶版本）

对于超过 2 个来回仍无法达成一致的通信，引入仲裁代理（Arbitrator）进行裁决：
1. 邮箱系统自动收集对话全文和双方任务描述
2. 仲裁代理（独立 LLM 调用，复用 ConflictResolver 模式）给出最终裁决
3. 裁决以 `type="steer", from="arbitrator"` 送达，不可递归，仲裁流程不超过 1 轮 LLM 调用

**暂不实施的原因**：当前代理间通信频率低，反问机制刚引入，尚未观测到实际循环问题。阶段一可作为快速止血方案，阶段二在多代理自组织落地后更有价值。

### 3.7 团队感知系统（Team Awareness）

**当前已实现（MVP 基线）**：`BuildTeamSnapshot` 在任务开始时注入 `<team-snapshot>` XML，包含队友 ID、忙碌/空闲状态、正在执行的任务描述（截断 80 字）。

**未来升级方向**：

#### 3.7.1 动态刷新

当前快照仅在任务开始时注入一次。改进：
- **定期刷新**：每 N 轮 ReAct 循环重新生成快照并注入（如每 10 轮或 5 分钟）
- **事件驱动刷新**：收到 `type="ack"` 或 `type="info"` 消息时，下一轮自动刷新

#### 3.7.2 角色与技能描述

- **Agent 角色标签**：Agent 结构体新增 `Role string`（如 `"code-writer"`、`"investigator"`），在 Bootstrap 时配置
- **快照渲染角色**：`<team-snapshot>` 中展示角色标签，帮助代理判断该联系谁

#### 3.7.3 任务关联感知

- **文件级关联**：快照中标注队友正在修改的文件列表（来自 Roster）
- **依赖关联**：队友任务是当前任务的前置或后置依赖时特别标记
- **共享上下文**：队友任务的 `PartialOutput` 摘要纳入快照

#### 3.7.4 从感知到自治

- **自发任务分流**：Worker 发现任务过大，主动联系空闲队友请求协助
- **冲突预防**：Worker 看到队友正在修改同一文件，主动协商分工顺序
- **进度同步**：长任务中周期性向相关队友广播进展

**这些改进依赖**：结构化消息类型（已实现）、防循环机制（§3.6，✅ 已完成）、分级权限模型（§1.4，待实现）。建议按 3.7.1 → 3.7.2 → 3.7.3 → 3.7.4 的顺序逐步推进。

### 3.8 Session 化日志与状态持久化

**Why：** 当前日志输出到控制台且无持久化归档，任务历史随进程结束而丢失。用户无法中断工作后恢复上下文，也无法在多个项目间切换。

**存储架构**：

```
~/.agentgo/sessions/
├── active-session              # 当前激活的 Session ID
├── sessions.db                 # 元数据索引（SQLite 或 JSON Lines）
│
├── sess-{uuid}/
│   ├── metadata.json           # {id, name, project_root, created_at, updated_at}
│   ├── snapshot.json           # 完整系统状态快照（可选）
│   ├── history.jsonl           # 操作历史事件流（可回放重建状态）
│   └── logs/agentgo.log        # 该 Session 的专属日志
│
└── archive/                    # 归档的已完成 Session
```

**Session 生命周期 CLI**：

```bash
./agentgo session new "fix-auth-bug" --root ~/projects/myapp
./agentgo session list
./agentgo session switch sess-a1b2c3d4
./agentgo session archive sess-x9y8z7w6
./agentgo session restore sess-x9y8z7w6
```

**状态持久化分阶段实施**：

| 阶段 | 内容 |
|------|------|
| 阶段一 | 仅日志隔离（最小可行）：Session 切换 = 切换日志文件 + 新建空白状态 |
| 阶段二 | 快照持久化：TaskStore、Roster、Mailbox 序列化到 `snapshot.json`，支持暂停-恢复 |
| 阶段三 | 事件溯源：记录所有操作事件到 `history.jsonl`，恢复时重放重建状态，支持时间旅行调试 |

**与工具系统的关联**：
- §1.5 Shell 命令拦截：Session 级别的拦截规则可随 Session 持久化
- §1.4 分级权限模型：Session 可绑定特定的权限模板
- ~~§2.1 Git Worktree：Session 天然与 worktree 绑定~~（**已删除**，2026-04-09）

**暂不实施的原因**：当前阶段优先级在于功能正确性和安全性。Session 化管理属于体验优化，待核心架构稳定后再实施。日志隔离（阶段一）可作为近期改善。

---

## 附录：V2 完结总结

### 已实现功能（✅）

| 章节 | 内容 | 完成日期 |
|------|------|---------|
| §1.1 | ToolGroup 架构重构 + Phase 1 Hook 迁移 | 2026-04-09 |
| §1.10 | Artifacts 基础设施（in-memory） | 2026-04-08 |
| §2 | Git Worktree 隔离 | ~~已删除~~ 2026-04-09 |
| §3.1 | ConflictResolver 崩溃恢复 | ~~已删除~~ 2026-04-09 |
| §3.3 | Explorer 权限强化 | 2026-04-08 |
| §3.4 | ConflictResolver 独立模型 | ~~已删除~~ 2026-04-09 |
| §3.6 | 代理间通信防循环机制 | 2026-04-09 |
| §1.9 | 探针工具（`probe_directory` 合并实现） | 2026-04-11 |

### 探针工具（§1.9）

| 原计划 | 优先级 | 状态 |
|--------|--------|------|
| `list_directory` | P0 | ✅ 由 `probe_directory` 覆盖（2026-04-11） |
| `count_files` | P1 | ✅ 由 `probe_directory` 覆盖（综述行含文件/文件夹计数） |
| `file_summary` | P2 | ✅ 由 `probe_directory` 覆盖（含文件大小 + 类型分布） |

原计划的三个独立工具合并为一个 `probe_directory`（Scheduler 专属），一次调用返回完整视图：树状目录（含每个文件的磁盘大小）、类型分布统计、综述行。实现位于 `internal/tools/scheduler_probe.go`，提示词指引已加入 scheduler system prompt。

### 已迁移到 V3 的内容

- **Finalization Tool 终止桥**（v3 §6）：从 scheduler 特化实现迁移为 agent 层通用能力
- **工具集分层配置**（v3 §9.1）：原 v2 §1.3
- **分级权限模型**（v3 §9.2）：原 v2 §1.4
- **管理员信赖标记**（v3 §9.3）：原 v2 §1.6
- **代理能力声明**（v3 §9.4）：原 v2 §1.7（阶段一 + 阶段二）
- **工具可用性探针**（v3 §9.5）：原 v2 §1.8
- **Artifacts 持久化**（v3 §9.6）：原 v2 §1.10 持久化部分
- **冲突避免长期方案**（v3 §9.7）：原 v2 §3.2
- **Agent 休眠/唤醒**（v3 §9.8）：原 v2 §3.5
- **团队感知系统**（v3 §7 + §8.2）：原 v2 §3.7（3.7.1-3.7.4 被 Agent Hook 完整接管）
- **Session 化日志**（v3 §9.9）：原 v2 §3.8

### 已完成但 v2 附录误标的项

| 章节 | 内容 | 实际状态 |
|------|------|---------|
| §1.2 | Web 工具重构 | ✅ 已完成（2026-04-08），正文已标记 |
| §1.5 | Shell 命令拦截配置化 | ✅ 已完成（2026-04-10），正文已标记 |

### 文档版本

*文档版本：v2.4*  
*最后更新：2026-04-11（探针工具以 probe_directory 合并实现，v2 全部完结）*
