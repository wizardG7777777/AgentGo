# AgentGo YAML 配置文件清单与对比分析

> 生成日期: 2026-04-26
> 数据来源: 项目根目录下 6 个 YAML 文件的逐文件审查 + explorer agent 跨文件分析
> 补充参考: [config-consolidated-reference.md](./config-consolidated-reference.md)（v5 schema 权威参考）、[yaml-config-guide.md](./yaml-config-guide.md)（撰写指南）

---

## 1. 文件清单

项目根目录共 **6 个 YAML 文件**，按用途分为三类：

| # | 文件 | 编码 | 行数 | 分类 | 角色 |
|---|------|------|------|------|------|
| 1 | `config.example.yaml` | UTF-8 | ~173 | 📘 模板 | 带英文注释的完整配置示例，供新用户复制起步 |
| 2 | `general.yaml` | UTF-8 | ~171 | 📘 模板 | 最详尽的中文注释配置模板，含 YAML anchor 复用示例 |
| 3 | `setting.yaml` | **UTF-16 LE** ⚠️ | ~156 | ✅ 运行配置 | 主运行配置，被 `main.go` 默认加载（`-config` 缺省值） |
| 4 | `setting.v4.yaml` | UTF-8 | ~164 | ✅ 运行配置 | v4 版运行配置，与 setting.yaml 内容几乎相同但编码正常 |
| 5 | `setting.test-concurrent.yaml` | UTF-8 | ~159 | 🧪 测试配置 | 并发场景冒烟测试专用（explorer replicas=3） |
| 6 | `general_reactor.yaml` | UTF-8 | 16 | 🧪 最简配置 | Reactor 模式最简启动配置，无 scheduler/agents/infra |

### ⚠️ `setting.yaml` 编码警告

`setting.yaml` 以 **UTF-16 LE** 编码存储，每个 ASCII 字符被 `\x00` 字节分隔。这可能是 Windows 下某些编辑器保存的产物。在非 Windows 环境或某些 YAML 解析器中可能导致乱码。**推荐使用 `setting.v4.yaml`（UTF-8 编码，内容等价）替代。**

---

## 2. 逐文件内容详解

### 2.1 `config.example.yaml` — 英文注释模板

**用途**: 新用户入门参考，复制后改名即可使用。

```yaml
# ── LLM 配置 ──
llm:
  base_url: "https://api.deepseek.com/v1"
  api_key: "${DEEPSEEK_API_KEY}"       # 必填，环境变量
  default_model: "deepseek-chat"
  timeout_sec: 120

# ── 工具配置集 ──
tool_profiles:
  - name: worker_standard
    tools: [read_file, list_dir, grep_search, glob_search, write_file, edit_file,
           run_shell, web_search, web_fetch, publish_task, send_message]
  - name: worker_readonly
    tools: [read_file, list_dir, grep_search, glob_search, send_message]
  - name: worker_code_only
    tools: [read_file, list_dir, grep_search, glob_search, write_file, edit_file,
           publish_task, send_message]
  - name: explorer_codebase
    tools: [read_file, list_dir, grep_search, glob_search, send_message]
  - name: explorer_full
    tools: [read_file, list_dir, grep_search, glob_search, web_search, web_fetch, send_message]

scheduler:
  model: "deepseek-reasoner"
  enforce_compact_token_threshold: 80000

agents:
  - kind: worker
    replicas: 3
    profile: worker_standard
    model: "deepseek-chat"
    system_prompt_file: prompts/worker.md
    task_max_retries: 3

  - kind: explorer
    replicas: 1
    event_type: explore
    profile: explorer_full
    model: "deepseek-reasoner"
    system_prompt_file: prompts/explorer.md

project_root: "."
infra:
  watchdog:    { interval_sec: 30 }
  mail_notifier: { enabled: true, interval_sec: 60 }
  store:
    event_channel_buffer: 256
    fifo_limit: 100
    default_concurrency: 3
    default_timeout_sec: 300
  roster: { wait_timeout_sec: 300 }

startup_probe: "tcp"
startup_probe_timeout_sec: 5
startup_probe_failure_action: "warn"

max_subtask_depth: 3
shell_timeout_sec: 60
search_api_provider: serper
search_api_url: https://google.serper.dev/search
search_api_key: "${SERPAPI_API_KEY}"
```

