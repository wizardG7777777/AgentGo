# nextUpgrade v4

> 状态：✅ **主体已完成**（2026-04-26 Phase A/B/C/D/E 全部落地；§7 Hashline / §3
> 能力声明阶段二 / §9 完整错误码策略 / §10 Did-You-Mean 扩展设计 是路线图上独立的
> 后续工作项，不阻塞 v4 主体——详见下方"实施进度"块与 §状态总览）

## 实施进度（2026-04-26）

**Phase A — 独立增量（已完成 ✅）**：
- §10 Did-You-Mean：新增 `internal/tools/suggest/`（包装 `github.com/sahilm/fuzzy`），
  接入 `local_read.go` 的 `glob_search` 空结果路径与 `read_file` 路径不存在路径
- §11.7.3 HistoryEntry 实测锚定：新增 `PromptTokens` / `CompletionTokens` / `Model`
  三字段；`agent.go` history append 处记录实测值
- §11.7.4 截断策略：新增 `internal/agent/token_truncate.go` 实现
  `keepRecentForTruncate=6` 包级常量、`PredictNextPromptTokens`、`TruncateHistory`
  与 `ErrContextLimitTooSmall`
  > ✅ **装配漏接已修复**（2026-04-26 commit `6e45a73`，commit message 末段明
  > 写"§11.7.4 truncate 接通修复"）：`processTask` 主循环现在在 `a.ContextLimit > 0`
  > 守卫下调用 `TruncateHistory()`（`internal/agent/agent.go:501-503`）。
  > 历史教训保留作 CLAUDE.md "Shipping conventions" 案例：函数级 6 条单测全绿，
  > 但装配握手位（main loop ↔ TruncateHistory）无人测；端到端
  > `TestE2E_TruncateFiresOnContextLimit`（详见 §11.7.4 验证计划）已加入作为
  > 反退化护栏。
- `Agent` 结构体追加 `Model` / `ContextLimit` 字段（runner 注入用）

**Phase B — v4 基础设施（已完成 ✅）**：
- §11.4 Go 配置类型：`LLMConfig` / `AgentKind` / `SchedulerKind` /
  `InfraConfig` + 4 个子结构 / `AgentRuntimeConfig` 新增到 `internal/config/`
  （Phase D 后 v3 顶层字段已整体下线，本节类型成为 `Config` 唯一字段集）
- §11.5.3 启动校验：`Config.Validate()` 实现 12 条规则；Phase D 后硬要求
  `agents` 非空（空列表直接 fail-fast）
- §11.3 末尾：`LoadConfig` 在反序列化前调用 `os.ExpandEnv` 替换 `${ENV_VAR}`
- §11.6.6 worker/explorer 折叠：`internal/runner/`（`Runner` + `CurrentTaskHolder`）
  统一 runner 实现；`internal/agent/team_snapshot.go` 持有 `BuildTeamSnapshot`
  （Phase D 已删除 `internal/worker/` 与 `internal/explorer/` 中的副本）
- §11.6.1 + §9.5：`internal/bootstrap/runtime_builder.go` 提供
  `buildKindLLMClient` / `buildAgentRuntime` / `buildSchedulerRuntime`；
  `internal/bootstrap/probe.go` 提供 `printStartupBanner` + `startupProbe`
- §11.8 S8：`prompts/worker.md` + `prompts/explorer.md`（从原 worker.go /
  explorer.go 常量抽出）+ `setting.v4.yaml` 参考模板（Phase D 后默认
  `setting.yaml` 也已替换为 v4 schema，两份近似等价）

**Phase C — bootstrap 切换（已完成 ✅，2026-04-26 同 session 完成）**：
- `bootstrap.go` 主流程：v4 kind-based 路径已成为唯一启动路径，按 `cfg.Agents`
  列表实例化 `runner.Runner`（kind × replicas），v3 旧分支与 mirror 层一并下线
  （详见 Phase D）
- 新增 `System.Runners []*runner.Runner` 字段；`Start()` 据此启动 goroutine
- `Bootstrap()` 早期插入 `cfg.Validate()` + `printStartupBanner` + `startupProbe`
  调用，失败按 `startup_probe_failure_action` 分支（warn / exit）
- **端到端烟测通过**（CLAUDE.md "Shipping conventions" 规则 1）：
  `setting.v4.yaml`：banner 列出 3 kind，TCP probe 到 api.deepseek.com:443 成功，
  4 个 runner 实例（worker×3 + explorer×1）启动 + /quit 干净关闭

**Phase D — v3 兼容层整体下线（已完成 ✅，2026-04-26 commit `6e45a73`）**：
- 删除 `internal/worker/` + `internal/explorer/` 两个 package（孤儿包清零，共 10
  个文件随 commit 一并删除：`worker.go` / `worker_test.go` / `team_snapshot_e2e_test.go`
  / `shell_approval_chain_e2e_test.go` / `send_message_test.go` / `worktree_isolation_test.go`
  + explorer 同名测试 + `chain_depth_test.go` + `explorer.go`）
- 删除 `Config` struct 中的 23 个 v3 顶层字段 + `mirrorV4ToV3` + `ValidateWorkers`
  + `ValidateAgentDeclarations` + `Resolved{Agent,Worker}Declaration`——v4 嵌套块
  成为唯一受支持格式（旧字段在 yaml 解析时被默默忽略，不再产生运行时效果）
- 删除 `bootstrap.go` 全部 `if len(cfg.Agents) == 0` v3 fallback 分支；统一走
  `runner.Runner`
- 系统级常量化：`mailbox.DefaultChainMaxDepth=10` / `mailbox.DefaultInboxSize=32`
  / `scheduler.schedulerMaxLoops=10` / `Infra.Store.DefaultTimeoutSec`
  （v4 §11.5.4 / §11.5.5）
- 默认 `setting.yaml` 已替换为 v4 格式（与 `setting.v4.yaml` 内容等价，仅微调
  `enforce_compact_token_threshold` 与 explorer `context_limit`：8000→16000，
  原因写在 yaml 注释里——实测 scheduler 首调 prompt_tokens=8525，8000 会让
  explorer 一启动就触发硬截断）
- 新增 banner 第二行打印 `Config File: <path>`，避免"测 v4 但实跑 v3 默认"的混淆
- `Validate()` 现在硬要求 `agents` 非空——空 yaml 启动直接 `agents 列表为空` 退出
- 删除 `internal/config/config_test.go`（59 个 v3 字段断言）和
  `internal/bootstrap/bootstrap_test.go`（v3 worker/explorer 装配测试）
- `main_startup_test.go` 重写：fallback 测试改为断言 v4 fail-fast
- `go test ./...` 全绿；`./agentgo.exe -config setting.v4.yaml` 烟测通过

**Phase E — 收尾清理（已完成 ✅，2026-04-26 同 session 完成）**：
- 删除 `tools.MetaGroup.DisablePublishTask` 字段：Phase D 删除 worker/explorer 后
  该 capability 位失去最后调用方；v4 路径完全靠 `runner.NewToolRegistryWithAllowlist`
  对 `AllowedTools` 做白名单过滤来收窄能力，与 capability 位等价但更内聚。同步
  更新 `meta.go` 上方注释保留考古信息