**特点**:
- `tool_profiles` 用 YAML 列表格式（`- name: xxx` + `tools: [...]`）
- llm 块不再含 `provider` 字段（V6 已移除，读到即返回迁移诊断错误），模型用 `deepseek-chat`

---

### 2.2 `general.yaml` — 中文注释模板

**用途**: 最详尽的中文注释模板，适合需要理解每个配置项含义的开发者。

**与 `config.example.yaml` 的关键差异**:

| 差异项 | config.example.yaml | general.yaml |
|--------|-------------------|--------------|
| `max_concurrent_agents` | **不存在** | **5**（general.yaml 特有字段） |
| tool_profiles 格式 | 列表格式 | 映射格式（`profile_name: [tools]`） |
| YAML anchor | 无 | `&worker_base` / `<<: *worker_base` 复用示例 |
| 注释语言 | 英文 | 详细中文 |
| agent kind 示例 | 仅 worker + explorer | 含 fast_worker、deep_worker 等多变体 |

**`max_concurrent_agents: 5`** 是 general.yaml 特有的顶层字段，限制同时运行的 agent 实例上限。

---

### 2.3 `setting.yaml` — 主运行配置（UTF-16 LE）

**用途**: `main.go` 中 `-config` 参数的默认值，即生产环境实际加载的配置。

**关键配置值**:

| 配置路径 | 值 |
|----------|-----|
| `llm.base_url` | `https://api.deepseek.com` |
| `llm.default_model` | `deepseek-v4-pro` |
| `scheduler.model` | `deepseek-v4-pro` |
| `agents[worker].model` | `deepseek-v4-flash` |
| `agents[worker].replicas` | 3 |
| `agents[explorer].model` | `deepseek-v4-flash` |
| `agents[explorer].replicas` | 1 |
| `infra.mail_notifier.enabled` | true |
| `search_api_provider` | serper |

**文件末尾**有一块被注释掉的 "DRY 模板"（YAML anchor 用法示例），与 `general.yaml` 的 anchor 示例呼应。

---

### 2.4 `setting.v4.yaml` — v4 运行配置（UTF-8）

**用途**: 与 `setting.yaml` 内容本质上相同，但正确使用 UTF-8 编码。推荐作为日常运行的配置文件。

**与 `setting.yaml` 的细微差异**:
- 注释略有增减（setting.yaml 末尾的 DRY 模板注释在 v4 中可能不存在或位置不同）
- 所有关键配置值一致: `llm.default_model: deepseek-v4-pro`, worker 用 `deepseek-v4-flash`, explorer 用 `deepseek-v4-flash`

**建议**: 用 `setting.v4.yaml` 替代 `setting.yaml` 以避免编码问题。启动时指定: `./agentgo -config setting.v4.yaml`

---

### 2.5 `setting.test-concurrent.yaml` — 并发测试配置

**用途**: 仅在跑"需要多 Explorer 并行"的人工冒烟测试时使用。基于 `setting.v4.yaml` 模板。

**唯一差异**: `agents[explorer].replicas` 从 **1 → 3**。

**适用场景**:
- **P12（多 explore 并行调研）**: 1 Explorer 串行无法验证 batch 并行 dispatch
- **P13（send_message 多收件人路由）**: 测 wake-worthy / per-agent-dedup hook 的实际行为
- **ClaimTask 原子性竞争**: 多实例同时 poll 同任务的 race window 验证

**不适用场景**（直接用 `setting.yaml` 即可）:
- 单 Explorer 调研（P11）: 3 实例反而引入 race noise
- fail-fast / hashline / shell 链路（P14、P8、P9）: 跟 replica 数无关
- 大部分日常使用: 多并发 web_search 同 IP 易触发 DDG 反爬

**已知副作用**:
- DDG bot 检测概率上升（3 并发 web_search → 更快 429）
- LLM 调用成本翻 3 倍（3× reasoning 并行）
- trace 当前对 mailbox/scheduler batch/ClaimTask race 无事件覆盖

**启用方式**: `./agentgo -config setting.test-concurrent.yaml`

**其他值得注意的差异**（相对 setting.v4.yaml）:
- `enforce_compact_token_threshold: 10000`（而非默认 4000）

---

### 2.6 `general_reactor.yaml` — Reactor 模式最简配置

**用途**: 最简 Reactor 模式启动配置，仅 16 行。