- §11.8 S11 终结路径 `trace.Emit` 对称扫描落地：新增
  [internal/agent/terminal_emit_symmetry_test.go](../../internal/agent/terminal_emit_symmetry_test.go)
  AST 静态扫描 3 条不变量（FailTask 同函数体内必有 KindTaskFailed emit /
  processTask 内 ctx.Done 分支必有 KindTaskCancelled emit / 文件级 KindTaskCompleted ≥ 2）+
  [panic_emit_test.go](../../internal/agent/panic_emit_test.go) 运行时双重验证
  - 副作用：扫描立即发现一处真缺陷——`processTask` 顶部 `defer recover()` 路径
    （[agent.go:268-296](../../internal/agent/agent.go#L268-L296)）调用 `Store.FailTask`
    后未 emit `KindTaskFailed`（与 [terminateTask](../../internal/agent/agent.go#L811)
    非对称），导致 panic 引发的失败对 trace 观察者完全失明。同 commit 已修复
  - 负向自检通过：临时移除 emit 后测试精确报"agent.go:282 在函数 processTask 内
    调用 a.Store.FailTask 但同函数体内未发现 trace.Emit{Kind: trace.KindTaskFailed}"
- 清理 `setting.yaml` 头 4-5 行 stale 注释：原文写"此文件是 v4 格式的参考模板，与
  现有 setting.yaml 并存 / 当前默认仍读 setting.yaml（v3）"，是从 `setting.v4.yaml`
  复制内容时带过来的旧注释，与 Phase D 实际状态相反——已改写为 "v4 嵌套 schema
  是唯一受支持的格式，旧 v3 顶层字段在 yaml 解析时会被默默忽略"

**剩余未落地（无具体待办）**：
- 主体 v4 路线图至此全部清空；后续工作进入路线图独立项（§7 Hashline / §3 能力声明
  阶段二 / §9 完整错误码策略与重试预算）

> **2026-04-26 验证（end-to-end 复核 + 收尾清理）**：本进度块由独立 agent 扫描
> 代码核对——Phase A/B/C/D/E 全部声明项与代码现状一致。`go test ./...` 全绿；
> `./agentgo.exe -config setting.v4.yaml` 端到端烟测通过（4 runner 启动 + TCP probe
> + /quit 干净关闭）。S11 测试包含负向自检：临时撤销修复时测试精确报告漏接位置。
> 与 `KNOWN_ISSUES.md` 同段保持一致。

---

## 1. Per-Worker 工具集配置（独立 Profile 分配）

> 状态：✅ **已完成**（2026-04-19），但**已被 §11 取代**（2026-04-25）
> 优先级：P2
> 前置依赖：v3 §9.1 工具集分层配置（已完成 2026-04-15，当前所有 worker 共享同一 profile）
> Spec：[`.kiro/specs/per-worker-tool-profiles/`](../../.kiro/specs/per-worker-tool-profiles/)
> 关联：v3 §9.2 分级权限模型、v3 §9.4 能力声明阶段二

> **取代说明（2026-04-25）**：本节解决的是"同一类 worker 内允许异质 profile"的问题，
> 但后续讨论判定**同一角色内的 worker 异构价值有限**——把同质化资源池人为打散后，
> scheduler 还要做"哪个 worker 能干这事"的额外匹配。正确的抽象是 **kind × 实例数**：
> 每个 kind 自带工具/提示词/模型，同 kind 内的实例完全同质。该方向在 §11 中实现，
> 包括将内置 `internal/worker` 与 `internal/explorer` 两个 Go 包整体废除。
>
> §1 的设计/实施细节（原 §1.1–§1.6）已删除，原内容可在 git history 中查阅。
> §1.4 的 5 个预置 profile 模板已迁入 §11.3；§1.1 的 3 个使用场景已迁入 §11.1。

---

## 3. 能力声明阶段二（任务级能力匹配）

> 原 v2 §1.7 阶段二 → v3 §9.4 阶段二迁入 | 优先级：P4
> 前置依赖：v3 §9.4 阶段一（✅ 已完成）、v4 附录 A 分级权限模型
> 关联：v4 §11.6.4 Board snapshot 按 kind 聚合

### 3.1 方案

`publish_task` 工具新增 `required_capabilities` 参数，由 `ClaimTask` 逻辑在代理认领时做能力匹配校验。

### 3.2 触发条件

当系统中存在不同能力的 worker（v4 §1 落地后），且通用型 worker 的 capabilities 不再是全集时，`ClaimTask` 匹配校验才有实际意义。

---

## 7. Hashline 行哈希增强

> 状态：✅ **已完成**（2026-04-26）
> 优先级：P1（解决多 agent 并发编辑的行级漂移痛点）
> 前置依赖：无（hook 系统已稳定、tools 子包已成型）
> 关联：v3 §2-§5 原"行哈希增强"占位（本节为其定稿方案，v3 占位关闭）
> 参考：`oh-my-openagent` 项目的 `HASHLINE_EDIT_INVESTIGATION.zh.md`（2026-04-19 取得）

### 7.1 背景

v3 §2-§5 原计划引入"行哈希增强"，因缺少参考实现而搁置。参考 `oh-my-openagent`（opencode 插件）的调查报告后，方案具备落地条件。本节是 AgentGo 适配版的定稿设计——**校验层叠加，不替换 `edit_file` 的 `old_str/new_str` 编辑模型**，并将护栏机制完整接入 Hook 系统。

**当前 `edit_file` 的三个真实痛点**：

1. **空白字符脆弱性**：`old_str` 要求字节级完全匹配。LLM 稍微吃掉一个缩进、tab/空格混用就匹配失败。为规避此问题 LLM 倾向把 `old_str` 写长以保证唯一性，反而放大空白漂移概率——正反馈失败模式。

2. **`expected_hash` 粒度过粗（多 agent 致命）**：整文件 SHA256 是 binary 信号，任何字节变化都失效。典型场景：worker-1 想改第 42 行（未变），worker-2 修改了第 5 行 → worker-1 的 `expected_hash` 失败 → 被迫重读全文。worker 数量越多这种"无辜失效"越频繁。

3. **缺少"位置 + 内容"双锚点**：`old_str/new_str` 纯内容，无位置；行号纯位置，无内容。LLM 推理时两者都需要，但工具只能表达一个——导致经常认错行（特别是文件中有多个相似块时）。

**Hashline 解决的是什么**：为每行绑定稳定的内容指纹（2 字符哈希），让 LLM 用 `LINE#HASH` 作为既有位置又有内容验证的锚点。多 agent 场景下，**只有 LLM 关心的那几行变了才拒绝编辑**，而非整文件任何变化都拒绝。

### 7.2 设计决策摘要

| 议题 | 决策 | 理由 |
|---|---|---|
| read_file 哈希输出位置 | 工具内格式化，不走 hook | AgentGo hook 语义禁止 Replace（v3 §5.8），postCall 只能观察 |
| edit_file 校验位置 | 新增 `ValidateLineAnchorsHook` | 维持 hook 作为护栏的架构一致性，与 ValidateExpectedHashHook 对称 |
| 两个 hash hook 关系 | 互斥：line_anchors 存在时跳过 expected_hash | 行级保护更细，整文件哈希变为噪声；通过 args 自检实现，hook 不互相通信 |
| RequireReadBeforeWrite 是否升级 | 不升级 | "anchors 是否新鲜"是 ValidateLineAnchorsHook 的职责，每 hook 守一块边界 |
| old_str 含 `N#HH\|` 前缀 | 工具内自动剥离 | 宽容解析，非校验逻辑 |
| 哈希算法 | `hash/crc32` IEEE，seed 通过 `crc32.Update(seed, table, content)` 注入 | 标准库内置、硬件加速、无新依赖；与参考实现 xxHash32(content, seed) 思路一致但不同算法（用户决议 2026-04-25 接受 thought-consistent 不强求 parity）|
| 字典与字符数 | `ZPMQVRWSNKTXJBYH`（16 字母低视觉歧义），2 字符 = 1 字节 | 256 种组合够低碰撞；4 字符是 overkill |
| dict 表实现 | 直接计算 `nibble[b>>4] + nibble[b&0xF]`，不预算 256 项查找表 | 参考实现预算表是 TS/V8 优化考虑；Go 直接拼字符更地道 |
| 空白行 seed 触发条件 | 行内**完全无 Unicode 字母 / 数字**（`unicode.IsLetter \|\| unicode.IsDigit`）才用 lineNumber 作 seed，否则 seed=0 | 纯标点行（如 `}`、`---`、`)`）也走 seed=lineNumber——避免"100 个 `}` 行哈希全相同"。判据是**字母数字**而不是**空白字符**——容易踩坑 |
| 行内空白规范化 | **不做**——只 strip `\r` + trimEnd 尾部空白，行内 TAB/SPACE 差异仍会产生不同哈希 | 与参考实现一致；中间空白漂移由 §7.1 痛点 1 的 `old_str` 匹配层吸收，不是哈希层职责 |
| Legacy 哈希兼容 | **不做**——`isCompatibleLineHash` 只跑新算法 | 参考实现保留 legacy（双算法都试）是为兼容已在 LLM 上下文里的旧指纹；AgentGo 是新项目无包袱，省掉一层复杂度 |
| `StripHashPrefix` 同时识别 diff `+` 前缀 | 是——`hashline 前缀计数 ≥ 50%` 优先剥 hashline，否则 `+ 前缀计数 ≥ 50%` 剥 `+` | LLM 经常把 diff 输出粘进 old_str；同时容忍 `>>>` / `+ ` / `- ` 三类 echo |
| 默认开启？ | 默认开启，`hashline_enabled` 可关 | 哈希前缀对 LLM 可读性几乎无负面，默认开启才能让 LLM 习惯 |
| 参数类型 | `line_anchors []string`（如 `["12#VK","13#QZ"]`） | 与 read_file 输出格式一致，LLM 复制粘贴即可 |
| Hook 命名 | `ValidateLineAnchorsHook`（用户决议） | 直观，与现有 hook 命名风格一致 |
| CRLF/BOM 保真 | **不在本节范围**，单独拆功能 | 用户决议（2026-04-19）——避免 PR 复杂度翻倍 |
| autocorrect 启发式 | **不做，且很长一段时间内都不做** | 用户决议（2026-04-19）——LLM 行为补丁需真实数据驱动 |
| Worker prompt 引导 | **仅靠工具描述**，不改 system prompt | 用户决议（2026-04-19）——新工具描述自说明，prompt 越短越好 |

### 7.3 Hashline 子包设计

新增 `internal/tools/hashline/` 子包，纯函数、无外部依赖：

```go
package hashline

// constants.go
const DictStr = "ZPMQVRWSNKTXJBYH" // 16 个低视觉歧义字母（无 0/O、1/l 等）
// HashLineRefRegex:    ^([0-9]+)#([ZPMQVRWSNKTXJBYH]{2})$
// HashLineOutputRegex: ^([0-9]+)#([ZPMQVRWSNKTXJBYH]{2})\|(.*)$
// HashlinePrefixRegex: ^\s*(?:>>>|>>)?\s*\d+\s*#\s*[ZPMQVRWSNKTXJBYH]{2}\| (用于 StripHashPrefix)
// DiffPlusRegex:       ^\+(?!\+)                                            (区分 ++ 双加号)

// hash.go
// ComputeLineHash 返回 2 字符哈希。
//   - 规范化：strings.ReplaceAll(content, "\r", "") + strings.TrimRight(., 空白集)
//     注意：仅处理 \r 与尾部空白，行内 TAB/SPACE 不规范化
//   - seed 触发条件：行内若有任何 unicode.IsLetter || unicode.IsDigit → seed=0；
//     完全无字母数字（含纯空白、纯标点如 `}`、`---`）→ seed=lineNumber
//   - 算法：sum := crc32.Update(uint32(seed), crc32.IEEETable, []byte(stripped))
//          b := byte(sum % 256)
//          return string(DictStr[b>>4]) + string(DictStr[b&0x0F])
func ComputeLineHash(lineNumber int, content string) string

// format.go
// FormatHashLine 单行：returns "1#VK|first"
func FormatHashLine(lineNumber int, content string) string
// FormatHashLines 整段：startLine 起每行 FormatHashLine，\n 连接
func FormatHashLines(startLine int, content string) string

// parse.go
type LineRef struct{ Line int; Hash string }
// ParseLineRef 宽容解析。归一化按以下严格顺序：
//   1. 整体 trim
//   2. 剥前缀：^(?:>>>|[+-])\s*  （>>>、+、- 加可选空白）
//   3. # 周围空白规范化：\s*#\s* → "#"  （允许 "42 # VK"）
//   4. 剥尾巴：\|.*$  （剥 "|content..."）
//   5. 再 trim
// 严格匹配 HashLineRefRegex 即返回；否则在剩余字符串中找
// LINE_REF_EXTRACT_PATTERN = ([0-9]+#[ZPMQVRWSNKTXJBYH]{2}) 的第一个匹配子串。
// 若仍失败但形如 "name#VK"（左侧非数字、右侧合法 hash），抛特定错误指明
// "left side is not a line number"。
func ParseLineRef(ref string) (LineRef, error)

// StripHashPrefix 在 old_str / new_str / lines 字段值上剥前缀。
// 算法：
//   1. 按 \n 切行，跳过空行统计 nonEmpty
//   2. 计 hashCount = 命中 HashlinePrefixRegex 的非空行数
//      计 plusCount = 命中 DiffPlusRegex 的非空行数
//   3. stripHash = hashCount > 0 && hashCount >= nonEmpty * 0.5  (注意 >= 不是 >)
//      stripPlus = !stripHash && plusCount > 0 && plusCount >= nonEmpty * 0.5
//      hashline 前缀优先于 diff +
//   4. 命中阈值后对每行 regex.ReplaceAll（不带前缀的行原样保留）
//   5. nonEmpty == 0 或两个阈值都不命中 → 原样返回
func StripHashPrefix(text string) string
```

### 7.4 read_file 输出格式变更

`hashline_enabled=true`（默认）时，输出：

```
[file] foo.go (lines 1-3 of 10)
[hash] <full-file-sha256>
---
1#VK|package main
2#QZ|
3#NP|func main() {
```

关闭时退回当前 `formatReadFileResult` 的纯内容（不带 `N#HH|` 前缀，行号通过 header `(lines X-Y of Z)` 已经表达）。

**实现位置**：`internal/tools/local_read.go` 的 `readFile` 函数，在 `formatReadFileResult` 之前对 `content` 调 `hashline.FormatHashLines`。受 `LocalReadGroup.HashlineEnabled bool` 字段控制（启动时从 cfg 注入）。

**与 FileStateCache 交互**：开关在启动时确定、运行期间不变 → cache 无需改动（缓存的格式化内容自动带前缀）。

### 7.5 edit_file 参数与行为变更

新增可选参数 `line_anchors`：

```go
r.Register("edit_file", "...", 
    schema.Object().
        String("path", "...", true).
        String("old_str", "...", true).
        String("new_str", "...", true).
        String("expected_hash", "...", false).
        StringArray("line_anchors",
            "行哈希锚点列表，如 [\"12#VK\",\"13#QZ\"]。"+
                "提供时 expected_hash 会被忽略；任一行哈希失配则拒绝并返回当前哈希。", false).
        Build(),
    g.editFile,
)
```

**工具内处理**（不变 + 增量）：
1. Hook 链跑完（含 ValidateLineAnchorsHook 的校验）
2. 工具内调 `hashline.StripHashPrefix(oldStr)` 剥离可能的 `N#HH|` 前缀
3. 余下匹配/替换逻辑不变

**注意**：`line_anchors` 仅供 hook 校验使用，工具本身不消费这个参数（除了被 schema 接收）。

### 7.6 ValidateLineAnchorsHook

新建 `internal/hook/builtin/validate_line_anchors.go`：

```go
type ValidateLineAnchorsHook struct{}

func (h *ValidateLineAnchorsHook) Name() string                     { return "validate-line-anchors" }
func (h *ValidateLineAnchorsHook) Phase() hook.ToolHookPhase        { return hook.PhasePreCall }
func (h *ValidateLineAnchorsHook) Priority() int                    { return 25 }
func (h *ValidateLineAnchorsHook) Matches(t string) bool            { return t == "write_file" || t == "edit_file" }
func (h *ValidateLineAnchorsHook) Run(hctx hook.ToolHookContext) hook.ToolHookDecision
```

Priority=25 位于 `ValidateExpectedHash`(20) 与 `RequireReadBeforeWrite`(30) 之间。完整 PreCall 链：

```
PathBoundary(10) → ValidateExpectedHash(20) → ValidateLineAnchors(25) → RequireReadBeforeWrite(30)
```

**Run 逻辑**：

1. `args["line_anchors"]` 不存在或为空 → Continue
2. `args["path"]` 缺失/类型错 → Continue（让 PathBoundary 报错）
3. 文件不存在（`os.IsNotExist`）→ Continue（新建豁免，与 ExpectedHash hook 对齐）
4. 其他 ReadFile 错误 → Abort
5. 按行切分文件内容（`strings.Split(content, "\n")`），对每个 anchor：
   - `hashline.ParseLineRef` 解析失败 → 加入 `parseErrors`
   - 行号越界 → 加入 `mismatches`
   - 重算哈希，失配 → 加入 `mismatches`
6. `parseErrors` 或 `mismatches` 非空 → Abort，AbortReason 含格式化消息

**错误消息格式**（参考 oh-my-openagent §4.4，简化为 AgentGo 风格）：

```
行哈希校验失败：1 行自读取以来已改变。请用下方更新后的 LINE#HASH 引用重试（>>> 标记失配行）。

    40#XB|  const config = loadConfig()
>>> 41#QZ|  const user = await getUser()     ← 期望 VK，实际 QZ
    42#NP|  return { config, user }

提示：复用最新 read_file / edit_file 输出里的 LINE#HASH 引用；不要凭记忆构造哈希。
```

`±2` 行上下文 + `>>>` 高亮 + 期望/实际哈希对比，让 LLM 不需要重读全文就能修正。

**消息生成算法**（与参考实现 §4.4 一致，常量 `MISMATCH_CONTEXT = 2`）：

1. 收集失配行号集合 `M`，每个 `m` 扩展为闭区间 `[max(1, m-2), min(len, m+2)]`
2. 把所有展示行号合并到去重 Set，再升序排序
3. 头部消息含失配行数 + 单复数处理（"1 行已改变" / "N 行已改变"）
4. 遍历排序后的行号生成正文：
   - 失配行：`>>> {line}#{hash}|{content}`，**hash 是磁盘上重算的当前值**（不是期望值），让 LLM 复制粘贴就能修正
   - 上下文行：`    {line}#{hash}|{content}`，4 空格缩进
   - 行号跳跃（`line > previous + 1`）时插入一行 `    ...` 表示省略
5. 错误对象同时携带 `remaps map[string]string`（旧 ID → 新 ID 映射），供下游程序化消费而非仅靠文本解析

**关键设计选择**：错误消息直接展示**重算后的当前哈希**——LLM 不用再发一次 read_file 就能拿到下一步重试用的锚点。这是参考实现里"失败是一等公民"原则的体现。

### 7.7 ValidateExpectedHashHook 互斥改造

`internal/hook/builtin/validate_expected_hash.go` 的 `Run` 入口新增一行：

```go
func (h *ValidateExpectedHashHook) Run(hctx hook.ToolHookContext) hook.ToolHookDecision {
    // 新增：line_anchors 提供时让行级 hook 接管，整文件 hash 退场
    if anchors, ok := hctx.Args["line_anchors"].([]any); ok && len(anchors) > 0 {
        return hook.ToolHookDecision{Action: hook.Continue}
    }
    // ... 其余逻辑不变
}
```

规则一句话：**"提供 line_anchors 时 expected_hash 即使存在也忽略，让行级保护接管。"**

实现采用"两个 hook 各自看 args 自决"，**不通过 hctx 通信**——保持 hook 隔离原则。

### 7.8 配置项

`internal/config/config.go` 新增：

```go
// HashlineEnabled 控制 read_file 是否在输出中附加行哈希前缀（N#HH|content）。
// 开启后 edit_file 可接受 line_anchors 参数做行级校验。
// 用 *bool 区分"未设"与"显式 false"——未设默认 true。
HashlineEnabled *bool `yaml:"hashline_enabled,omitempty" json:"hashline_enabled,omitempty"`
```

`LoadConfig` 默认值处理：未显式设置时 `cfg.HashlineEnabled = ptrTo(true)`。已有 `AgentDeclaration` 用 `*bool` 区分 nil/false 的先例。

Bootstrap 解析后，把 `*cfg.HashlineEnabled` 注入到 `LocalReadGroup.HashlineEnabled`。

### 7.9 实施步骤

| 步骤 | 内容 | 产出 |
|---|---|---|
| S1 | `internal/tools/schema/jsonschema.go` 新增 `StringArray(name, desc, required)` 方法（当前只有 String/Int/Object） | 前置基础设施 |
| S2 | 创建 `internal/tools/hashline/` 子包：constants.go / hash.go / format.go / parse.go + 对应 `_test.go` | 纯函数库 |
| S3 | `internal/tools/local_read.go` 集成哈希输出，受 `LocalReadGroup.HashlineEnabled` 控制 | read_file 新格式 |
| S4 | `internal/tools/local_write.go` `edit_file` schema 加 `line_anchors`；工具内调 `hashline.StripHashPrefix(oldStr)` | edit_file 改造 |
| S5 | 新增 `internal/hook/builtin/validate_line_anchors.go` + `validate_line_anchors_test.go` | 校验 hook |
| S6 | 改 `internal/hook/builtin/validate_expected_hash.go`：`Run` 入口添加 line_anchors 跳过逻辑 + 单测覆盖互斥规则 | 互斥规则 |
| S7 | `internal/bootstrap/bootstrap.go` 注册 `ValidateLineAnchorsHook`；构造 `LocalReadGroup` 时注入 `HashlineEnabled` | 启动集成 |
| S8 | `internal/config/config.go` 新增 `HashlineEnabled *bool`；`LoadConfig` 默认 true；YAML/JSON round-trip 测试 | 配置开关 |
| S9 | 更新 read_file / edit_file 的 schema description 与 register 时的描述文本，明确新格式与 line_anchors 用法 | LLM 指导（仅工具描述） |
| S10 | 单测：哈希稳定性 / 字典覆盖 / 空行 seed / ParseLineRef 宽容（剥 >>> + - 空格 \|content）/ StripHashPrefix 阈值 / Hook 错误消息格式 / 互斥规则 / old_str 前缀剥离 | 回归保证 |
| S11 | 属性测试（pgregory.net/rapid 或 testing/quick）：`ComputeLineHash` 同 normalized-input → 同输出；`FormatHashLine → ParseLineRef` round-trip | 不变量保证 |
| S12 | 文档：`config.example.yaml` 加 `hashline_enabled` 示例；`Archtechture.md` 工具表 + Hook 链图同步；`nextUpgrade_v3.md` §2-§5 占位关闭 | 文档同步 |

### 7.10 不在本节范围

- **CRLF/BOM 保真**：参考实现的 `file-text-canonicalization.ts`（BOM 信封 + \r\n 保真）是独立关注点，单独拆出为下一次小功能。当前 `os.ReadFile/WriteFile` 行为不变。
- **Autocorrect 启发式**：参考实现的 `autocorrect-replacement-lines.ts`（单行多行展开、缩进恢复、整行还原）不做，且"至少从现在开始的很长一段时间都不做"（用户决议 2026-04-19）。
- **位置型操作模型（replace/append/prepend + pos）**：不替换 `edit_file` 现有 `old_str/new_str` 语义。行哈希只做**校验层叠加**，不改编辑模型。
- **批量编辑（edits 数组）**：参考实现允许一次 call 多条 edits + 去重 + 底向上排序。AgentGo 保持单次 edit 语义，多处修改靠多次调用——简单、可组合、与现有 prompt 对齐。
- **Formatter 触发**：参考实现写入后触发用户配置的 formatter（prettier / gofmt 等）。AgentGo 让用户/Worker 自行调 `run_shell`，工具层不承担格式化责任。
- **生成带哈希 diff 输出**：参考实现有 `generateHashlineDiff`。AgentGo 目前 edit_file 只返回"字节变化 +N/-M"，不加这一层。
- **Per-edit 可观测性**：参考实现输出 `additions / deletions / firstChangedLine / noopEdits / deduplicatedEdits` 给 TUI。AgentGo 无 TUI，不加。
- **System prompt 引导**：仅靠工具描述（`schema.String(..., desc, ...)` 与 `r.Register(..., desc, ...)` 文本）让 LLM 自然学会使用，不动 worker / explorer 的 system prompt。

### 7.11 验证计划

| 优先级 | 场景 | 测试方法 | 需要多 worker |
|---|---|---|---|
| V1 | 哈希输出正确性 | 调 read_file，确认输出含 `N#HH\|` 前缀且重复调用哈希稳定 | 否 |
| V2 | 匹配成功路径 | LLM 提供正确 line_anchors 调 edit_file，校验通过 | 否 |
| V3 | 匹配失败路径 | 修改文件后用旧 anchors 调 edit_file，确认 Abort + 错误消息含新哈希 + ±2 行上下文 | 否 |
| V4 | 互斥规则 | 同时提供 line_anchors + expected_hash（expected_hash 故意错）→ 应放行，line_anchors 生效 | 否 |
| V5 | old_str 前缀剥离 | 让 LLM 把读到的 `42#VK\|foo` 整行粘回 old_str，确认仍能匹配 | 否 |
| V6 | 新建文件豁免 | 对不存在文件 write_file + line_anchors → hook 放行 | 否 |
| V7 | 多 agent 粒度优化（核心价值） | WorkerCount=2，两个 worker 读同一文件，A 改第 5 行，B 用行锚点改第 50 行 → 均成功 | **是** |
| V8 | 配置关闭 | `hashline_enabled: false` 时 read_file 退回老格式；edit_file 的 line_anchors 参数仍可用但 LLM 看不到带 hash 输出 | 否 |
| V9 | 属性测试 | `hashline.ComputeLineHash` 在等同 normalized 输入下输出稳定；FormatHashLine ↔ ParseLineRef round-trip | 否 |

V7 是验证本方案核心价值（粒度优化）的关键场景，必须实测通过。

---

## 8. 跨子系统装配的自动化护栏（已拆分）

> 状态：📦 **已拆分**（2026-04-25）
> 优先级：P2（原）
> 历史触发：2026-04-19 一晚单任务测试同时暴露 3 个"装配漏接"缺陷（Trace CLI 路径脱钩、
> history.jsonl 断链、Finalization 短路 emit 漏）；2026-04-20 复盘补充第 4 个
> （Mail `chain_depth` 全程为 0）。共同模式："零件单测都过 → 装配握手位无人测 → 漏接到上线"。

> **拆分说明（2026-04-25）**：原 §8.2 的 4 条建议复盘后，3 条具备项目独有的不可
> 替代价值，2 条属于行业惯例或被 feature-specific 计划吸收，无需独立条款。已分别
> 迁入：
>
> - **"完成"定义包含"实际启动跑一次 + 断言产物"**（原 §8.2 第 2 条）→ `CLAUDE.md`
>   "Shipping conventions" 段（项目级硬性约定）
> - **修复 PR 必须同步 `KNOWN_ISSUES.md` 状态**（原 §8.2 第 4 条后半段）→ 同上
>   `CLAUDE.md` "Shipping conventions" 段
> - **终结路径 `trace.Emit` 对称性作为永久不变量**（原 §8.2 第 3 条）→ §11.8 实施
>   步骤 S11（§11 重构后挂在统一 runner `internal/agent/runner.go` 上，扫描目标
>   比原"扫 agent.go"更稳定）
> - "大功能要 E2E 烟测"（原 §8.2 第 1 条）：被 §7.11 / §10.9 / §11.8 各自的
>   feature-specific 验证计划吸收，不在项目层重复声明
> - "红态回归测试先于修复"（原 §8.2 第 4 条前半段）：行业 TDD 惯例，不在项目层
>   独立编码
>
> 原 §8.1 复盘表与 §8.3 优先级排序已删除；上文"历史触发"段保留事故的精简引用，
> 完整原文可在 git history 中查阅。本章作为占位保留，便于历史链接与状态表索引。

---

## 9. 运行时致命错误快速失败 + 启动期轻量连通性检查

> 状态：📝 设计定稿（2026-04-25）
> 优先级：P2
> 触发来源：2026-04-20 手工验证时 LLM 服务器关机导致 scheduler 无限重试 166+ 次（~25 分钟）
> 设计原则：**用户为配置负责，系统在不可恢复错误时立即中断而非空转**

### 9.1 问题描述

当前 [internal/bootstrap/bootstrap.go](../../internal/bootstrap/bootstrap.go) 初始化 LLM client 时只检查配置字段是否齐全，**不验证 `llm_base_url` 是否真正可达**。若启动时 LLM 服务不可达：
- 启动阶段打印"系统就绪"，看起来一切正常
- 用户提交 prompt 后 scheduler 发起 LLM 调用 → `connection refused`
- `handleFailure` → `RetryRollback`；scheduler 重试 → 再次失败 → 再次重试
- 日志刷屏"重试 #N"直到手动 Ctrl+C

用户视角：**启动成功 ≠ 真正可用**。

### 9.2 设计决策：不做启动期模型可用性验证

**决策**：v4 版本**取消启动阶段对模型名、模型可用性的探测**。理由：

1. **厂商实现差异大**：OpenAI 兼容 API 的 `/models` 端点不是强制标准，不同厂商的实现、返回格式、权限模型差异极大，探测逻辑无法统一。
2. **用户应为配置负责**：`setting.yaml` 中的 `model` 字段是用户显式指定的，系统不应当承担"帮用户检查拼写"的责任。拼写错误或模型不存在，应当在**第一次调用时暴露**。
3. **避免过度工程化**：启动期探测 `/models` 需要处理分页、别名、权限、格式差异等边界情况，投入产出比低。
4. **运行时切换是合理需求**：未来版本可能支持任务级模型切换或热重载，启动期强验证会成为阻碍。

**替代方案**：把"预防空转"的重心从"启动期探测"转移到**"运行时遇到不可恢复错误立即中断"**。

### 9.3 运行时错误码策略：可恢复 vs 不可恢复

LLM API 返回的 HTTP 状态码分为两类：**可恢复**（应重试）和**不可恢复**（应立即中断并报告）。

| 状态码 | 分类 | 行为 | 理由 |
|--------|------|------|------|
| 200 | 成功 | 正常处理 | — |
| 408, 429, 502, 503, 504 | **可恢复** | SDK 内部指数退避重试 | 临时网络波动、限流、网关超时，重试可能成功 |
| 400 | **不可恢复** | **立即中断任务**，解析错误体给出诊断 | 请求参数错误（含 model 名拼写错），重试 100 次也一样失败 |
| 401 | **不可恢复** | **立即中断任务**，提示 API key 无效 | 鉴权失败，重试无用 |
| 403 | **不可恢复** | **立即中断任务**，提示权限不足 | 该 key 未开通此模型权限，重试无用 |
| 404 | **不可恢复** | **立即中断任务**，提示 base_url 或路径错误 | 端点不存在，重试无用 |
| 405 | **不可恢复** | **立即中断任务** | HTTP 方法不匹配，通常是 URL 配置错误 |
| 500 | **不可恢复** | **立即中断任务**，提示服务端内部错误 | 服务器端 bug，客户端重试无法修复 |

**关键改动**：当前 `classifySDKError`（`internal/llm/client.go`）把 429/5xx 标记为 `ErrRecoverable`，401/403/404 标记为 `ErrUnrecoverable`。这个分类是对的，但**`ErrUnrecoverable` 在 agent.go 的处理路径中只是"记录日志后继续重试"，没有真正中断**。

需要修改 `agent.go` 的 `handleFailure`：当遇到 `ErrUnrecoverable` 时，**不进入 RetryRollback，直接标记任务失败并终止当前轮次**。

```go
// agent.go handleFailure 伪代码
switch err.(type) {
case *llm.ErrRecoverable:
    // 网络波动、限流等 → 指数退避重试
    return a.retryWithBackoff(task, history, err)
case *llm.ErrUnrecoverable:
    // 400/401/403/404/405/500 → 立即失败，不再重试
    a.emitTaskFailed(task, err)
    return err
case *llm.ErrBadResponse:
    // 响应格式异常 → 是否可恢复视具体情况
    // 如 finish_reason=length 属于可恢复（换模型或截断后重试）
    // 如 JSON 解析失败属于不可恢复
}
```

### 9.4 错误诊断：从响应体解析具体原因

厂商的错误响应体通常包含结构化信息。解析后给出**比 HTTP 状态码更具体**的提示。

v4 版本假设所有接入的 LLM provider 均遵循 OpenAI 兼容 API 的错误响应格式。不针对个别厂商的响应差异做特殊适配——精力有限，优先保证主链路稳定。

**标准错误响应格式（OpenAI 兼容）**：
```json
{
  "error": {
    "message": "The model `gpt-5` does not exist or you do not have access to it.",
    "type": "invalid_request_error",
    "param": null,
    "code": "model_not_found"
  }
}
```

**诊断映射规则**（基于上述标准格式解析）：

| 解析信号 | 诊断结论 | 向用户输出的提示 |
|---------|---------|----------------|
| `code == "model_not_found"` 或 message 含 `"model"+"not found"` | **模型名错误** | `"模型名 '%s' 不存在。请检查 setting.yaml 中的 model 配置。当前使用的是 endpoint: %s"` |
| `code == "invalid_api_key"` 或 status == 401 | **API key 无效** | `"API key 无效或已过期。请检查 setting.yaml 中的 api_key 或环境变量。"` |
| `code == "insufficient_quota"` | **配额不足** | `"API 配额不足。请检查账户余额或联系 provider。"` |
| status == 404 && path 含 `/chat/completions` | **base_url 路径错误** | `"端点返回 404。请检查 setting.yaml 中的 base_url 是否包含正确的 API 路径（如 https://api.deepseek.com/v1）。"` |
| status == 404 && host 不可达 | **base_url 主机错误** | `"无法连接到 %s。请检查网络连通性或 base_url 配置。"` |
| `code == "context_length_exceeded"` | **上下文超限** | `"请求超出模型上下文上限（%d tokens）。当前历史长度约 %d tokens，请考虑降低 context_limit 或开启更积极的历史压缩。"` |
| 其他不可恢复错误 | **未知** | `"LLM 调用失败: %s (status=%d, code=%s)。完整响应: %s"` |

**实现位置**：在 `internal/llm/client.go` 的 `classifySDKError` 中增加响应体解析逻辑，把解析结果封装进 error 结构体，供上层 `agent.go` 打印。

### 9.5 启动期可选的轻量连通性检查

虽然不做模型可用性验证，但保留一个**可选的、最轻量的 TCP 连通性检查**，用于捕获"服务完全没启动"这种明显错误。

```yaml
# v4 setting.yaml
startup_probe: "tcp"           # 默认 "tcp"，可选 "off"
startup_probe_timeout_sec: 5   # 单次探测超时，默认 5 秒
```

**只做一件事**：对 `llm.base_url` 的 host:port 做 `net.DialTimeout`，验证 TCP 层是否可达。

**不做的事**：
- 不验证 API key
- 不验证 model 名
- 不发 HTTP 请求
- 不解析响应

**失败处理**：
- TCP 不通 → 打印明确错误（`"无法连接到 LLM 服务端 %s，请确认服务已启动且网络可达"`）
- 默认策略：继续启动（因为可能是临时网络问题，且运行时错误处理会捕获真正的调用失败）
- 可配置为硬失败：`startup_probe_failure_action: "warn" / "exit"`，默认 `"warn"`

#### 9.5.1 启动期配置 banner

probe 之前先打印一份配置摘要 banner，让用户视觉核对 YAML 是否被正确读取。banner 与
probe 同源——都在启动期执行，输出顺序为"banner → probe → 启动结果"，组成完整的
启动可观测性。

**字段选择原则**：
- 列出**所有用户在 YAML 中显式写过**的字段，让用户能直接对照检查
- `api_key` 强制脱敏：仅显示前 4 字符 + `***` + 后 4 字符 + 长度，避免日志/截图泄露
- 内置常量（scheduler 工具集 / 提示词 / 行为参数等收窄项）不重复打印，仅标注"(built-in)"

**示例输出**：

```
=== AgentGo Startup Configuration ===
LLM Endpoint:     https://dashscope.aliyuncs.com/compatible-mode/v1
LLM API Key:      sk-A***xyz9 (length=40)
Default Model:    qwen3.6-plus
Timeout:          120s

Agent Kinds:
  - scheduler   model=qwen3.6-plus    (tools/prompt/behavior 全部 built-in，详见 §11.5.5)
  - worker × 3  model=qwen3.6-plus    profile=worker_standard   loops=10  ctx=16000
  - explorer    model=qwen3.6-flash   profile=explorer_full     loops=5   ctx=8000

=== Startup LLM Probe (level=tcp, timeout=5s) ===
  [OK]   dashscope.aliyuncs.com:443  (3-way handshake 87ms)
  best-effort connectivity check; auth/model validity verified at first runtime call

System ready, awaiting user input...
```

**实施位置**：`internal/bootstrap/bootstrap.go` 在配置校验通过后、实例化共享 infra
之前调用 `printStartupBanner(cfg)`；probe 在 banner 之后立即执行。

**调试价值**：YAML 读取错误（譬如 anchor 写错、缩进错位、env 变量未替换）在 banner
上立刻可见——用户不用解析 trace 也能定位"我配的明明是 X，banner 上显示 Y"这类问题。

#### 9.5.2 TCP probe 的局限（best-effort 性质）

TCP probe 只验证 host:port 的 3-way handshake 能完成，**不等于"endpoint 真正可用"**。
以下场景会出现 probe 与实际可用性不一致：

| 场景 | 表现 | 影响 |
|---|---|---|
| **mTLS endpoint**（双向 TLS）| TCP 握手通，但 TLS 需要客户端证书 → probe pass，runtime 失败 | False positive（probe 给绿灯但实际不通），与不做 probe 等价，无损失 |
| **L4 负载均衡 + 后端不健康** | TCP 到 LB 通，后端实际无服务 | False positive，无损失 |
| **CDN / WAF L4 接受所有连接** | probe pass 但 L7 可能仍拒绝 | False positive，无损失 |
| **SYN cookies / 限流丢包** | probe fail 但实际能通 | False negative——多一行误导性 warning，但 warning-only 模式不阻止启动 |
| **企业网关 / 代理** | probe fail 但实际能通 | False negative，同上 |

**关键原则**：probe 是 advisory 不是 authoritative——失败仅 warning，启动继续，运行时
重试机制（§9.6）才是真正的健康判定。即使 false negative 误警告，用户使用 `startup_probe: off`
即可关闭。

**warning 文案明确说明 best-effort 性质**：
```
[WARN] TCP probe to dashscope.aliyuncs.com:443 failed: connection refused
       This is a best-effort connectivity check. If you believe the endpoint is
       reachable (e.g. mTLS / corporate gateway / CI mock), set startup_probe: off
       in setting.yaml. Auth/model validity is always verified at first runtime call.
```

主流公开 LLM API（OpenAI / Anthropic / DashScope / DeepSeek / Groq 等）的 HTTPS endpoint
都遵循标准 TCP 443，probe 准确反映可达性；自建 vLLM / Ollama / LocalAI 同理。**TCP probe
的盲区主要存在于 mTLS 企业部署 + 特殊网关配置**——这类场景的用户通常知道自己在做什么，
直接 `startup_probe: off` 即可。

### 9.6 可恢复错误的重试上限（防止无限空转）

§9.3 已解决不可恢复错误（400/401/403/404/405/500）的立即中断问题，但 2026-04-20 的事故根源是 `connection refused`——它被 `classifySDKError` 归类为 `ErrRecoverable`（网络错误），当前逻辑会无限重试。

**设计**：
- **404 / 路径错误**：已在 §9.3 中列为不可恢复错误，直接中断（服务器收到了请求但路径不存在，重试 100 次也一样失败）
- **connection refused / 429 / 5xx 等其他可恢复错误**：给重试机会，但设置**单任务级最大重试上限**，防止无限空转

```go
const maxRecoverableRetries = 3  // 同一任务中，ErrRecoverable 最多再重试 3 次

func (a *Agent) handleFailure(task *Task, history []HistoryEntry, err error) error {
    switch e := err.(type) {
    case *llm.ErrRecoverable:
        if task.RetryCount >= maxRecoverableRetries {
            return fmt.Errorf("可恢复错误重试 %d 次后仍失败，放弃: %w", maxRecoverableRetries, e)
        }
        task.RetryCount++
        return a.retryWithBackoff(task, history, e)
    case *llm.ErrUnrecoverable:
        // §9.3：400/401/403/404/405/500 立即失败
        a.emitTaskFailed(task, e)
        return e
    }
}
```

**为什么是 3 次**：
- SDK 内部已对 connection errors / 429 / 5xx 自动重试 2 次
- AgentGo 层再允许 3 次重试，总尝试次数 = 6 次
- 对临时故障（服务重启、网络抖动）6 次尝试足够恢复
- 对完全宕机，6 次失败后立即止损，不再空转

**不实现复杂熔断的原因**：单任务重试上限已能阻止 2026-04-20 式的无限循环，无需引入全局计数器和状态机。

### 9.7 与旧版 §9 的差异（决策记录）

| 旧版设计（已废弃） | 新版设计（当前） | 理由 |
|---|---|---|
| 默认 `/models` 探测模型可用性 | 取消 `/models` 探测 | 厂商实现差异大，格式不统一，误杀风险高 |
| per-kind degraded 降级启动 | 取消 degraded 概念 | 用户为配置负责，不试图在启动期做智能降级 |
| 启动期硬失败（`os.Exit(1)`） | 启动期仅 warning，运行时不可恢复错误才中断 | 避免启动期过度敏感，把失败暴露推迟到第一次真实调用 |
| did-you-mean 模糊匹配 model 名 | 取消 | 不应由系统承担"帮用户检查拼写"的责任 |
| `--skip-llm-probe` 命令行旗标 | 保留 `--skip-startup-probe` | 保留绕过能力，但只跳过 TCP 探测 |

### 9.8 落地价值

- **节省 wall-clock**：不可恢复错误（400/401/403/404/500）不再进入重试循环，25 分钟空转变为 1 秒失败报告
- **降低排查成本**：运行时错误带具体诊断（"模型名拼写错" vs "API key 无效" vs "base_url 不通"），用户一眼知道改哪里
- **减少 token 浪费**：不再对注定失败的请求重复重试
- **简化实现**：取消 `/models` 解析、degraded kind 路由、did-you-mean 模型匹配等复杂逻辑

### 9.9 关联条目

- 与 §11 的 `context_limit` 配合：当请求因上下文超限失败（`context_length_exceeded`）时，诊断提示应引导用户调整 `context_limit` 或 `compact_token_threshold`
- 与 §10（Did-You-Mean）的关系：§10 的模糊匹配只用于**工具参数**（文件名、路径），不用于 model 名——model 名由用户显式配置，系统不做猜测
- 与 KNOWN_ISSUES.md "Scheduler 网络失败无限重试"老 bug 的关系：**本节直接修复该 bug**（不可恢复错误不再重试），而非仅做启动期探测互补

---

## 10. 工具调用错误恢复（Did-You-Mean 候选提示）

> 状态：📝 规划中（2026-04-24 触发记录）
> 优先级：P2
> 触发来源：2026-04-23 实战日志暴露 Explorer 连吃 5 次 grep_search 空结果 result_len=18；2026-04-24 已完成"诊断消息层"修复（工具行为说明），但用户提出应进一步叠加"意图拯救层"——空结果时主动给近似匹配候选，让 Agent 一次看到"Did you mean X?"直接换用正确参数。

### 10.1 背景与定位

2026-04-24 已完成 grep_search / glob_search 空结果的**行为说明层**：告诉 LLM "扫描了多少文件、跳过了什么、pattern 是字面子串非正则"。这解决了"工具为什么返回空"的问题。

本节要叠加的是**意图拯救层**：在 LLM 拼写打错、命名记混、路径层级猜错时，工具自己基于"当前目录下实际存在什么"给出 Top-K 近似候选，让 LLM 一眼看到"`Archtechture.md` 不存在，但有 `Architecture.md`——你是不是想找这个？"。

两层的关系：**行为说明**（现在的诊断）+ **意图拯救**（本节）= 完整的"空结果恢复信息"。前者回答"为什么 0"，后者回答"你或许真正想要的是什么"。

#### 10.1.1 主题定位：工具调用错误恢复的一个分支

§10 实际定位是"工具调用错误恢复"主题的一个分支——不是覆盖所有错误，而是其中
"输入错但合理 + 能静态找到候选"这一子集。其他错误类型由其他机制承接（详见
§10.7 多层分工）。

**§10 处理范畴的判定准则**（两条均满足才纳入 §10）：

1. **LLM 输入错但合理**：参数语法合法、语义上是真实尝试，只是与实际状况不匹配
   （不是参数缺失、不是恶意越界、不是时机问题、不是权限问题）
2. **能从已有数据中静态识别候选**：候选空间是工具调用前后已经在内存里 / 一次
   syscall 能拿到的实体集合，不需要新的运行时调用

满足两条 → §10 给 Did-You-Mean。缺任一条 → 由其他机制承接，**不强行塞进 §10**——
让"输入修正建议"语义保持单一。

#### 10.1.2 错误分类与责任分工速查

| # | 错误类型 | §10 范畴 | 归属机制 |
|---|---|---|---|
| 1 | 文件名拼写错（`Archtechture.md`）| ✅ In | §10.2 glob_search 候选 |
| 2 | 路径层级猜错 | ✅ In | §10.2 read_file / list_dir 候选 |
| 3 | 工具名 typo（`read_files`）| ✅ In | §10.2 工具调度层候选（新增）|
| 4 | grep_search 空结果（仅文件名候选层）| ✅ In | §10.2 文件名层（token 层暂缓）|
| 5 | `edit_file` `old_str` 找不到 | ❌ Out | **§7 Hashline**——行哈希失败时返回当前哈希 + ±2 行上下文 |
| 6 | `edit_file` 多匹配 | ❌ Out | **§7 Hashline**——若 hashline 不能解决，留空待后续 |
| 7 | 必填参数缺失 / 类型错 | ❌ Out | **Schema 校验层**——工具调用前拒绝 |
| 8 | 行号越界、参数值越界 | ❌ Out | **Tool Hook 系统**——参数验证职责，调用前拦截给出明确错误 |
| 9 | 文件被占用（Roster 锁）| ❌ Out | 重试策略——输入对、时机不对 |
| 10 | `expected_hash` 冲突 | ❌ Out | 重试 + 重读——输入对、文件已变 |
| 11 | 工具未授权（kind 不持有该工具）| ❌ 不可能场景 | **registry-level filter** 已在注册阶段过滤——参考 [agent.NewToolRegistryWithAllowlist](../../internal/agent/tool_registry.go#L37)：allowlist 之外的工具不进 ToolRegistry，LLM 视野不可见，因此无法触发 §10 也无法触发 hook |
| 12 | `run_shell` 命令不存在 / 黑名单拦截 | ❌ Out | 直接错误消息——不建议命令名 fuzzy（安全风险）|
| 13 | `web_fetch` 网络错误 / 超时 | ❌ Out | 重试策略——无替代输入可建议 |
| 14 | `web_search` 配额耗尽 / 鉴权失败 | ❌ Out | §9 启动期探测前置 / 运行时管理员介入 |

**关键设计决议**：
- **#5、#6 归 §7 Hashline**：行哈希失败已经天然附带当前哈希 + ±2 行上下文，本身就是
  "输入对但文件已变"的恢复机制。若 hashline 仍无法解决某些边角，留空待后续，不在
  §10 重复造轮。
- **#8 行号越界归 Tool Hook**：参数值校验属 hook 系统职责（在调用前拦截 + 给明确
  错误），不需要 fuzzy 候选——文件长度是确定值而非候选集合。
- **#11 工具未授权归 registry-level filter**：[agent.NewToolRegistryWithAllowlist](../../internal/agent/tool_registry.go#L37)
  在工具注册阶段静默跳过 allowlist 之外的工具，使其根本不进 ToolRegistry——LLM 视野
  不可见、调用不可达、无运行时机制可触发。**这意味着原本可能担忧的"已注册但当前
  kind 没权限"场景在当前架构下不可能发生**。唯一仍走 §10 的是 #3 工具名 typo（已注
  册工具集合内查 typo 候选）。

### 10.2 覆盖范围的分层

MVP 包含前三类工具 + grep_search 文件名层 + 工具调度层；grep_search token 层延后：

| 错误类别 | 候选空间 | 实现成本 | MVP 包含 |
|---|---|---|---|
| **glob_search**（文件名拼写错）| 本次 Walk 已扫的所有相对路径（≤10k 条）| 低（复用 Walk 结果）| ✅ |
| **read_file**（路径不存在）| 失败路径的父目录 ReadDir 列表 | 极低（1 次 syscall）| ✅ |
| **list_dir**（路径不存在）| `Dir(path)` + `Dir(Dir(path))` 的 ReadDir | 低（≤3 次 syscall）| ✅ |
| **工具调度层**（工具名 typo）| 当前 kind 持有的已注册工具名集合（5-15 条）| 极低（复用 ToolRegistry）| ✅ |
| **grep_search**（文件名候选层）| 本次扫描的文件路径列表 | 低（复用扫描列表）| ✅（仅文件名层）|
| **grep_search**（token 候选层）| 扫描文件的 token/标识符（百万量级）| 高（需建 trigram 索引 + 去噪）| ❌（暂缓）|

**grep_search 双层处理**：文件名层（"你 grep 没匹配，但提到 `auth` 关键字的文件
名有 X、Y、Z"）成本与 glob_search 相当，纳入 MVP；token 层（"你 grep 的关键字
没出现，但相似 token 有 Y"）需要 trigram/BK-tree 索引和频率去噪（不能把 `the`
`func` 当候选），等 MVP 实战数据收集后再决定是否启动。

**工具调度层**（错误分类表 #3）：在 LLM 调用工具时若工具名不存在于当前 kind 的
ToolRegistry，调度层捕获失败前先调 `suggest.Suggest` 在已注册工具名集合中找候选——
典型场景 LLM 把 `read_file` 写成 `read_files` / `readFile` / `read-file`。失败消息
从"tool not found"升级为"tool 'X' not found. Did you mean: read_file? web_search?"。
注意：仅在工具**未注册**时触发；**未授权**（已注册但当前 kind 不持有）走 Tool Hook
拒绝路径，不给 Did-You-Mean（详见 §10.7 L2 / §10.10）。

### 10.3 算法选择（MVP）

**决策：采用 [`github.com/sahilm/fuzzy`](https://github.com/sahilm/fuzzy) 作为模糊匹配底座，不自造轮子。**

理由：
- fzf 风格子序列匹配 + 连续性/位置加权打分，**直接就是 Did-You-Mean 语义**——API 一步到位：`fuzzy.Find(pattern, candidates) Matches`，返回已按 score 排序的匹配列表
- 真实采用方包括 fzf、lazygit 等，参数调优经过大量真实用户打磨（连续命中加权、边界加权、大小写策略等），自写必定复现不到这个成熟度
- 单一依赖、零传递依赖、约 400 行代码可审计
- 副产物 `Match.MatchedIndexes []int` 可以用来在输出里**高亮命中字符**（如 `m*u*lti*A*gent_upgrade_plan.md`），比"编辑距离=2"的数字化 reason 对 LLM 更直观

trade-off 认知：
- fzf 风格对**纯拼写错乱序**（`Archtechture` ↔ `Architecture`——字母顺序变了）的打分会弱于 Levenshtein。但 Did-You-Mean 的候选池通常 << 1000 条，真候选即使 score 偏低仍会排进 Top-3；实战数据不佳时可叠加一层 Levenshtein 兜底，但 **MVP 阶段只用 sahilm/fuzzy**
- 不引入 [`agnivade/levenshtein`](https://github.com/agnivade/levenshtein) 作为补充——原则上一个问题只挑一个库，避免同类能力重复依赖；fzf 风格覆盖 80% 用例已经足够

不纳入的算法选项（决策记录）：
- **自写 Levenshtein + 子串匹配**：错误的自举冲动，距离阈值 / 子串长度下限 / CJK rune 边界等细节坑已被上游库填过
- **[`lithammer/fuzzysearch`](https://github.com/lithammer/fuzzysearch)**：同类功能但 API 不如 sahilm/fuzzy 成熟，采用方少
- **Token 分词 + 共享 token 计数**：MVP 不做，等 sahilm/fuzzy 实战有证据不够用再叠加
- **Trigram 索引**：扫描目标 > 10k 时才必要，MVP 阶段线性扫描 O(N) 够用

### 10.4 候选构造范围（避免误导）

关键原则：**候选必须来自"LLM 看得到的、实际存在的"实体**，不能跨越它当前查询的范围去瞎提示。

- **glob_search**：候选 = 本次 Walk 已扫到的所有路径（工具已经有这份列表），对 pattern 的"文件名主干"（去掉 `**/`、`*`、`.ext`）做相似度。pattern 是 `**/Archtechture.md` → 主干是 `Archtechture.md` → 找相似文件名
- **read_file**（path 错）：候选 = `filepath.Dir(path)` 下的文件列表。**不跨目录提示**，否则 `internal/foo/x.go` 不存在时建议 `cmd/bar/x.go` 会误导
- **list_dir**（path 错）：候选 = `Dir(path)` 下的目录 + `Dir(Dir(path))` 下的目录（覆盖"层级猜错一层"）
- **grep_search**（空结果，文件名层）：候选 = 本次扫描的文件路径列表，对 pattern 找相似文件名。仅做文件名层——不进入文件内容做 token 匹配（详见 §10.2 grep_search 双层处理）
- **工具调度层**（工具名 typo）：候选 = **当前 kind ToolRegistry 中已注册的工具名集合**（5-15 条，allowlist 已在注册阶段过滤）。一次 fuzzy 即可。**关键边界**：仅当工具名完全不存在于 ToolRegistry 时触发；unauthorized 工具因 [registry-level filter](../../internal/agent/tool_registry.go#L37) 不进 registry，对 LLM 不可见，不需要 §10 处理也不需要 hook 处理

### 10.5 输出格式

空结果消息在现有诊断基础上追加一段（仅当 sahilm/fuzzy 返回非空 Matches 时——由库内部的子序列匹配语义自然筛出"字符全命中"的候选，MVP 阶段不额外加 score 阈值）：

```
未找到匹配 "Archtechture.md" 的文件（扫描 128 个文件，根目录=.，隐藏目录已跳过）。
若意外为空：1) 确认 pattern 符合 glob 语法（** 递归、* 单层、? 单字符）；...

Did you mean:
  - Arch[i]tecture.md
  - [arch]itecture_v3.md
  - docs/[arch]itecture/overview.md
```

**高亮策略**：用 sahilm/fuzzy 返回的 `Match.MatchedIndexes` 在每段连续命中字符两侧加 `[...]`（方括号配对），直观展示"为什么推荐这条"。**不用 `*` 或 `**`** —— 前者在 markdown 渲染上下文会被当作斜体指令，后者是粗体，都容易与候选本身的字面字符混淆；方括号成对出现语义明确、与文件名字符冲突概率低。

约束：
- 最多 3 条候选（对应 `suggest.Suggest(pattern, candidates, 3)`）
- **高亮完整性优先于长度省略**：候选本身展示必须包含全部 MatchedIndexes 对应的字符——超过 80 字符时允许在**首个命中字符之前**或**末个命中字符之后**做 `...` 截断（如 `.../nested/[arch]itecture/overview.md`），但决不能在命中段内省略
- **不设额外 score 阈值**：sahilm/fuzzy 返回的 Matches 本身就是"至少能完成子序列匹配"的集合，空列表即"实在没有合理候选"；MVP 不再叠加 `minScore`，等实战出现"低质候选刷屏"证据再调
- 调用方逻辑：`matches := fuzzy.Find(pattern, candidates); if len(matches) == 0 { 不追加 Did-you-mean 段 }`

### 10.6 性能

- **glob_search**：Walk 已扫过的路径列表已有现成的，调 `fuzzy.Find(pattern, paths)` 复杂度约 O(N·L²)（fzf 风格子序列匹配最差情况，N=候选数、L=平均候选长度），**预期 < 20ms（N=10k, L=30），以 S7 benchmark 实测为准**——这是预期值不是已知 benchmark 数
- **read_file / list_dir**：仅失败路径触发一次 `os.ReadDir`，成功路径零开销
- **MVP 不建索引**：纯线性扫描足够。若 S7 benchmark 显示 N > 10k 时延迟感知明显，再考虑换库或叠加前置筛选（如按首字符桶分组缩小 N）——sahilm/fuzzy 本身不提供索引选项

### 10.7 工具调用错误恢复的多层分工

工具调用错误恢复不是单层职责——按错误特征不同，分别由 4 层机制承接，§10 是其中
一层（L3）。各层不互斥但归属唯一——同一类错误只走一层，避免出现"同一错误两层都
给提示，互相矛盾或重复"。

| 层 | 归属 | 职责 | 典型错误（错误编号见 §10.1.2）| §10 范围 |
|---|---|---|---|---|
| **L1: Schema 校验** | 工具调度前的参数校验 | 拒绝不合法参数（必填缺失、类型错）| #7 | ⛔ 不属于 §10 |
| **L2: Tool Hook 拦截** | Hook 系统（v3 §5.8）| 越界路径 / 参数值越界 / 违反契约 → 拒绝并警告 | #8 | ⛔ 不属于 §10（Hook 自身职责）|
| **L3: 工具自身反馈** | 工具函数内部 / 调度层 | 行为说明（2026-04-24 已完成）+ Did-You-Mean 候选（**本节**）| #1-4 | ✅ 本节核心 |
| **L4: 编辑层兜底（Hashline）** | §7 Hashline 增强 | `edit_file` `old_str` 不匹配 / 多匹配时返回当前哈希 + ±2 行上下文 | #5、#6 | ⛔ 由 §7 承接 |

**层间分工的核心规则**：
- **L1/L2 在调用前拦截**——LLM 错误根本没机会到达工具内部
- **L3/L4 在调用进入工具后给"恢复线索"**——L3 处理"输入错但合理"（候选可建议），
  L4 处理"输入是对的但状态已变"（重读 + 重试）
- **同一类错误归属唯一**——例如工具未授权（#11）由 [registry-level filter](../../internal/agent/tool_registry.go#L37)
  在注册阶段消除（unauthorized 工具不进 ToolRegistry，LLM 视野不可见），不走 L2 也不走 L3
- **运行时错误不属于任何层**（#9、#10、#13、#14）——重试策略 / 启动期探测 / 管理员介入

Did-You-Mean 归到 L3 的具体理由：
- 候选构造需要**工具内部状态**（glob_search 的 Walk 结果、read_file 的 ReadDir、
  ToolRegistry 已注册名集合）——hook 从 args 层面拿不到这些
- 与 v3 §5.8 决议一致：hook 不做 Replace，不改写工具返回值；"给返回值加
  Did-You-Mean 段"本质是增强返回值，不是拦截
- 各层单一职责清晰：L1/L2 守边界、L3 提结果质量、L4 容编辑漂移

### 10.8 共享基础设施

抽 `internal/tools/suggest/` 子包做**薄适配层**——算法全部由 `sahilm/fuzzy` 承担，本子包只负责"候选构造约束 + 命中字符高亮 + 输出格式化"：

```go
package suggest

import "github.com/sahilm/fuzzy"

// Suggest 对 candidates 按 fzf 风格子序列匹配打分，返回 Top-K 高亮标记后的字符串。
// - pattern: LLM 传入的查询（如 "Archtechture.md"）
// - candidates: 当前查询范围内实际存在的实体（glob_search 的 Walk 结果 / ReadDir 列表）
// - k: 最多返回几条（建议 3）
// 返回空切片表示无合理候选（调用方应不显示 "Did you mean" 段）。
func Suggest(pattern string, candidates []string, k int) []string

// highlightMatch 用 fuzzy.Match.MatchedIndexes 在每段连续命中字符两侧加 [...] 方括号，
// 超长路径（>80 字符）时在首个命中字符之前 / 末个命中字符之后截断，
// 绝不在命中段内省略（高亮完整性优先，见 §10.5）。
func highlightMatch(m fuzzy.Match) string

// FormatForToolMessage 把 Suggest 返回的高亮候选列表格式化为
// "Did you mean:\n  - Arch[i]tecture.md\n  - ..." 文本段；空切片返回 ""。
func FormatForToolMessage(highlighted []string) string
```

整个子包预计 ≤ 80 行代码 + 对应单测。**所有距离/打分/排序逻辑委托给 sahilm/fuzzy**，本子包不出现任何算法实现。

### 10.9 实施步骤

| 步骤 | 内容 |
|------|------|
| S1 | `go get github.com/sahilm/fuzzy@latest`，确认无传递依赖污染 go.mod |
| S2 | 新增 `internal/tools/suggest/` 子包：`suggest.go`（Suggest + highlightMatch + FormatForToolMessage）+ `suggest_test.go`；总代码 ≤ 80 行 |
| S3 | `local_read.go` globSearch 空结果路径：用 Walk 过的 paths 列表调 suggest.Suggest |
| S4 | `local_read.go` readFile 路径不存在路径：调 os.ReadDir(Dir(path)) 后 suggest.Suggest |
| S5 | `local_read.go` listDir 路径不存在路径：父目录 + 上两级目录合并后 suggest.Suggest |
| S6 | `local_read.go` grepSearch 空结果路径（**文件名层**）：本次扫描的文件路径列表调 suggest.Suggest 找与 pattern 相似的文件名 |
| S7 | **工具调度层**（`internal/agent` 工具分派代码）：捕获"tool not found"错误时，调 suggest.Suggest 在当前 kind 已注册工具名集合中找候选；升级错误消息为 "tool 'X' not found. Did you mean: Y? Z?" |
| S8 | 单测：各工具空结果 / 路径不存在 / 工具名 typo 场景，断言输出含"Did you mean"+ 正确候选 + 高亮标记 |
| S9 | 性能基准：10k 文件 glob_search 失败路径耗时 benchmark，确认 < 20ms |
| S10 | KNOWN_ISSUES.md 登记本节作为"工具调用错误恢复"后续项，观察实战效果 |

### 10.10 不在本节范围

#### 10.10.1 §10 内部技术取舍（不做的事）

- **自写模糊匹配算法**：sahilm/fuzzy 已覆盖 fzf 风格子序列匹配；不叠加 agnivade/levenshtein 做编辑距离补充（一个问题只挑一个库，避免同类能力重复依赖）
- **grep_search token 层候选**：候选空间（token 集合）数量级差两三个数量级，需要独立的"token 提取 + 频率去噪 + 索引"方案。MVP 期待"实战证实 grep_search 空结果误用严重"后再单独启动；**文件名层候选已纳入 MVP**（见 §10.2 grep_search 双层处理）
- **语义相似度（向量/嵌入）**：语义"你是不是想找 authentication？" vs `login`——需要 embedding 模型。当前 MVP 保持纯字符串算法，不引入 embedding 依赖
- **跨工具自动切换建议**（"你用了 grep_search 但或许 glob_search 更合适"）：需要对任务意图做推断，过早且容易误导
- **多语言分词**：sahilm/fuzzy 按 rune 匹配，对中英混合路径够用；不引入专门的中日韩分词器

#### 10.10.2 不归 §10 但归别处的错误恢复（明确归属）

以下错误类型不属于 §10，但**有明确的归属机制**——读者寻找对应错误的解决方案应当
看下表指向的章节，而不是在 §10 内部找：

| 错误类型 | 归属 | 理由 |
|---|---|---|
| `edit_file` `old_str` 找不到 / 多匹配 | **§7 Hashline 行哈希增强** | 行哈希失败已附带当前哈希 + ±2 行上下文，本身就是"输入对的但文件已变"的恢复机制；若 hashline 仍无法解决某些边角，留空待后续，不在 §10 重复造轮 |
| 必填参数缺失 / 类型错 | **Schema 校验层** | 工具调用前拦截，不进入工具内部，§10 看不到也不该处理 |
| 行号越界 / 参数值越界 | **Tool Hook 系统** | 参数验证职责，调用前拦截给出明确错误（"文件实际 50 行，你给了 999"）——值是确定的而不是候选集合，无 fuzzy 可言 |
| 工具未授权（kind 不持有该工具）| **registry-level filter**（不可能场景）| [agent.NewToolRegistryWithAllowlist](../../internal/agent/tool_registry.go#L37) 在注册阶段静默跳过 allowlist 之外的工具——unauthorized 工具压根不进 ToolRegistry，LLM 视野不可见，调用不可达；不需要任何运行时机制处理 |
| 文件被占用（Roster 锁）/ `expected_hash` 冲突 | 重试策略 | 输入是对的，时机/状态问题；无替代输入可建议 |
| `run_shell` 命令不存在 / 黑名单拦截 | 直接错误消息 | 安全风险——不应在 sandbox 外建议命令名（命令名 fuzzy 可能引导 LLM 试探敏感命令）|
| `web_fetch` 网络超时 / `web_search` 配额耗尽 | §9 启动期探测前置 / 重试 / 管理员介入 | 输入对的或外部依赖问题，无 Did-You-Mean 可言 |

**这部分明确写下来的目的**：避免读者将来读 §10 时困惑"那 X 错误怎么办"——每类错误
都有对应章节或机制承接，§10 只负责其能解决的子集。

### 10.11 验证计划

| 优先级 | 场景 | 测试方法 |
|---|---|---|
| V1 | glob_search 拼写错一字母（`Archtechture` ↔ `Architecture`）→ 候选正确 + 输出含 `[...]` 方括号高亮所有命中段 | 单测：临时目录放正确文件，pattern 用错拼写；断言候选字符串 + MatchedIndexes 对应区间均被方括号包住 |
| V2 | glob_search 只记得一半（`multi_agent` ↔ `multiAgent_upgrade_plan.md`）→ 候选正确（验证 fzf 子序列匹配场景） | 单测 |
| V3 | read_file 路径层级错（`docs/Architecture.md` 实际 `docs/architecture/Architecture.md`）→ 候选为父目录下同名 | 单测 |
| V4 | list_dir 层级错 → 候选含上级目录 | 单测 |
| V5 | 完全无相似候选 → 输出不含 "Did you mean"，避免噪声 | 单测 |
| V6 | 性能基准 → 10k 文件 < 20ms | benchmark |
| V7 | 实战：重跑 2026-04-23 类似调查任务，观察 Explorer 是否在第一次空结果后直接用候选纠错 | 手工烟测 |
| V8 | **已知弱点验证**：纯字母乱序拼写错（`Archtechture` 其实是 transpose `te` ↔ `ech`）fuzzy 打分可能偏低，确认仍排进 Top-3；若实战失败则记录为"v2 可能叠加 Levenshtein 兜底"的触发信号 | 单测 + 实战观察 |
| V9 | grep_search 文件名层候选：pattern 找不到 token，但当前扫描列表有相似文件名（如 grep `auth` 没命中但有 `authentication.go`）→ 候选输出含相似文件名 | 单测 |
| V10 | 工具调度层 typo：LLM 调 `read_files` / `readFile` / `read-file`（实际工具名 `read_file`）→ 错误消息含 "Did you mean: read_file" | 单测 |
| V11 | 验证 unauthorized 工具不可见：当前 kind allowlist = {read_file, write_file}，构造 ToolRegistry 后断言 `Defs()` 返回值不含 `run_shell` / `web_fetch` 等其他工具，`RegisteredCount()` 等于 allowlist 长度——证明 unauthorized 路径在注册阶段消除，不会触发 §10 也不会触发 hook | 单测：覆盖 [agent.NewToolRegistryWithAllowlist](../../internal/agent/tool_registry.go#L37) 的过滤行为，与 §10 的 Did-You-Mean 路径完全分离 |

---

## 状态总览

| 章节 | 内容 | 优先级 | 前置依赖 | 状态 |
|------|------|--------|----------|------|
| §1 | Per-Worker 工具集配置 | P2 | v3 §9.1 ✅ | ✅ 已完成（2026-04-19），但**已被 §11 取代**（2026-04-25）|
| §3 | 能力声明阶段二 | P4 | v3 §9.4 阶段一 ✅ + 附录 A | 📝 待附录 A 完成 |
| §7 | Hashline 行哈希增强 | **P1** | 无 | ✅ **已完成**（2026-04-26）|
| §8 | 跨子系统装配护栏 | P2 | 无 | 📦 已拆分（2026-04-25 → CLAUDE.md "Shipping conventions" + §11.8 S11）|
| §9 | Bootstrap LLM 连通性 + 模型名探测 | P2 | §11 kind 体系 | ✅ **已完成**（2026-04-26）：§9.5 启动期可观测性（banner + TCP probe + failure_action）+ §9.3 错误码分类（recoverable/unrecoverable）+ §9.4 诊断映射（model_not_found / invalid_api_key / context_length_exceeded 等）+ §9.6 重试预算上限（MaxRetries 兜底）全部落地 |
| §10 | 工具调用错误恢复（Did-You-Mean） | P2 | 无 | 🚧 MVP 已落地（2026-04-26：`internal/tools/suggest/` + `glob_search` / `read_file` 两处接入），扩展设计待重构 |
| §11 | 统一 Agent 声明式配置（v4 配置格式重写）| **P1** | 无 | ✅ **已完成**（2026-04-26 Phase A/B/C/D/E 全部落地） |
| 附录 A | 分级权限模型 | P3 | v4 §11 | 📝 触发条件未满足（2026-04-25 由 §2 迁入附录）|
| 附录 B | 管理员信赖标记 | P4 | 待引入外部代理 / 插件 / MCP | 📝 触发条件未满足（2026-04-25 由 §4 迁入附录）|
| 附录 C | 冲突避免长期方案 | P3 | v3 §8.3 ✅ + v4 §11 | 📝 触发条件未满足（迄今零写冲突；2026-04-25 由 §5 迁入附录）|
| 附录 D | Agent 休眠/唤醒优化 | P4 | v4 §11 落地后实测 agent 总数 ≥ 20 | 📝 触发条件未满足（项目长期预期 < 10；2026-04-25 由 §6 迁入附录）|

---

## 11. 统一 Agent 声明式配置（v4 配置格式重写）

> 状态：✅ **已完成**（2026-04-26 Phase A/B/C/D/E 全部落地；设计定稿日期 2026-04-25）
> 实施进度详见**本文件顶部"实施进度"块**——Phase A 增量 / Phase B 基础设施 /
> Phase C bootstrap 切换 / Phase D v3 兼容层下线 / Phase E 收尾清理全部完成。
> 优先级：P1
> 替代关系：取代 §1（per-worker 异质化方案）—— 改在 kind 层做差异化
> 触发来源：内置 Worker/Explorer Go 类型与硬编码常量阻碍新增 agent 种类；行为参数全局共享无法 per-kind 调优
> 约束：**不向后兼容**——v4 配置格式为全新 schema，不保留旧字段 fallback

### 11.1 背景

当前 `setting.yaml` 与 Go 代码两层都把"Agent 类型"当作硬编码概念：

- **Go 类型层**：`internal/worker` 与 `internal/explorer` 是两个独立的 package，
  `Worker` / `Explorer` 各自带 `New` / `NewWithID` 构造函数、`systemPrompt` 常量、
  `workerMaxRetries=3` / `explorerMaxRetries=3` 等"角色属性"硬编码常量。两个文件
  90% 结构一致，差异仅在：组合的 ToolGroup 集合、system prompt 字符串、event_type
  字符串、是否持有 Roster/ApprovalCh、`MetaGroup.DisablePublishTask` 标志位。
- **YAML 配置层**：`agent_max_loops`、`compact_token_threshold` 等行为参数全局
  共享；`worker_count` + `worker_profile` 仅描述 Worker 数量与统一工具集；
  Explorer 通过单独的 `explorer_profile` 字段配置；新增 agent 种类必须改 Go 代码。

这导致以下结构性问题：

1. **新增 agent 种类必须写 Go**：增加"文档撰写代理 / 测试执行代理 / SQL 审查代理"
   等场景，需要新建 package、复制 90% 雷同的构造函数代码，再在 bootstrap 里接线。
   配置层完全没有表达力。
2. **行为参数全局共享**：Scheduler 拆解任务可能需要 15 步 ReAct，Explorer 出报告
   通常 5 步够用，但当前共用同一 `agent_max_loops`。Token 压缩阈值同理。
3. **per-kind 模型选择缺失**：低成本的纯调查任务应当走低价模型，重写代码应当走
   高端模型，但当前 LLM 模型是全局唯一。
4. **运行时与配置时信任域混淆**：早期"工具受 ProjectRoot 约束"是合理的安全基线，
   但配置层路径（如未来的 `system_prompt_file`）不应当受同样约束——它在用户启动
   权限下解析，不在 agent 沙箱里执行。
5. **上下文管理缺失**：没有 `context_limit` 概念，Agent 历史长度无上限，长任务会
   溢出模型上下文窗口导致 400/413 错误（详见 §11.7）。

**v4 解锁的具体使用场景**（在 v3 全局 profile 模型下无法纯靠配置实现，在 v4 的
kind 体系下都成为 YAML 编辑动作）：

- **混合能力团队**：`worker_standard` 与 `worker_readonly` 两个 kind 同时存在，
  scheduler 根据任务性质路由——编码类任务投递给 `worker_standard`，纯调查任务
  投递给 `worker_readonly`
- **安全隔离**：持有 `run_shell` 的 kind 与不持有的 kind 分离，敏感操作（如操作
  数据库）只投递给前者，其他 kind 在 schema 层面就不可能执行
- **渐进式扩展**：先加一个新 kind（replicas=1）试水，确认稳定后再调高 replicas
  推广，或新增同类 kind 以不同模型组合 A/B 验证

### 11.2 设计原则

1. **Agent kind 是 YAML 一级公民**：每个 kind 在 YAML 里是一个自包含的声明块，
   含 `tools` / `system_prompt_file` / `model` / `event_type` / `replicas` 以及
   per-kind 行为参数。
2. **通用 runtime + 配置驱动差异**：Go 侧仅保留一个泛型 agent runner，没有
   `Worker` / `Explorer` 类型。所有差异下沉到 YAML。
3. **内置 Go 类型完全废除**：`internal/worker` 与 `internal/explorer` 两个 package
   整体删除。出厂兼容性靠默认 `setting.yaml` 中预置 `worker` + `explorer` 两个
   kind 块 + 收窄后的 `scheduler` 块（仅含 `model`）达成，**不靠代码内置 fallback**。
4. **强制外部配置文件**：启动时若 `setting.yaml`（或 `-config` 指定的文件）不
   存在 / 无法读取，立即终止启动并报错。**不嵌入二进制默认值**。
5. **kind 块自洽，无模板继承**：每个 kind 块完整声明自己的行为参数。需要 DRY 时
   使用 YAML 锚点（`&` / `*`）这一原生特性，不引入自定义模板机制。
6. **配置层 vs 运行时层的信任域分离**：
   - **配置层路径**（`system_prompt_file` 等）允许绝对路径，仅校验"文件存在 +
     可读"，不走 `pathutil.ValidatePath`
   - **运行时工具路径**继续受 `pathutil.ValidatePath` 约束在 `ProjectRoot` 内
     （早期阶段，待 Tool Hook + 能力声明阶段二落地后放开，详见 CLAUDE.md
     "Runtime file access scope"）
7. **路径风格统一为正斜杠**：YAML 中所有路径字段仅允许 forward slash，启动期
   严格校验；不接受反斜杠双向兼容。代码读取时 `filepath.FromSlash()` 在边界
   做一次转换。
8. **`mail_chain_max_depth` 不进 YAML**：作为系统级安全常量保留在
   `internal/mailbox` 包内，防止邮件级联爆炸——这是稳定性红线，不允许用户调。
9. **上下文上限内建**：`context_limit` 不是可选装饰，而是核心运行时参数，配套
   token 估算与截断策略（详见 §11.7）。
10. **不向后兼容**：v4 配置格式为全新文件，旧字段全部移除。升级时人工迁移一次
    即可，避免代码中充斥 fallback 逻辑。

### 11.3 v4 YAML 配置格式（完整示例）

```yaml
# ============================================================
# v4 setting.yaml — 统一 Agent 声明式配置
# ============================================================
# 路径约定：所有路径字段仅允许 forward slash（/）。启动期会拒绝反斜杠。
# system_prompt_file 允许绝对路径（用户启动权限域）；
# 运行时工具路径仍受 ProjectRoot 约束（agent 沙箱域，详见 CLAUDE.md）。

# --- LLM 默认基础设施（per-kind 可覆盖 model）---
llm:
  base_url: https://dashscope.aliyuncs.com/compatible-mode/v1
  api_key: ${DASHSCOPE_API_KEY}
  default_model: qwen3.6-plus
  timeout_sec: 120

# --- 工具集白名单（命名复用层）---
# 每个 kind 块可以引用 profile 名（profile 字段），也可以直接列工具（tools 字段），
# 二选一。同时给两者会启动失败。
# 以下 5 个 profile 是预置起手菜单，用户可直接引用、修改或新增。
tool_profiles:
  # 全能执行：代码修改 + Shell + 网络 + 协作
  worker_standard:
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

  # 只读 worker：纯调查，不能修改代码、不能执行 Shell
  worker_readonly:
    - read_file
    - list_dir
    - grep_search
    - glob_search
    - send_message

  # 纯代码 worker：可读写代码，但无网络无 Shell（适合敏感环境）
  worker_code_only:
    - read_file
    - list_dir
    - grep_search
    - glob_search
    - write_file
    - edit_file
    - publish_task
    - send_message

  # 本地只读调查：仅代码库，不联网
  explorer_codebase:
    - read_file
    - list_dir
    - grep_search
    - glob_search
    - send_message

  # 完整调查：本地 + 网络只读
  explorer_full:
    - read_file
    - list_dir
    - grep_search
    - glob_search
    - web_search
    - web_fetch
    - send_message

# --- Scheduler 配置（独立块，配置面刻意收窄，详见 §11.5.5）---
# Scheduler 不在 agents 列表中，因为它是事件驱动的全局唯一编排者，
# 结构上与轮询型 agent 不同（不通过 QueryAvailable 认领任务）。
# 其工具集 / 系统提示词 / 行为参数全部内置在 internal/scheduler 包，
# 外部仅允许覆盖 model 字段。
scheduler:
  model: qwen3.6-plus    # 可选，缺省回落 llm.default_model
  # 未来可能扩展：base_url / api_key 覆盖（若 scheduler 走独立 endpoint）

# --- Agent kind 列表（核心）---
# 每个块自包含；replicas 控制该 kind 起几个实例。
# 同 kind 的多个实例完全同质（同一工具集 / 同一提示词 / 同一模型）。
agents:
  - kind: worker
    replicas: 3
    event_type: ""                              # 默认任务队列
    profile: worker_standard
    model: qwen3.6-plus
    system_prompt_file: prompts/worker.md
    agent_max_loops: 10
    task_max_retries: 3
    enforce_compact_token_threshold: 4000
    context_limit: 16000

  - kind: explorer
    replicas: 1
    event_type: explore
    profile: explorer_full                      # 默认调查代理保留 web 能力（与现有 internal/explorer 行为一致）
    model: qwen3.6-flash                        # 调查类用低成本模型
    system_prompt_file: prompts/explorer.md
    agent_max_loops: 5
    task_max_retries: 3
    enforce_compact_token_threshold: 3000
    context_limit: 8000

# --- 运行时基础设施（非 Agent 配置）---
project_root: "."
infra:
  watchdog:
    interval_sec: 30
  mail_notifier:
    enabled: true
    interval_sec: 60
  store:
    event_channel_buffer: 256
    fifo_limit: 100
    default_concurrency: 3
  roster:
    wait_timeout_sec: 300

# --- 启动期可观测性（详见 §9.5）---
startup_probe: "tcp"                 # "tcp" 做 host:port DialTimeout；"off" 完全跳过
startup_probe_timeout_sec: 5         # 单次探测超时，默认 5 秒
startup_probe_failure_action: "warn" # "warn"=失败仅 warning + 启动继续；"exit"=失败硬退出

# --- 杂项 ---
max_subtask_depth: 3
shell_timeout_sec: 60
shell_blacklist: []
shell_greylist: []
search_api_provider: serper
search_api_url: https://google.serper.dev/search
search_api_key: ${SERPER_API_KEY}
```

**环境变量替换**：上文示例中的 `${DASHSCOPE_API_KEY}` / `${SERPER_API_KEY}` 是 shell-style
env var 引用，需要 `config.LoadConfig()` 在反序列化**前**对原始 YAML 文本做一次
[`os.ExpandEnv`](https://pkg.go.dev/os#ExpandEnv) 替换才能生效——这是 Twelve-factor app 的
标准做法，避免把 API key 提交到版本库。**未引用 env var 的字段不受影响**（例如
`base_url` / `default_model` / 路径字段等仍按字面值处理）。该实现路径在 §11.8 S1 落地。

**DRY 提示**：如需多个 kind 共享部分配置（如同一组行为参数），使用 YAML 锚点而
非自定义模板机制：

```yaml
agents:
  - kind: worker-fast
    <<: &worker_base
      replicas: 1
      event_type: ""
      profile: worker_standard
      system_prompt_file: prompts/worker.md
      agent_max_loops: 10
      task_max_retries: 3
      enforce_compact_token_threshold: 4000
      context_limit: 16000
    model: qwen3.6-flash

  - kind: worker-deep
    <<: *worker_base
    model: qwen3.6-plus
    agent_max_loops: 20
```

### 11.4 Go 配置结构体

```go
package config

// ============================================================
// v4 Config — 全新结构，不兼容 v3
// ============================================================

type Config struct {
    // --- LLM 基础设施（全局默认值，per-kind 可覆盖 model）---
    LLM LLMConfig `yaml:"llm" json:"llm"`

    // --- 工具集白名单（命名复用层）---
    ToolProfiles map[string][]string `yaml:"tool_profiles" json:"tool_profiles"`

    // --- Scheduler 独立块（事件驱动单例，结构与轮询型 agent 不同）---
    Scheduler SchedulerKind `yaml:"scheduler" json:"scheduler"`

    // --- Agent kind 列表（核心）---
    Agents []AgentKind `yaml:"agents" json:"agents"`

    // --- 运行时基础设施 ---
    Infra InfraConfig `yaml:"infra" json:"infra"`

    // --- 启动期可观测性（详见 §9.5）---
    StartupProbe              string `yaml:"startup_probe" json:"startup_probe"`                                 // "tcp" / "off"，默认 "tcp"
    StartupProbeTimeoutSec    int    `yaml:"startup_probe_timeout_sec" json:"startup_probe_timeout_sec"`         // 单次探测超时，默认 5
    StartupProbeFailureAction string `yaml:"startup_probe_failure_action" json:"startup_probe_failure_action"`   // "warn" / "exit"，默认 "warn"

    // --- 杂项 ---
    ProjectRoot       string   `yaml:"project_root" json:"project_root"`
    MaxSubtaskDepth   int      `yaml:"max_subtask_depth" json:"max_subtask_depth"`
    ShellTimeoutSec   int      `yaml:"shell_timeout_sec" json:"shell_timeout_sec"`
    ShellBlacklist    []string `yaml:"shell_blacklist" json:"shell_blacklist"`
    ShellGreylist     []string `yaml:"shell_greylist" json:"shell_greylist"`
    SearchAPIProvider string   `yaml:"search_api_provider" json:"search_api_provider"`
    SearchAPIURL      string   `yaml:"search_api_url" json:"search_api_url"`
    SearchAPIKey      string   `yaml:"search_api_key" json:"search_api_key"`
}

// LLMConfig 全局 LLM 默认值
type LLMConfig struct {
    BaseURL      string `yaml:"base_url" json:"base_url"`
    APIKey       string `yaml:"api_key" json:"api_key"`
    DefaultModel string `yaml:"default_model" json:"default_model"`
    TimeoutSec   int    `yaml:"timeout_sec" json:"timeout_sec"`
}

// AgentKind 一个 agent 种类的声明，自包含。
// 同 kind 的多个实例（replicas 个）完全同质——同工具集、同提示词、同模型。
// 异质化通过声明多个 kind 实现。
type AgentKind struct {
    Kind                         string   `yaml:"kind" json:"kind"`                       // 该种类标识，agents 列表内必须唯一
    Replicas                     int      `yaml:"replicas" json:"replicas"`               // 实例数，>=1
    EventType                    string   `yaml:"event_type,omitempty" json:"event_type,omitempty"` // 任务队列分类字符串，缺省 ""

    // 工具集二选一（同时给两者 → 启动失败）
    Profile                      string   `yaml:"profile,omitempty" json:"profile,omitempty"`
    Tools                        []string `yaml:"tools,omitempty" json:"tools,omitempty"`

    Model                        string   `yaml:"model,omitempty" json:"model,omitempty"` // 缺省回落 LLM.DefaultModel
    SystemPromptFile             string   `yaml:"system_prompt_file" json:"system_prompt_file"` // 路径，启动时读入

    // 行为参数（per-kind 必填，无全局默认）
    AgentMaxLoops                int      `yaml:"agent_max_loops" json:"agent_max_loops"`               // 单次任务内 ReAct 步数上限
    TaskMaxRetries               int      `yaml:"task_max_retries" json:"task_max_retries"`             // 任务级回滚重试次数上限
    EnforceCompactTokenThreshold int      `yaml:"enforce_compact_token_threshold" json:"enforce_compact_token_threshold"` // 强制压缩触发点
    ContextLimit                 int      `yaml:"context_limit" json:"context_limit"`                   // 历史硬上限（详见 §11.7）
}

// SchedulerKind scheduler 独立块（配置面刻意收窄，详见 §11.5.5）。
// 工具集 / 系统提示词 / 行为参数 / replicas 全部内置在 internal/scheduler 包，
// YAML 仅允许覆盖 model 字段。
type SchedulerKind struct {
    Model string `yaml:"model,omitempty" json:"model,omitempty"` // 缺省回落 LLM.DefaultModel
    // 未来如需扩展 base_url / api_key 覆盖在此添加
}

// InfraConfig 非 Agent 运行时基础设施
// 子类型独立命名（不用匿名嵌套 struct）——便于单测、扩展与 IDE 跳转。
type InfraConfig struct {
    Watchdog     WatchdogConfig     `yaml:"watchdog" json:"watchdog"`
    MailNotifier MailNotifierConfig `yaml:"mail_notifier" json:"mail_notifier"`
    Store        StoreConfig        `yaml:"store" json:"store"`
    Roster       RosterConfig       `yaml:"roster" json:"roster"`
}

type WatchdogConfig struct {
    IntervalSec int `yaml:"interval_sec" json:"interval_sec"`
}

type MailNotifierConfig struct {
    Enabled     bool `yaml:"enabled" json:"enabled"`
    IntervalSec int  `yaml:"interval_sec" json:"interval_sec"`
}

type StoreConfig struct {
    EventChannelBuffer int `yaml:"event_channel_buffer" json:"event_channel_buffer"`
    FIFOLimit          int `yaml:"fifo_limit" json:"fifo_limit"`
    DefaultConcurrency int `yaml:"default_concurrency" json:"default_concurrency"`
}

type RosterConfig struct {
    WaitTimeoutSec int `yaml:"wait_timeout_sec" json:"wait_timeout_sec"`
}

// AgentRuntimeConfig 内部使用，由 Bootstrap 从 AgentKind + LLMConfig 合成后注入
// 到 agent runner。不出现在 YAML 中。
//
// **LLM 客户端不在此结构中**——Bootstrap 单独构造 llm.Client（基于 LLMConfig 与
// kind.Model 合并值），通过 deps 注入 runner。本结构的 Model 字段仅作为运行时元
// 数据使用——主要用途是 HistoryEntry.Model 记录（详见 §11.7.3 模型切换基准重置）
// 与运行时日志，**非 LLM 客户端构造源**。
type AgentRuntimeConfig struct {
    InstanceID                   string   // 形如 "worker-1"，由 kind + replica 序号生成
    Kind                         string
    EventType                    string
    AllowedTools                 []string // 从 Profile 解析或直接来自 Tools
    Model                        string   // 已合并后的最终值；运行时元数据用途，非 client 构造源
    SystemPrompt                 string   // 已读入内存的提示词内容
    AgentMaxLoops                int
    TaskMaxRetries               int
    EnforceCompactTokenThreshold int
    ContextLimit                 int
}

// Validate 在 Bootstrap 主流程中调用，对应 §11.5.3 全部启动校验规则。
// 任一规则失败即返回 non-nil error 终止启动。
func (c *Config) Validate() error
```

### 11.5 字段语义、默认值与启动校验

#### 11.5.1 per-kind 字段对照

| YAML 字段 | 含义 | 是否必填 | 备注 |
|---|---|---|---|
| `kind` | 种类标识 | 是 | `agents` 列表内必须唯一 |
| `replicas` | 实例数 | 是 | ≥1，否则启动失败 |
| `event_type` | 该种类的任务队列字段值 | 否 | 缺省 `""`（默认队列）|
| `profile` | 引用 `tool_profiles` 命名集 | `profile`/`tools` 二选一 | 同时给两者 → 启动失败 |
| `tools` | 直接内联工具名列表 | `profile`/`tools` 二选一 | 多个 kind 复用时优先用 `profile` |
| `model` | per-kind 模型覆盖 | 否 | 缺省回落 `llm.default_model` |
| `system_prompt_file` | 提示词文件路径 | 是 | 启动时读入；缺失/不可读则启动失败 |
| `agent_max_loops` | 单次任务内 ReAct 步数上限 | 是 | 耗尽后 `RetryRollback` |
| `task_max_retries` | 任务级回滚重试次数上限 | 是 | 耗尽后 terminal failure + 崩溃汇报邮件 |
| `enforce_compact_token_threshold` | 强制压缩触发点 | 是 | Token 超过即触发 LLM summary |
| `context_limit` | 历史硬上限 | 是 | 详见 §11.7（截断策略）|

**没有全局默认值**：行为参数（`agent_max_loops` / `task_max_retries` /
`enforce_compact_token_threshold` / `context_limit`）必须在每个 kind 块内显式声明。
理由：这些参数直接影响成本与运行时行为，silent default 容易造成"我没意识到那是
上限"的事故；既然每个 kind 都该独立调优，就不该有可被遗忘的全局兜底值。

#### 11.5.2 路径约定

- **风格**：所有路径字段仅允许 forward slash。启动期扫描所有路径字段，发现反
  斜杠（`\`）即报错并终止启动。**不接受**反斜杠双向兼容——避免出现"Linux 上配
  好的 yaml 给 Windows 用户改了反斜杠，再 commit 回 Linux 又跑不了"的来回踩坑。
- **配置层路径**（`system_prompt_file`）：允许绝对路径；启动期 `os.ReadFile`
  读入，失败即终止启动。仅做"文件存在 + 可读"校验，**不走** `pathutil.ValidatePath`。
- **运行时工具路径**：继续受 `pathutil.ValidatePath` 约束在 `ProjectRoot` 内。
  详见 CLAUDE.md "Runtime file access scope"——这是早期阶段的临时纪律，待 Tool
  Hook + 能力声明阶段二落地后放开。
- **代码侧**：`config.LoadConfig()` 在反序列化后立即对所有路径字段调用
  `filepath.FromSlash()`，让内部统一以 OS 分隔符处理，无需各调用点自己 normalize。

#### 11.5.3 启动校验规则（任一失败即终止启动）

1. `setting.yaml`（或 `-config` 指定文件）必须存在且可读——**不嵌入二进制默认值**
2. `agents` 列表非空
3. 每个 `AgentKind.Kind` 在列表内唯一
4. `agents[*].replicas >= 1`
5. `profile` 与 `tools` 互斥（恰一）
6. `profile` 引用的名称必须存在于 `tool_profiles`
7. 所有工具名必须在 `tools.AllToolNames` 注册表内
8. **AgentKind 列表的**每个 `system_prompt_file` 存在且可读，读入内存（SchedulerKind 不适用——scheduler 提示词为内置 Go const，详见 §11.5.5）
9. 所有路径字段不含反斜杠
10. `scheduler` 块约束收窄（详见 §11.5.5）：若 `scheduler.model` 出现则必须为非空字符串；整块缺失等价于 `scheduler.model = llm.default_model`，**不报错**；出现 `model` 之外的字段触发未知字段警告
11. **AgentKind 列表中**每个 kind 的行为参数（`agent_max_loops` / `task_max_retries` /
    `enforce_compact_token_threshold` / `context_limit`）显式声明且 > 0（SchedulerKind 不适用——这些参数为内置常量，详见 §11.5.5）
12. 每个 `AgentKind.Kind` 必须为非空字符串——补 第 3 条（唯一性）的死角：唯一性允许"只有一个 kind 且 kind=''"通过，本条堵死此情况

#### 11.5.4 系统级常量（不进 YAML）

以下参数维持代码内常量，不暴露 YAML：

| 常量 | 位置 | 理由 |
|---|---|---|
| `MailChainMaxDepth` | `internal/mailbox` | 邮件级联爆炸是系统稳定性红线，不允许用户调 |
| 工具名 → 运行时依赖的静态映射表 | `internal/bootstrap/dependency_map.go`（新增）| 用户只声明工具名，依赖图属内部实现 |
| `keepRecentForTruncate`（含 layer-1 压缩保留 + §11.7.4 截断保护）| `internal/agent` | 模型 context 完整性的物理需求（最近一对 request/response 不能丢），不属于 per-kind 调优维度。**v3 的 `compact_keep_recent` YAML 字段在 v4 移除**；未来需灵活性时仅允许环境变量覆盖 |

#### 11.5.5 Scheduler 配置面收窄（与 agent kind 的不对称性）

Scheduler 在 §11 体系内被特殊对待：虽然结构上仍是一个 kind，但其外部可配置表面
**远窄于** worker / explorer——只允许覆盖 `model`，其余字段（工具集 / 系统提示词 /
4 个行为参数 / replicas / event_type）全部硬编码为 Go 常量。

**理由**：Scheduler 是系统编排核心——事件驱动、全局唯一、决定整个任务图的拆解
策略。其工具集 / 系统提示词 / 行为参数都是**编排逻辑的内禀部分**，用户改了不是
调优而是破坏。允许外部覆盖的字段严格限定为"API 相关"——切换模型 / 切换 endpoint
/ 切换 key 是合理运维操作，其他都属于改源码的范畴。

**配置面对照表**：

| 字段 | worker / explorer | scheduler |
|---|---|---|
| `model` | 可配 | **可配**（唯一允许的外部覆盖）|
| `tools` / `profile` | 可配 | 内置常量（`internal/scheduler/tools.go`）|
| `system_prompt_file` | 可配（必填）| 内置 Go const（`internal/scheduler/prompt.go`）|
| `agent_max_loops` | 可配（必填）| 内置常量 |
| `task_max_retries` | 可配（必填）| 内置常量 |
| `enforce_compact_token_threshold` | 可配（必填）| 内置常量 |
| `context_limit` | 可配（必填）| 内置常量 |
| `replicas` | 可配（≥1）| 恒为 1（事件驱动单例）|
| `event_type` | 可配 | 不适用（scheduler 不通过 `QueryAvailable` 认领）|

**Scheduler YAML 块的最小化形态**（取代 §11.3 中的完整形态）：

```yaml
# Scheduler 仅允许 model 字段外部覆盖；其他字段全部内置在 internal/scheduler。
# 块整体可省略，等价于 scheduler.model = llm.default_model。
scheduler:
  model: qwen3.6-plus    # 可选，缺省回落 llm.default_model
  # 未来可能扩展：base_url / api_key 覆盖（若 scheduler 走独立 endpoint）
  # 出现任何其他字段触发启动期未知字段警告
```

**对应 Go 结构体**（取代 §11.4 中的 `SchedulerKind` 完整版）：

```go
type SchedulerKind struct {
    Model string `yaml:"model,omitempty" json:"model,omitempty"` // 缺省回落 LLM.DefaultModel
    // 未来如需扩展 base_url / api_key 覆盖在此添加
}
```

**对应启动校验**（取代 §11.5.3 第 10 条）：`scheduler` 块若提供 `model` 字段，必
须为非空字符串；若整块缺失或为空，等价于 `scheduler.model = llm.default_model`，
**不报错**。其他原本"必填"的字段（system_prompt_file / 行为参数）从校验列表中
移除——它们不再属于 yaml 表面。

**§11.3 / §11.4 / §11.5.3 上游章节已按本节做最小机械对齐**——本节是 scheduler
配置面的唯一权威。

**未来若放开某字段**（以 `context_limit` 为例）：
1. `SchedulerKind` 增字段 `ContextLimit int`
2. `internal/scheduler` 内对应常量改为"优先用配置值、缺失时回落原常量"
3. §11.5.3 增一条针对该字段的校验（必填或允许缺省，按需）
4. 本节配置面对照表把该字段从"内置常量"改为"可配"

**默认收紧的方法论**：一个字段从内置升级为可配是低成本的兼容性扩展，反向收回
（"以前能配现在不让配了"）是破坏性变更。所以收紧门槛低、放开门槛高——只在反复
出现明确运维调优需求时才放开，避免给系统稳定性留缝。

### 11.6 启动流程与依赖注入

#### 11.6.1 Bootstrap 主流程（伪代码）

```go
func Bootstrap(configPath string) (*System, error) {
    // 1. 强制读取外部配置（缺失即终止）
    cfg, err := config.LoadConfig(configPath)
    if err != nil {
        return nil, fmt.Errorf("配置加载失败: %w", err)
    }

    // 2. 启动校验（见 11.5.3）—— 包括读入所有 system_prompt_file 内容
    if err := cfg.Validate(); err != nil {
        return nil, fmt.Errorf("配置校验失败: %w", err)
    }

    // 2.5. 启动期 banner + TCP probe（见 §9.5）
    //   - banner：打印逐 kind 的 (base_url, model, profile) 摘要 + 脱敏 api_key
    //   - probe：对 cfg.LLM.BaseURL 的 host:port 做 net.DialTimeout（best-effort）
    //   - 失败行为：默认 warning + 继续启动；startup_probe_failure_action="exit"
    //     可改为硬退出；startup_probe="off" 可整体关闭
    printStartupBanner(cfg)
    if err := startupProbe(cfg); err != nil {
        if cfg.StartupProbeFailureAction == "exit" {
            return nil, fmt.Errorf("启动期 probe 失败: %w", err)
        }
        log.Printf("[WARN] startup probe: %v (best-effort, 启动继续)", err)
    }

    // 3. 实例化共享基础设施
    storeView := store.NewMemoryTaskStore(...)
    rosterView := roster.NewMemoryRoster()
    mbReg := mailbox.NewRegistry(...)
    cancelReg := store.NewTaskCancelRegistry()
    searchProvider := webtool.NewProvider(cfg.SearchAPIProvider, ...)
    shellFilter, _ := shell.BuildFilter(cfg.ProjectRoot,
        cfg.ShellBlacklist, cfg.ShellGreylist)

    // 4. 启动 Scheduler（独立路径，结构特殊）
    //   LLM client 由 buildKindLLMClient 基于 cfg.LLM + scheduler.Model 合并构造
    //   （Model 缺省回落 cfg.LLM.DefaultModel，详见 §11.5.5）
    schedRT := buildSchedulerRuntime(cfg.Scheduler, cfg.LLM)
    schedLLM := buildKindLLMClient(cfg.LLM, cfg.Scheduler.Model)
    sched := scheduler.New(storeView, mbReg, cancelReg, schedRT, schedLLM)

    // 5. 启动 Watchdog（不属于任何 kind）
    wd := watchdog.New(storeView, cfg.Infra.Watchdog, ...)

    // 6. 按 kinds 列表批量实例化 agent runner
    //   每个 kind 单独构造 LLM client（基于 cfg.LLM + kind.Model 合并值），
    //   同 kind 的多 replicas 共享一个 client。
    //   LLM client 不进 AgentRuntimeConfig（详见 §11.4 注释），通过 deps 注入。
    var runners []*agent.Runner
    for _, kind := range cfg.Agents {
        kindLLM := buildKindLLMClient(cfg.LLM, kind.Model)  // 内部处理 Model fallback
        for i := 1; i <= kind.Replicas; i++ {
            instanceID := fmt.Sprintf("%s-%d", kind.Kind, i)
            rt := buildAgentRuntime(kind, cfg.LLM, instanceID)
            deps := resolveDependencies(rt.AllowedTools,
                storeView, rosterView, mbReg, searchProvider, shellFilter, ...)
            runner := agent.NewRunner(rt, deps, kindLLM)
            runners = append(runners, runner)
        }
    }

    return &System{Sched: sched, Watchdog: wd, Runners: runners, ...}, nil
}
```

#### 11.6.2 工具 → 依赖项的静态映射

Bootstrap 在实例化 agent runner 时，按 `AllowedTools` 列表查表注入运行时依赖。
**用户在 YAML 里只声明工具名，不需要懂内部依赖图**：

| 工具 | 自动注入的依赖 |
|---|---|
| `read_file` / `list_dir` / `grep_search` / `glob_search` | `Workdir` + `FileStateCache` |
| `write_file` / `edit_file` | + `Roster`（文件级写锁，附 `RosterWaitTimeoutSec`）|
| `run_shell` | + `ApprovalCh` + `shell.CommandFilter` + `ShellTimeoutSec` |
| `publish_task` | + `Store` + `TaskHolder` + `MaxSubtaskDepth` |
| `send_message` | + `mailbox.Registry` + `MailChainMaxDepth`（常量）|
| `web_search` / `web_fetch` | + `webtool.SearchProvider`（按 `SearchAPIProvider` 配置创建一次）|

新增工具时同步更新该映射表（位于 `internal/bootstrap/dependency_map.go`）。

#### 11.6.3 Scheduler 路由：沿用 event_type

Scheduler 通过 `publish_task` 时填写 `event_type` 字段决定任务投放队列。Agent
runner 的 `QueryAvailable(eventType)` 仍按字符串匹配认领。**event_type 与 kind
是多对多关系**：

- 多个 kind 可以共享同一 `event_type`（同质化资源池，例如未来加 `worker-fast` /
  `worker-deep` 两个 kind 都用 `event_type: ""` 抢同一队列）
- 一个 kind 监听单一 `event_type`（当前简化版；多值监听作为未来扩展点）

#### 11.6.4 Board snapshot 改按 kind 聚合

为了让 scheduler LLM 在分解任务时知道"系统里有哪些 kind 各能做什么"，
`MemoryTaskStore.Snapshot()` 输出的 `resources` 段按 kind 聚合（取代旧的"按
event_type 聚合"）：

```json
{
  "resources": {
    "kinds": [
      {"kind": "worker", "replicas": 3, "busy": 1, "event_type": "",
       "tools": ["read_file", "write_file", "run_shell", "..."]},
      {"kind": "explorer", "replicas": 1, "busy": 0, "event_type": "explore",
       "tools": ["read_file", "grep_search", "web_search", "..."]}
    ]
  }
}
```

scheduler prompt 中的路由指引据此说明"调查类任务派给 explorer kind，编码类派给
worker kind"等规则。

#### 11.6.5 共享基础设施列表（不属于任何 kind）

以下组件由 Bootstrap 创建一次并注入需要的 agent runner，与 kind 数量无关：

- `MemoryTaskStore`（公告板）
- `MemoryRoster`（花名册）
- `mailbox.Registry`
- `TaskCancelRegistry`
- `Watchdog`
- `Scheduler`（事件驱动单例，`scheduler` 独立块配置）
- `webtool.SearchProvider`（按 SearchAPIProvider 创建一次，所有 kind 共享）
- `hook.AgentHookRegistry` / `hook.ToolHookRegistry`
- `shell.CommandFilter`（按 ShellBlacklist/Greylist 创建一次）

#### 11.6.6 与旧 Worker/Explorer 包的关系

本 §11 落地后：

- **删除** `internal/worker` 整个 package（含 `Worker` struct、`New`/`NewWithID`、
  `systemPrompt` 常量、`workerMaxRetries` 常量、`currentTaskHolder` 类型）
- **删除** `internal/explorer` 整个 package
- `worker.BuildTeamSnapshot` 函数（被 explorer 借用）迁移到 `internal/agent` 或
  新建 `internal/team` 包
- 两份重复的 `currentTaskHolder` 合并到 `internal/agent` 一份
- `MetaGroup.DisablePublishTask` 标志位删除——Explorer 不能 `publish_task` 改为
  YAML 工具列表里不列该工具来表达
- 默认 `setting.yaml` 中预置 `worker` + `explorer` 两个 kind 块 + 收窄后的
  `scheduler` 块（仅含 `model`，详见 §11.5.5），配上对应的 `prompts/worker.md` /
  `prompts/explorer.md` 文件（**scheduler 提示词保持 Go const，不外置文件**），
  确保用户克隆仓库后无需改动即可保持原有行为

### 11.7 Context Limit 与 Token 消耗追踪策略

`context_limit` 是 v4 配置格式的核心新增能力。它不依赖外部 tokenizer 库，而是基于 **SDK 返回的实测 `Usage` 数据 + 轻量新增内容估算** 实现精确管控。

#### 11.7.1 核心洞察：openai-go SDK 已返回实测 Token 数

`openai-go/v3` 的 `ChatCompletion` 响应结构体中包含 `Usage` 字段：

```go
completion.Usage.PromptTokens     // 本次请求实际消耗的 prompt token 数
completion.Usage.CompletionTokens // 本次请求实际消耗的 completion token 数
```

AgentGo 的 `internal/llm/client.go` 已在每次调用后提取这两个值（第 216-217 行）。这意味着**每次 LLM 调用后，我们都知道这条历史记录对应的实际 token 消耗**。

**关键认知**：`Usage.PromptTokens` 是**事后值**——请求发出去、模型处理完才返回。它不能替代"请求前的截断决策"，但可以作为**下一次请求的精确基准点**。

#### 11.7.2 双层阈值机制

```
历史消息 Token 长度
       │
       ▼
  ┌────────────┐  context_limit（硬上限，如 16000）
  │  强制截断   │  ← 超过时从最老消息开始丢弃（保留 system + 最近 N 条）
  └────────────┘
       │
  ┌────────────┐  enforce_compact_token_threshold（压缩阈值，如 4000）
  │  触发 Summary │  ← 超过时对老历史做 LLM 压缩，生成 condensed summary
  └────────────┘
       │
       └─ 正常区间，不做任何处理
```

#### 11.7.3 Token 追踪方案：实测锚定 + 轻量估算

**不引入 tiktoken-go，不自建复杂估算器**。策略分为两部分：

##### （1）HistoryEntry 逐条记录实测 Token 消耗

在 `HistoryEntry` 中增加两个字段，记录**产生该条 assistant 回复时**的实际消耗：

```go
type HistoryEntry struct {
    // ... 现有字段（Role, Content, ToolCalls, ExtraFields 等）
    PromptTokens     int    `yaml:"prompt_tokens,omitempty" json:"prompt_tokens,omitempty"`
    CompletionTokens int    `yaml:"completion_tokens,omitempty" json:"completion_tokens,omitempty"`
    Model            string `yaml:"model,omitempty" json:"model,omitempty"` // 产生该条 assistant 回复时使用的模型名（详见 §11.7.6 模型切换后实测值失效的应对）
}
```

**记录时机**：每次 `processTask` 中 LLM 调用返回后，将 `resp.Usage` 与当前生效模型名写入刚产生的 assistant 条目：

```go
history = append(history, HistoryEntry{
    Role:             "assistant",
    Content:          resp.Content,
    ToolCalls:        resp.ToolCalls,
    ExtraFields:      resp.ExtraFields,
    PromptTokens:     resp.Usage.PromptTokens,      // ← 实测值
    CompletionTokens: resp.Usage.CompletionTokens,  // ← 实测值
    Model:            currentModel,                  // ← 与 PromptTokens 同源——为模型切换后基准重置提供依据
})
```

**为什么记录在 assistant 条目上**：
- `PromptTokens` 描述的是"请求时整条历史的长度"，属于该轮对话的元数据
- 放在 assistant 条目上，自然形成"每轮一问一答都携带该轮的实际 token 开销"的结构
- 序列化到 `history.jsonl` 时不增加新文件类型

##### （2）下次请求前的长度预测：上次实测 + 新增估算

```go
func PredictNextPromptTokens(history []HistoryEntry, currentModel, newUserContent string) int {
    if len(history) == 0 {
        // 首次请求：无实测值，粗略估算
        return len(systemPrompt)/3 + len(newUserContent)/3 + 100
    }
    
    // 找到最近一条带有 PromptTokens 且模型与当前一致的 assistant 条目。
    // 模型一致性是基准成立的前提——不同模型 tokenizer 不同，跨模型实测值不可比
    // （详见 §11.7.6 模型切换后实测值失效的应对）。
    lastActual := 0
    lastIdx := -1
    for i := len(history) - 1; i >= 0; i-- {
        if history[i].Role == "assistant" && history[i].PromptTokens > 0 &&
            history[i].Model == currentModel {
            lastActual = history[i].PromptTokens
            lastIdx = i
            break
        }
    }
    
    if lastActual == 0 {
        // 历史中没有当前模型的实测值（首次请求 / 模型切换 / 兼容性保护），
        // 退化到粗略估算
        return estimateFromScratch(history)
    }
    
    // 计算上次 assistant 之后新增的内容
    // 包括：tool results、新的 user message、system prompt 不变
    added := 0
    for i := lastIdx + 1; i < len(history); i++ {
        added += len(history[i].Content) / 3  // 轻量估算新增部分
        for _, raw := range history[i].ExtraFields {
            added += len(raw) / 3
        }
    }
    // 加上本次即将发出去的新 user message
    added += len(newUserContent) / 3
    
    return lastActual + added
}
```

**精度分析**：

| 场景 | 纯自估算误差 | 实测锚定+新增估算误差 | 原因 |
|------|------------|---------------------|------|
| 英文为主 | ±25% | ±8% | 主体部分有实测值锚定 |
| 中文为主 | ±40% | ±10% | 新增部分占比小，估算误差被稀释 |
| 代码混合 | ±30% | ±7% | 工具定义等固定开销已被实测值覆盖 |

误差从"整条历史"缩小到"新增几条消息"，即使新增部分的 `/3` 估算有 ±30% 偏差，对最终二值决策（是否超过 16000）的影响也很小。

##### （3）Agent 级别的累计统计

每个 Agent 维护累计 Token 消耗，用于成本追踪和运行时监控：

```go
type Agent struct {
    // ... 现有字段
    TokenStats TokenStats
}

type TokenStats struct {
    TotalPromptTokens     int64
    TotalCompletionTokens int64
    CallCount             int
}
```

每次 LLM 调用后累加：

```go
a.TokenStats.TotalPromptTokens += int64(resp.Usage.PromptTokens)
a.TokenStats.TotalCompletionTokens += int64(resp.Usage.CompletionTokens)
a.TokenStats.CallCount++
```

可产出运行时日志：

```
[worker-1] 本轮: prompt=4213, completion=892 | 累计: prompt=38721, completion=8402, 调用=12
[explorer] 本轮: prompt=1892, completion=340 | 累计: prompt=12450, completion=2103, 调用=8
```

未来可在 YAML 中配置模型单价，实现**任务结束时的成本汇报**。

#### 11.7.4 截断策略（基于预测值）

> ✅ **状态：已修复（2026-04-26 同 session 接通）** — 4 步修复落地。
>
> **修复落地点**：
>
> 1. [agent.go:483-510](../../internal/agent/agent.go#L483-L510) — `processTask` 主循环 `Execute`
>    调用前插入 `TruncateHistory` 调用，覆盖每轮 LLM 请求。`a.ContextLimit <= 0` 时整段为
>    no-op（v3 兼容路径的零回归保险）。`ErrContextLimitTooSmall` 不 fail 任务，warn + 用
>    截断 history 继续，让 §9.3 ErrUnrecoverable 兜底——避免在截断阶段把任务标失败。
> 2. [internal/trace/event.go](../../internal/trace/event.go) 新增
>    `KindHistoryTruncated EventKind = "history_truncated"`；调用截断时 emit 一条携带
>    `PromptTokensBefore` / `PromptTokensAfter` / `KeptEntries` / `Strategy=head_keep_tail_keep`
>    的事件——下次复盘可直接 `grep history_truncated` 验证截断生效。
> 3. [setting.v4.yaml](../../setting.v4.yaml) explorer 的 `context_limit` 从 8000 上调至 16000
>    （2026-04-26 实测 scheduler 首调 prompt_tokens=8525，8000 会让 explorer 一启动就触发
>    硬截断），同时把 `enforce_compact_token_threshold` 6000，与 16000 ceil 形成合理梯度。
> 4. 回归用例分两层（双层防护）：
>
>    **函数级**（[token_truncate_test.go](../../internal/agent/token_truncate_test.go)，6 条）：
>    - `TestTruncateHistory_OverLimitGetsTruncated`：构造 `prompt_tokens > context_limit`
>      的人造历史 → 断言截断后 `PredictNextPromptTokens(...) ≤ context_limit`、头部第 0 条
>      不丢、尾部 `keepRecentForTruncate` 条不丢
>    - `TestTruncateHistory_NoOpWhenUnderLimit`：history 已在限制内时不动 history
>    - `TestTruncateHistory_ContextLimitZeroIsNoOp`：v3 兼容路径不触发截断
>    - `TestTruncateHistory_TooSmallReturnsErr`：物理下界 `ErrContextLimitTooSmall`
>    - `TestPredictNextPromptTokens_AnchorAndAdded`：实测锚 + 新增估算正确性
>    - `TestPredictNextPromptTokens_ModelSwitchResetsAnchor`：模型切换后基准重置
>
>    **端到端**（[truncate_e2e_test.go](../../internal/agent/truncate_e2e_test.go)，1 条）：
>    - `TestE2E_TruncateFiresOnContextLimit`：用真实 `processTask` 主循环 + fake executor
>      驱动 ContextLimit 触发，在临时 trace 目录读回 JSONL 断言至少一条 `KindHistoryTruncated`
>      事件且至少一条携带 `Before > After`（成功路径，非 `ErrContextLimitTooSmall` 退化）。
>      **这条专门防"装配漏接"——即使有人误删 agent.go 中的 TruncateHistory 调用点，
>      函数级 6 条单测仍全绿（函数没改），但本端到端测试会因 trace 里找不到事件而红**。
>
> 至此本节修复闭环：函数正确（函数级 6 条）+ 函数被调用（端到端 1 条 + trace 水印）+
> 配置上下文管理合理（explorer context_limit=16000）。
>
> **2026-04-26 实测复现的根因（修复前）**：
> - worker-2 task `75799b58` loop=0~9 反复 read_file 同一文件不同 offset，prompt_tokens
>   持续增长直至 `agent_max_loops=10` 耗尽触发 RetryRollback
> - 上一轮（v3 兼容层时代）worker-1 task `3d662b7d` 32 loops，prompt_tokens 涨到 20560
>   仍未被截 ——同一根因
>
> **设计反思（CLAUDE.md "Shipping conventions" 第 1 条的反复故障模式实证）**：
> 函数定义 + 字段定义 + 字段注入都是装配链条的"准备"，**真正生效需要主流程显式调用**。
> 单测全绿（函数本身正确）+ 烟测能 startup（启动期不调 TruncateHistory）= 漏 S7 不会被
> 任何已有验证捕获。今后类似多步骤特性（v4.md S6 + S7 这类）应当：
>   - 把 wire-into-main-loop 步骤独立列为 todo 项
>   - 加上 trace 事件作为"不变量水印"——能在 trace 复盘中肉眼断言"这个特性确实被运行时调过"
>   - 端到端烟测除了"binary 启动 + /quit"还要包含"发一个真实任务 + 断言关键 trace 事件"

```go
// keepRecentForTruncate 是 internal/agent 包级常量——截断时保护尾部最近的消息条数。
// 不进 YAML、不进 AgentKind——这是模型 context 完整性的物理需求（最近一对 request/
// response 不能丢，否则 LLM 失忆），不属于 per-kind 调优维度。
// 当前值为 6（覆盖最近 ~3 对 request/response）。未来若需灵活性，通过环境变量覆盖
// （如 AGENTGO_KEEP_RECENT_FOR_TRUNCATE=8），仍不进 YAML。
const keepRecentForTruncate = 6

func TruncateHistory(history []HistoryEntry, currentModel string, contextLimit int) ([]HistoryEntry, error) {
    predicted := PredictNextPromptTokens(history, currentModel, "")
    
    if predicted <= contextLimit {
        return history, nil
    }
    
    // 保护不可删除的部分
    protectedHead := 1                       // system prompt（第 0 条）
    protectedTail := keepRecentForTruncate   // 最近 N 条不可丢（包级常量）
    
    for PredictNextPromptTokens(history, currentModel, "") > contextLimit &&
          len(history) > protectedHead + protectedTail {
        // 删除 protectedHead 之后最老的一条
        history = append(history[:protectedHead], history[protectedHead+1:]...)
    }
    
    if PredictNextPromptTokens(history, currentModel, "") > contextLimit {
        return history, ErrContextLimitTooSmall
    }
    return history, nil
}
```

**关键约束**：
- 截断发生在**每次 LLM 调用前**（`agent.go` 的 `processTask` → `buildMessages` 阶段）
- 截断后必须保证消息序列仍满足 OpenAI 格式约束（`assistant(tool_calls)` 后必须紧跟对应的 `tool` 消息）
- 如果删除某条 `assistant(tool_calls)`，必须同时删除其后直到下一条非 `tool` 消息之前的所有 `tool` 消息

**与 v3 `CompactKeepRecent` 的关系**：v3 [config.CompactKeepRecent](../../internal/config/config.go#L71)
是 layer-1 历史压缩（[snipOldToolResults](../../internal/agent/agent.go#L398)）使用的"压缩
时保留最近 N 条"参数，YAML 可配（默认 3）。**v4 的语义调整**：
- 该字段从 YAML 中**移除**——layer-1 压缩与本节截断都改用 `internal/agent` 包级常量
- v3 的"压缩保留 N=3"与 v4 的"截断保护 N=6"虽然数值不同但本质同源（都是"保护最
  近 N 条不被丢弃"），未来若拆分需求出现可分别命名，当前合并为单一常量族管理
- 用户**不再能通过 YAML 调节这一行为**——属于系统稳定性内禀参数，与
  `MailChainMaxDepth` 同档（详见 §11.5.4 系统级常量）

#### 11.7.5 与 Compact 机制的协作

`enforce_compact_token_threshold` 和 `context_limit` 不是互斥的，而是**协同工作**：

```
Token 长度
    │
16000 ├──────────── context_limit（硬上限，截断）
    │     ╱
 6000 ├────╱─────── enforce_compact_token_threshold（软阈值，触发 summary）
    │   ╱
    └─╱──────────── 正常区间
```

- **`< enforce_compact_token_threshold`**：不做任何处理，完整历史直送 LLM
- **`≥ enforce_compact_token_threshold`**：触发历史压缩（summary），由 Agent 自己发起 compaction，用 LLM 生成老历史的 condensed summary
- **`≥ context_limit`**：即使还未压缩到阈值以下，**强制截断**最老的消息，确保不超限

两者的关系：**压缩是主动的语义保留，截断是被动的物理丢弃**。压缩优先于截断——如果历史超过了 `enforce_compact_token_threshold` 但未超过 `context_limit`，Agent 会尝试用 LLM 压缩；如果压缩后仍然超过 `context_limit`，则进入强制截断。

#### 11.7.6 局限与应对

| 局限 | 说明 | 应对 |
|------|------|------|
| **首次请求无实测值** | 历史为空时，没有 `lastActual` 可参考 | 首次请求用粗略估算（`/3`），从第二次开始有实测值锚定 |
| **工具定义未计入 HistoryEntry** | `PromptTokens` 包含 tools schema，但 history 中不存储 tools | 工具定义长度相对固定，可被实测值自然吸收；新增工具时首条请求的实测值会更新基准 |
| **模型切换后实测值失效** | 不同模型的 tokenizer 不同，之前的 `PromptTokens` 不再可比 | 在 `HistoryEntry` 中同时记录 `Model` 字段；切换模型时重置基准，首条请求退化到粗略估算 |
| **Reasoning content 的 token 数** | DeepSeek thinking 模式下 `reasoning_content` 是否计入 `PromptTokens` | 由模型/API 决定，SDK 返回的 `Usage` 已经包含，应用层无需关心 |

### 11.8 实施步骤

| 步骤 | 内容 | 产出 |
|------|------|------|
| S1 | 定义 v4 Config 结构体（`internal/config/config.go` 整体重写），与 v3 完全隔离；新增路径校验逻辑（拒绝反斜杠 + `filepath.FromSlash`）；**LoadConfig 在反序列化前对原始 YAML 文本做一次 [`os.ExpandEnv`](https://pkg.go.dev/os#ExpandEnv) 替换**，支持 `${ENV_VAR}` 语法（详见 §11.3 末尾"环境变量替换"段）| 新 schema |
| S2 | 实现 `buildAgentRuntime` / `buildSchedulerRuntime` / `resolveDependencies`：从 AgentKind + LLMConfig 合成 `AgentRuntimeConfig`，按 AllowedTools 查表注入运行时依赖 | 合并逻辑 + 单测 |
| S3 | 新增统一 agent runner（`internal/agent/runner.go`）：取代 `internal/worker` 与 `internal/explorer`，接收 `AgentRuntimeConfig` + 共享依赖，内部组装 ToolGroup、调用 `agent.Agent` | 通用 runtime |
| S4 | **删除** `internal/worker` 和 `internal/explorer` 两个 package；`BuildTeamSnapshot` / `currentTaskHolder` 迁移到 `internal/agent`；`MetaGroup.DisablePublishTask` 标志位删除 | 旧代码下线 |
| S5 | 改写 `bootstrap.go`：按 v4 Config 流程启动所有组件（缺失 setting.yaml 即终止；按 kinds 列表批量实例化 runner；新增 `printStartupBanner` + `startupProbe` 调用，详见 §9.5 / §11.6.1 step 2.5）| 新启动流程 |
| S6 | `HistoryEntry` 增加 `PromptTokens` / `CompletionTokens` / `Model`；`processTask` 中记录实测值；实现 `PredictNextPromptTokens`（实测锚定 + 新增估算）+ `TruncateHistory` | token 追踪 + 截断 |
| S7 | 在 `agent.go` `buildMessages` 阶段接入截断逻辑 | 运行时保护 |
| S8 | 默认 `setting.yaml` 重写：预置 `worker` + `explorer` 两个 kind 块 + 收窄后的 `scheduler` 块（仅含 `model`，详见 §11.5.5）；新增 `prompts/worker.md` / `prompts/explorer.md` 文件，内容来自原 Go 常量；**scheduler 提示词保持 Go const**（不外置文件）| 出厂配置 |
| S9 | 启动校验：见 §11.5.3 全部 11 条 | 防御性校验 |
| S10 | 端到端烟测：启动 → 发一个长任务 → 确认历史压缩和截断工作正常；用 `setting.yaml` 删除测试验证 fail-fast 路径 | 回归保证 |
| S11 | ✅ **已完成**（2026-04-26）— 实现为 [terminal_emit_symmetry_test.go](../../internal/agent/terminal_emit_symmetry_test.go) AST 静态扫描 + [panic_emit_test.go](../../internal/agent/panic_emit_test.go) 运行时双重验证。扫描目标实际为 `internal/agent/agent.go`（不是 spec 原文写的 `internal/agent/runner.go`——`internal/runner/runner.go` 仅是薄壳，终结状态转换仍位于 `agent.go` 的 `processTask` + `terminateTask` + `Run defer`）。落地时立即发现并修复一处真缺陷：panic-recovery 路径 FailTask 后未 emit KindTaskFailed | 装配漏接防护 |

### 11.9 不在本节范围

- **v3 配置格式的自动迁移工具**：v4 不向后兼容，升级时人工迁移一次即可。不维护迁移脚本。
- **配置热重载**：Agent 启动后配置不可变。运行时行为调整靠重启。
- **Per-tool 权限模板**：v4 附录 A 分级权限模型是独立功能，本节只解决"Agent 行为参数配置"，不涉及任务级权限裁剪。
- **跨 kind 的 ProjectRoot 越界**：运行时工具仍受 `pathutil.ValidatePath` 约束，详见 CLAUDE.md "Runtime file access scope"。该放开点在 Tool Hook + 能力声明阶段二落地后处理，不在本 §11 范围。
- **Tokenizer 依赖决策**：本节不引入 tiktoken-go 等外部 tokenizer。Token 长度管理完全基于 SDK 返回的 `Usage.PromptTokens` 实测值 + 轻量新增估算。如果未来实测值策略被证明不够精确，再考虑引入 tiktoken-go 作为可选后端。
- **kind 内部模板继承机制**：YAML 锚点（`&` / `*`）已是原生 DRY 工具，不引入自定义 `agent_templates` 复用层。
- **同 kind 内异质实例**：同 kind 的 N 个实例完全同质（同工具集 / 同提示词 / 同模型）。需要异质就声明多个 kind。这是 §1（per-worker profile）方案被取代的核心理由。

---

## 附录

> 本附录收录"§11 落地后预期之内的问题及其应对方案"。每个附录章节都有**严格的触发
> 条件**——条件未触发时，该方案保持冷启动状态；触发后再激活实施。这是渐进式提升
> 的明确路径：主线（§11）尽量保持简单，特定问题出现时才接入对应附录章节。
>
> **判断要点**：附录方案的实现复杂度往往高于"在 §11 内穷举更多 kind / 工具组合"
> 的复杂度。只有当穷举法已经导致配置爆炸或维护成本不可接受时，才考虑接入附录。

### 附录 A. 分级权限模型（PermissionMode）

> 原 v4 §2 → v4 附录 A 迁入（2026-04-25）| 优先级：P3
> 历史血脉：原 v2 §1.4 → v3 §9.2 迁入
> 前置依赖：v4 §11 统一 Agent 声明式配置
> 关联：v4 §11.6.4 Board snapshot 按 kind 聚合、v4 §3 能力声明阶段二
> 状态：📝 触发条件未满足（详见 A.4）

#### A.1 背景

不同任务的风险等级不同——"搜索调研"任务不应持有 `write_file / run_shell`，
"代码修改"任务不应持有 `web_fetch`。工具集与任务风险不匹配会放大 LLM 幻觉导致
的破坏面。

§11 的 kind 体系让不同 kind 拥有不同工具集，这是"**静态分级**"——同一个 kind
内的所有任务共享同一工具集。本附录章节的分级权限模型是"**动态分级**"——同一个
kind 的 agent 在不同任务中使用不同权限。

#### A.2 改进方向

- **任务级工具裁剪**：`Task` 结构体新增 `AllowedTools []string` 和/或
  `DisallowedTools []string` 字段，Scheduler 在 `publish_task` 时指定
- **预设权限模板**：定义命名权限等级（如 `readonly`、`standard`、`privileged`），
  Scheduler 通过模板名快速指定
- **运行时权限提升**：Agent 执行中发现需要额外工具时，通过 `permission_request`
  协议向 Scheduler 申请临时提权

#### A.3 与 v4 §11 的关系（互补，非替代）

- **静态分级（§11）**：启动时由 kind 确定，适合长期角色分工
- **动态分级（附录 A）**：任务级确定，适合临时权限裁剪

§11 是默认走的方案；本附录章节是针对 §11 覆盖不到的"同 kind 内任务风险差异显著"
场景的后备。

#### A.4 触发条件（严格）

启用本附录章节需要**两个条件同时满足**：

1. **同一个 kind 的 agent 需要在不同任务中使用不同权限**——并且靠"穷举更细粒度
   的 kind"已经导致配置爆炸或维护成本不可接受
2. **风险敞口确实存在**——实测出现过"agent 在低风险任务里误用高风险工具"的事故，
   或经过威胁建模后判定该风险不可接受

只满足条件 1 而条件 2 不成立 → 容忍 kind 数量增加，不启用动态分级（kind 穷举的
复杂度仍低于动态权限系统的实现复杂度）。

只满足条件 2 而条件 1 不成立 → 通过 §11 拆分新 kind 解决，不需要本附录章节。

### 附录 B. 管理员信赖标记（SourceAdminTrusted）

> 原 v4 §4 → v4 附录 B 迁入（2026-04-25）| 优先级：P4
> 历史血脉：原 v2 §1.6 → v3 §9.3 迁入
> 前置依赖：待引入外部代理 / 插件机制 / MCP 接入后
> 关联：v4 附录 A 分级权限模型（信任标记作为权限模板选择的输入之一）
> 状态：📝 触发条件未满足（外部 agent 入口未引入）

#### B.1 背景

**对标 Claude Code 的 `isSourceAdminTrusted` 机制**——当系统未来支持用户自定义
代理、外部插件代理、或通过 MCP 接入外部 agent 时，需要区分"可信来源"和"不可信
来源"，限制不可信代理的工具访问与跨代理通信能力。

当前所有 agent 都由 `internal/bootstrap` 启动期硬编码创建，天然全部可信，因此
无需区分信任级别。本附录章节是面向未来"打开外部 agent 入口"的纵深防御预案。

#### B.2 改进方向

- **代理来源标记**：Agent 结构体新增 `Source string`（如 `"system"` / `"user"` /
  `"plugin"` / `"mcp"`）和 `Trusted bool` 字段
- **信任级别与工具映射**：不可信代理自动降级为只读工具集，**且不注入 mailbox 的
  `send_message` 工具**——这一条不是可选的。理由：恶意代理即便自身没有 shell /
  web / 写权限，只要能发邮件，就能向高权限的 system agent 发送
  `<agent-mail type="steer">` 借刀杀人（横向移动攻击）。把 send_message 从不可信
  代理的工具表里剥离，是阻断这条攻击链的关键一刀
- **配合分级权限模型**：信任标记作为附录 A 权限模板选择的输入之一（trusted ×
  permission_template 的二维矩阵）
- **Hook / MCP 桥接层硬隔离（最终态）**：不可信代理在编译期就拿不到危险服务的
  引用——MCP 客户端句柄、Tool Hook 注册接口、scheduler 内部状态等都需要
  `Trusted=true` 断言才能获取，从根源截断恶意代理调用危险服务的可能性。这一层
  比"工具列表过滤"更深，靠语言层 visibility + 接口拆分实现，不依赖运行时检查

#### B.3 触发条件（任一即可启动）

1. AgentGo 引入用户自定义代理机制（YAML 中允许声明用户编写的 Go agent）
2. AgentGo 引入第三方插件 agent 加载机制
3. AgentGo 通过 MCP 接入外部 agent

**关键纪律**：任一条件触发即应**同时**启动本章节，不要让"外部 agent 入口"
先于信任标记落地——否则启用入口的当天就是默认全部可信的当天，纵深防御为零。
PR 评审时把"是否同时引入了 Source/Trusted 字段"作为 hard gate。

### 附录 C. 冲突避免长期方案

> 原 v4 §5 → v4 附录 C 迁入（2026-04-25）| 优先级：P3
> 历史血脉：原 v2 §3.2 → v3 §9.7 迁入
> 前置依赖：v3 §8.3 文件冲突排队（✅ 已完成 2026-04-12，过渡方案在跑）
> 关联：v4 §11 统一 Agent 声明式配置（kind × replicas 增加后冲突频率随实例数平方上升）
> 状态：📝 触发条件未满足（迄今未观察到任何文件写冲突事件）

#### C.1 背景：一个有意保留的设计债

早期 AgentGo 用 git worktree 给每个任务做物理隔离（每个任务自己的目录，互不干扰）。
**2026-04-09 删除 git 依赖**后，所有 worker 共享 `ProjectRoot`，"两个 worker 并发
写同一文件 → 后写覆盖前写"成为**故意暴露**的退化（参见 `docs/archived/nextUpgrade_v2.md`
§3.2）。这是已知的设计债——项目方决定先用简单方案过渡，长期再彻底解决。

**当前的过渡方案（v3 §8.3，2026-04-12 commit `f6552d4` 已上线）**：

| 组件 | 职责 |
|---|---|
| `roster.WaitForRelease(ctx, agentID, filePath, timeout)` | Roster 接口新增等待方法 |
| `MemoryRoster.waiters` | FIFO 等待队列；冲突时 agent 排队，前任 Release 时唤醒队首 |
| `claimOrWait` helper（`internal/tools/local_write.go`）| 让 LLM 感知不到排队（透明排队）|
| `RosterWaitTimeoutSec`（默认 30s）| 排队上限，超时回退报错 |
| `KindFileWriteQueued` trace 事件 | 记录 `QueueLen` / `WaitMS` 用于排查 |

**核心特点**：**事后排队**——冲突已经发生了，让晚来的 agent 等。本附录章节是从
"事后等"升级到"**事前避免**"的长期方案。

#### C.2 改进方向（分阶段递进，不是平行三选一）

三条改进**有先后依赖**，应当按阶段实施：

**阶段 1（最浅）— Scheduler 层面：boardSnapshot 暴露文件占用**

在 `boardSnapshot.resources` 中暴露各 agent 当前修改的文件列表（来自 Roster）。
让 scheduler LLM 在 `publish_task` 拆解任务时主动避开"会撞同一文件"的并发分配。

- **优点**：实现成本低，仅 board snapshot 数据扩展 + scheduler prompt 微调
- **局限**：**概率性防御**——LLM 可能没看仔细、可能误判任务文件影响面

**阶段 2（中等）— Roster 意图声明**

把 Roster 从"写时锁"升级为"**意图声明**"。Agent 在**真正动手前**先声明"我打算
修改这些文件"，让其他 agent / scheduler 看见。

- **优点**：把阶段 1 的概率防御变成**显式信号**，不靠 LLM 注意力
- **新增接口**：`Roster.DeclareIntent(agentID, filePaths, ttl)`、`Roster.ListIntents()`
- **难点**：意图与实际写入的同步（agent 可能声明了但没写，或写了没声明）

**阶段 3（最深）— Mailbox 间协调**

Agent 之间发现冲突苗头时，通过 `send_message` 直接协商分工（"你先改这个，我等你"
或"这个改动我全包了，你不用碰"）。

- **优点**：去中心化协调，scheduler 减负
- **风险**：邮件级联可能爆炸（已有 `MailChainMaxDepth` 兜底，但仍需 prompt 谨慎设计）

阶段 1 完成后多数冲突应已被消除；只有当阶段 1 数据表明 LLM 注意力不够可靠时，
才上阶段 2；阶段 3 是更远的目标，等阶段 2 实测后再评估。

#### C.3 与 §11 的关系

§11 引入 `kind × replicas` 后，同一 kind 的 replicas 完全同质——如果
`worker_standard.replicas=5`，5 个 worker 都能写文件。**冲突概率随 replicas
平方上升**（n 个 agent 两两组合 = n(n−1)/2）。所以本附录章节的触发条件天然
由 §11 落地后的实测数据决定。

#### C.4 触发条件（量化指标）

启用本附录章节需要**任一**满足：

1. **过渡方案排队事件频繁**：`KindFileWriteQueued` 事件 / 任务数 > 0.3，或
   单任务平均 `WaitMS` > 5000
2. **超时回退事件出现**：`WaitForRelease` 因 30s 超时返回错误的次数 > 0（一旦
   出现说明排队队列已积压到不可接受）
3. **写覆盖事故实测发生**：尽管有排队，仍观察到"后写覆盖前写"的数据丢失（这种
   情况下哪怕单次也应启动）

**为什么需要量化指标**：原 v4 §5 写"冲突频率过高"时没说阈值，触发判断变成主观
裁量，结果就是永远不会被触发。明确数字之后，可以直接基于 trace event 数据自动
评估。

**当前实测**（2026-04-25）：迄今未观察到任何 `KindFileWriteQueued` 事件，写冲突
为零。本附录章节冷启动状态。

### 附录 D. Agent 休眠/唤醒优化（Suspend/Resume）

> 原 v4 §6 → v4 附录 D 迁入（2026-04-25）| 优先级：P4
> 历史血脉：原 v2 §3.5 → v3 §9.8 迁入
> 前置依赖：v4 §11 落地后实测 agent 总数（kind × replicas 累加）超过阈值
> 状态：📝 触发条件未满足（项目长期预期 agent 总数 < 10）

#### D.1 背景：为什么早期选了"拉取模式"

当前所有 agent 通过 500ms 周期轮询 `store.QueryAvailable(eventType)` 主动拉取
任务（pull）。**这不是"先简单用着"，而是早期阶段经过权衡的有意选择**，目的是
规避 push 模式的两个具体缺陷：

1. **盲目推送的雷鸣群（thundering herd）**：如果在 `store.PublishTask` 时
   broadcast 唤醒所有 idle agent，N 个 agent 同时被唤醒去抢同一任务，只有 1 个
   能 `ClaimTask` 成功，其余 N-1 个唤醒就是纯浪费。Agent 数量大时，单次发布产生
   的总唤醒成本随 N 线性上升，CPU / 上下文切换 / store 锁争用都会成为瓶颈。
2. **失去拉取期的隐式协调**：拉取模式下，第 1 个 agent 拉到任务后，第 2 个 agent
   在下一次轮询时**看到的是更新后的公告板状态**（前一任务已被认领），可以基于
   完整的当前状态选自己的任务、规划自己的策略。这种"基于最新快照决策"的能力，
   在 broadcast 唤醒模型下被打破——多个 agent 在 race 期看到的是同一个旧快照。

换句话说：**拉取模式不只是省事，它本身提供了一种轻量级的隐式协调**。500ms 的
poll 间隔是一个故意的速率限流器，让 agent 之间天然分时认领。

#### D.2 何时这套设计开始失效

- 1 个 worker：每秒 2 次轮询，CPU 完全可忽略
- 10 个 worker：每秒 20 次轮询，仍然可忽略（项目长期所处档位）
- 20+ worker：每秒 40+ 次轮询 + store 锁争用上升，开始成为可见的 CPU 浪费

到这个规模下，"持续轮询"的成本超过了它带来的协调收益，需要换更高级的方案。

#### D.3 改进方向（任何方案都必须同时解决两个原始问题）

**关键约束**：替换为 push 模式时，**不能**简单用 `sync.Cond.Broadcast()` 复活
雷鸣群问题；**也不能**丢失"基于最新快照决策"的协调收益。

候选方案（**分层互补，不是三选一**）：

- **动态间隔（兜底层）**：保留拉取，但空闲越久间隔越大（1s → 2s → 5s），新任务
  到达时通过 channel 信号重置回 500ms。即便 push 通道异常也仍能正常工作——这是
  最低风险的改进，**应当作为第一阶段实施**
- **定向唤醒（Signal 而非 Broadcast）**：每个 event_type 维护一个等待 agent 的
  单链表，新任务发布时 `Signal` 仅唤醒队首一个 agent。**消除雷鸣群同时保留隐式
  协调**——队首拿到任务后队列推进，下一次唤醒看到的就是更新后的板面快照
- **分桶唤醒**：按 event_type 分桶，仅唤醒该桶内的 idle agent。粒度比 broadcast
  细，比 Signal 粗，作为"agent 数极大但 event_type 不多"场景的折中方案

实施顺序建议：先做"动态间隔"作为低风险改进观察实测数据，再视情况上"定向唤醒"。
"分桶唤醒"在 §11 的 kind × replicas 模型下意义有限（多个 kind 通常对应多个
event_type），可暂不考虑。

#### D.4 触发条件（量化指标）

启用本附录章节需要**任一**满足：

1. **Agent 总数 ≥ 20**——所有 kind 的 replicas 累加（不是单一 kind 的 replicas）
2. **空闲 CPU 占用可测**：pprof 实测 idle agent 的轮询循环占用单核 CPU > 5%
3. **store 锁竞争中位数 > 1ms**：`QueryAvailable` 调用平均阻塞时长达到这个量级，
   说明轮询已经开始成为系统瓶颈

**当前实测**（2026-04-25）：项目长期预期 agent 总数 < 10，2 / 3 两条指标在该规模
下不会触发。本附录章节冷启动状态，无近期实施计划。