```yaml
llm:
  base_url: "https://api.deepseek.com/v1"
  api_key: "${DEEPSEEK_API_KEY}"
  default_model: "deepseek-chat"

tool_profiles:
  - name: default
    tools: [read_file, list_dir, grep_search, glob_search, web_search, web_fetch,
           write_file, edit_file, run_shell, publish_task, send_message]

max_loops: 10
project_root: "."
```

**特点**:
- 无 `agents` 列表 — reactor 模式下 agent 由代码内部管理
- 无 `scheduler` 块 — 不依赖事件驱动调度
- 无 `infra` / `startup_probe` — 无 watchdog、邮件通知、启动探针
- 单一 tool profile `default`，所有工具合一
- 唯一的运行时参数是 `max_loops: 10`

---

## 3. 跨文件差异矩阵

| 方面 | config.example | general | setting.yaml | setting.v4 | test-concurrent | reactor |
|------|:---:|:---:|:---:|:---:|:---:|:---:|
| **角色** | 英文模板 | 中文模板 | 运行配置 | 运行配置(v4) | 并发测试 | 最简启动 |
| **编码** | UTF-8 | UTF-8 | UTF-16 LE ⚠️ | UTF-8 | UTF-8 | UTF-8 |
| **行数** | ~173 | ~171 | ~156 | ~164 | ~159 | 16 |
| **default_model** | deepseek-chat | deepseek-chat | deepseek-v4-pro | deepseek-v4-pro | deepseek-v4-flash | deepseek-chat |
| **scheduler.model** | deepseek-reasoner | deepseek-reasoner | deepseek-v4-pro | deepseek-v4-pro | deepseek-v4-pro | — |
| **worker.model** | deepseek-chat | deepseek-chat | deepseek-v4-flash | deepseek-v4-flash | deepseek-v4-flash | — |
| **explorer.model** | deepseek-reasoner | deepseek-reasoner | deepseek-v4-flash | deepseek-v4-flash | deepseek-v4-flash | — |
| **worker replicas** | 3 | 3 | 3 | 3 | 3 | — |
| **explorer replicas** | 1 | 1 | 1 | 1 | **3** | — |
| **tool_profiles 格式** | 列表 | 映射 | 列表 | 列表 | 映射 | 列表 |
| **max_concurrent_agents** | — | **5** | — | — | — | — |
| **max_loops** | — | — | — | — | — | **10** |
| **compact_threshold** | 4000 | 4000 | 4000 | 4000 | **10000** | — |
| **邮件通知** | ✓ | ✓ | ✓ | ✓ | ✓ | — |
| **启动探针** | tcp | tcp | tcp | tcp | tcp | — |
| **注释语言** | EN | 中文 | 中/EN | 中/EN | 中文 | — |

---

## 4. 文件关系图谱

```
general.yaml (最详尽中文模板, ~171行)
  ├── 模板蓝本 ──→ config.example.yaml (精简英文模板, ~173行)
  │                  ├── 参考 ──→ setting.v4.yaml (UTF-8 运行配置, ~164行)
  │                  │              └── 派生 ──→ setting.test-concurrent.yaml (并发测试, ~159行)
  │                  └── 参考 ──→ setting.yaml (UTF-16 LE 运行配置, ~156行) ⚠️
  │                                  └── 内容 ≈ setting.v4.yaml（编码不同）
  └── 精简 ──→ general_reactor.yaml (最简 reactor 模式, 16行)

推荐使用路径:
  新用户起步:     config.example.yaml → 复制改名 → 修改后使用
  日常运行:       setting.v4.yaml（或 setting.yaml，但注意编码）
  并发测试:       setting.test-concurrent.yaml
  Reactor 模式:   general_reactor.yaml（极少使用）
  学习参考:       general.yaml（中文注释最详尽）
```

---

## 5. 配置项完整参考

### 5.1 LLM 配置 (`llm:`)

| 配置项 | 类型 | 默认值 | 必需 | 说明 |
|--------|------|--------|------|------|
| `base_url` | string | — | ✅ | API 端点地址 |
| `api_key` | string | — | ✅ | API 密钥，支持 `${ENV_VAR}` 环境变量引用 |
| `default_model` | string | — | ✅ | 全局默认模型，所有 agent 的后备 |
| `timeout_sec` | int | 120 | ❌ | 单次 LLM 请求超时秒数 |
| ~~`provider`~~ | — | — | — | **V6 已移除**：AgentGo 只实现 OpenAI-compatible Chat Completions；旧配置保留该字段会在 Validate 返回迁移诊断错误 |

### 5.2 工具配置集 (`tool_profiles:`)

| 配置项 | 类型 | 说明 |
|--------|------|------|
| `tool_profiles[].name` 或 key | string | profile 标识名 |
| `tool_profiles[].tools` 或 value | string[] | 该 profile 包含的工具列表 |

**预定义 profile 及工具清单**:

| Profile | read_file | list_dir | grep | glob | write_file | edit_file | run_shell | web_search | web_fetch | publish_task | send_message |
|---------|:---:|:---:|:---:|:---:|:---:|:---:|:---:|:---:|:---:|:---:|:---:|
| `worker_standard` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `worker_readonly` | ✓ | ✓ | ✓ | ✓ | — | — | — | — | — | — | ✓ |
| `worker_code_only` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | — | — | — | ✓ | ✓ |
| `explorer_codebase` | ✓ | ✓ | ✓ | ✓ | — | — | — | — | — | — | ✓ |
| `explorer_full` | ✓ | ✓ | ✓ | ✓ | — | — | — | ✓ | ✓ | — | ✓ |
| `default` (reactor) | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |

### 5.3 Scheduler 配置 (`scheduler:`)

| 配置项 | 类型 | 默认值 | 必需 | 说明 |
|--------|------|--------|------|------|
| `model` | string | 继承 `llm.default_model` | ❌ | scheduler 专用模型覆盖 |
| `enforce_compact_token_threshold` | int | 80000 | ❌ | 单任务累计 prompt token 的一次性历史压缩阈值；0 使用默认值 |

### 5.4 Agent 节点配置 (`agents[]:`)

| 配置项 | 类型 | 默认值 | 必需 | 说明 |
|--------|------|--------|------|------|
| `kind` | string | — | ✅ | agent 类型标识（`worker` / `explorer`） |
| `replicas` | int | — | ✅ | 该 kind 的并发实例数 |
| `event_type` | string | `""` | ❌ | 事件类型过滤，空 = 接受所有（explorer 填 `"explore"`） |
| `profile` | string | — | ✅ | 引用 `tool_profiles` 中的名称 |
| `model` | string | 继承 `llm.default_model` | ❌ | 覆盖该 kind 的模型 |
| `system_prompt_file` | string | — | ✅ | 系统提示词文件路径 |
| `task_max_retries` | int | 3 | ❌ | 任务失败最大重试次数 |
| `enforce_compact_token_threshold` | int | 4000 | ❌ | 触发上下文压缩的 token 阈值 |

### 5.5 基础设施 (`infra:`)

| 配置项 | 类型 | 默认值 | 说明 |
|--------|------|--------|------|
| `project_root` | string | `"."` | 项目根目录（顶层字段，不在 infra 内）；启动时 canonicalize，空值/不可访问目录拒绝启动 |
| `infra.watchdog.interval_sec` | int | 30 | watchdog 健康检查间隔 |
| `infra.mail_notifier.enabled` | bool | true | 是否启用 agent 间邮件通知 |
| `infra.mail_notifier.interval_sec` | int | 60 | 邮件轮询间隔 |
| `infra.store.event_channel_buffer` | int | 256 | 事件通道缓冲区 |
| `infra.store.fifo_limit` | int | 100 | FIFO 队列上限 |
| `infra.store.default_concurrency` | int | 3 | 默认并发数 |
| `infra.store.default_timeout_sec` | int | 300 | 默认任务超时 |
| `infra.roster.wait_timeout_sec` | int | 300 | roster 等待超时 |

### 5.6 启动探针

| 配置项 | 类型 | 默认值 | 说明 |
|--------|------|--------|------|
| `startup_probe` | string | `"tcp"` | `"tcp"` 做 TCP 拨号探针，`"off"` 跳过 |
| `startup_probe_timeout_sec` | int | 5 | 单次探针超时 |
| `startup_probe_failure_action` | string | `"warn"` | `"warn"` = 警告后继续，`"exit"` = 失败即退出 |

### 5.7 杂项

| 配置项 | 类型 | 默认值 | 说明 |
|--------|------|--------|------|
| `max_subtask_depth` | int | 3 | 子任务最大递归深度 |
| `shell_timeout_sec` | int | 60 | shell 命令超时 |
| `shell_blacklist` | string[] | `[]` | 禁止执行的命令 |
| `shell_greylist` | string[] | `[]` | 需确认才能执行的命令 |
| `allow_project_shell_rule_removals` | bool | `false` | 是否显式允许项目规则删除系统默认或主配置追加的黑/灰名单；默认只能追加 |
| `search_api_provider` | string | `"serper"` | 搜索 API 提供商 |
| `search_api_url` | string | — | 搜索 API 端点 |
| `search_api_key` | string | — | 搜索 API 密钥（支持 `${ENV_VAR}`） |
| `max_concurrent_agents` | int | — | **仅 general.yaml**，限制同时运行的 agent 数 |
| `max_loops` | int | 10 | **仅 general_reactor.yaml**，reactor 模式循环上限 |

---

## 6. 必需 vs 可选配置项速查

### ✅ 绝对必需（无默认值可继承）

| 配置项 | 原因 |
|--------|------|
| `llm.base_url` | 无默认值 |
| `llm.api_key` | 无默认值 |
| `llm.default_model` | 无默认值 |
| `tool_profiles`（至少一个） | agent 能力白名单 |
| `agents` 或 reactor 顶层参数 | 至少一种 agent 定义方式 |
| `agents[].kind` | 类型标识 |
| `agents[].replicas` | 实例数 |
| `agents[].profile` | 引用 tool_profile |
| `agents[].system_prompt_file` | 系统提示词 |

### 🔶 条件必需

| 配置项 | 触发条件 |
|--------|----------|
| `search_api_url` + `search_api_key` | 任意 agent 使用了 `web_search` / `web_fetch` |
| `scheduler.model` | scheduler 需要与全局 LLM 不同的模型 |
| `agents[].model` | 该 agent 需要与全局 LLM 不同的模型 |
| `startup_probe` | 需要启动健康检查 |
| `infra.roster.wait_timeout_sec` | 使用多 agent 集群 |

### ❌ 可选（有合理默认值）

`llm.timeout_sec`(120), `scheduler.enforce_compact_token_threshold`(80000), `agents[].event_type`(""), `agents[].task_max_retries`(3), `agents[].enforce_compact_token_threshold`(4000), `infra.*`(见上表), `project_root`("."), `max_subtask_depth`(3), `shell_timeout_sec`(60), `shell_blacklist`([]), `shell_greylist`([]), `search_api_provider`("serper"), `startup_probe`("tcp"), `startup_probe_timeout_sec`(5), `startup_probe_failure_action`("warn")

> V6 移除项（不再出现在默认值清单，显式设置报迁移诊断）：`llm.provider`、`scheduler.agent_max_loops`、`agents[].agent_max_loops`、`scheduler.context_limit`、`agents[].context_limit`。

---

## 7. LLM 模型覆盖层级

配置中的模型选择按以下优先级逐层覆盖：

```
llm.default_model           ← 全局默认（必须设置）
  ├── scheduler.model       ← scheduler 专用覆盖（可选）
  └── agents[].model        ← 每个 agent kind 独立覆盖（可选）
```

**实际运行配置中的取值**:

| 层级 | config.example | setting.yaml/v4 | test-concurrent |
|------|---------------|-----------------|-----------------|
| 全局 | `deepseek-chat` | `deepseek-v4-pro` | `deepseek-v4-flash` |
| scheduler | `deepseek-reasoner` | `deepseek-v4-pro` | `deepseek-v4-pro` |
| worker | `deepseek-chat` | `deepseek-v4-flash` | `deepseek-v4-flash` |
| explorer | `deepseek-reasoner` | `deepseek-v4-flash` | `deepseek-v4-flash` |

---

## 8. 已知问题与建议

| # | 问题 | 严重程度 | 建议 |
|---|------|----------|------|
| 1 | `setting.yaml` 为 UTF-16 LE 编码 | ⚠️ 中 | 迁移到 `setting.v4.yaml`（UTF-8），删除或重命名旧文件 |
| 3 | ~~`setting.test-concurrent.yaml` 的 `llm.provider` 取值笔误~~ | ✅ 已解决 | `llm.provider` 字段已于 V6 整体移除，各配置文件中的该字段已同步删除 |
| 4 | 文件数量多（6 个），部分功能重叠 | 💡 低 | 考虑: 保留 `config.example.yaml`（模板）+ `setting.yaml`（UTF-8 修复后）+ `setting.test-concurrent.yaml`，归档其余 |
| 5 | `setting.test-concurrent.yaml` 含硬编码 api_key | 🔴 高 | 应立即替换为 `${DEEPSEEK_API_KEY}` 环境变量引用 |
