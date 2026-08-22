package config

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"

	"agentgo/internal/modes"

	"gopkg.in/yaml.v3"
)

// ============================================================
// nextUpgrade_v4.md §11.4 — v4 配置类型（增量引入）
//
// 落地策略：v4 类型与 v3 顶层字段并存于本 Config。Bootstrap 当前仍走 v3 路径；
// v4 字段（LLM / Agents / Scheduler / Infra / StartupProbe*）由 Validate() 单独校验，
// 由后续 runner-based bootstrap 重写时消费。
// ============================================================

// LLMConfig 全局 LLM 默认值（v4 §11.4）。
// per-kind 通过 AgentKind.Model 覆盖默认模型；BaseURL/APIKey/TimeoutSec 共用。
// Provider 字段已于 V6 移除（AgentGo 只实现 OpenAI-compatible Chat Completions，
// 不再按 provider 分支做请求变换）；结构体保留该字段仅为让旧 YAML 仍能解析，
// Validate() 会对非空值返回明确的迁移诊断错误。
type LLMConfig struct {
	BaseURL      string `yaml:"base_url" json:"base_url"`
	APIKey       string `yaml:"api_key" json:"api_key"`
	DefaultModel string `yaml:"default_model" json:"default_model"`
	TimeoutSec   int    `yaml:"timeout_sec" json:"timeout_sec"`
	Provider     string `yaml:"provider,omitempty" json:"provider,omitempty"`
	// ReasoningEffort maps to the OpenAI Chat Completions reasoning_effort
	// request parameter. Empty means omit the parameter and let the selected
	// model/provider choose its default. Validation accepts the union of values
	// currently documented by OpenAI models.
	ReasoningEffort string `yaml:"reasoning_effort,omitempty" json:"reasoning_effort,omitempty"`
	// Stream switches the SDK transport to Chat Completions SSE streaming. The
	// client still returns one fully accumulated Response so ReAct/tool semantics
	// remain unchanged while live deltas can be observed by the UI.
	Stream bool `yaml:"stream" json:"stream"`
}

// OpenAIReasoningEfforts is the current union of reasoning-effort values
// documented across OpenAI reasoning models. Individual models may support a
// subset; that model-specific capability remains authoritative at request time.
var OpenAIReasoningEfforts = []string{
	"none", "minimal", "low", "medium", "high", "xhigh", "max",
}

func isOpenAIReasoningEffort(value string) bool {
	for _, candidate := range OpenAIReasoningEfforts {
		if value == candidate {
			return true
		}
	}
	return false
}

// AgentKind 一个 agent 种类的声明（v4 §11.4）。
// 同 kind 的多个实例（replicas 个）完全同质——同工具集、同提示词、同模型。
// 异质化通过声明多个 kind 实现。
type AgentKind struct {
	Kind             string   `yaml:"kind" json:"kind"`
	Replicas         int      `yaml:"replicas" json:"replicas"`
	EventType        string   `yaml:"event_type,omitempty" json:"event_type,omitempty"`
	Profile          string   `yaml:"profile,omitempty" json:"profile,omitempty"`
	Tools            []string `yaml:"tools,omitempty" json:"tools,omitempty"`
	Model            string   `yaml:"model,omitempty" json:"model,omitempty"`
	SystemPromptFile string   `yaml:"system_prompt_file" json:"system_prompt_file"`
	// AgentMaxLoops 已于 V6 移除（固定循环上限不再是终止条件，见
	// docs/nextUpgrade-V6.md §5 升级思路 5/6/8）。结构体保留该字段仅为让旧
	// YAML 仍能解析，Validate() 会对非零值返回明确的迁移诊断错误。
	AgentMaxLoops  int `yaml:"agent_max_loops" json:"agent_max_loops"`
	TaskMaxRetries int `yaml:"task_max_retries" json:"task_max_retries"`
	// EnforceCompactTokenThreshold 已由 Context v3 的 Snapshot-pressure
	// projection 取代；保留解析位只为给旧 YAML 明确迁移诊断。
	EnforceCompactTokenThreshold int `yaml:"enforce_compact_token_threshold" json:"enforce_compact_token_threshold"`
	// ContextLimit 已于 V6 移除（固定上下文硬限截断层与 history_truncated 事件
	// 一并删除，见 docs/nextUpgrade-V6.md §7.4；上下文适配由 L2 压缩与 L3 溢出
	// 重试承担）。结构体保留该字段仅为让旧 YAML 仍能解析，Validate() 会对
	// 非零值返回明确的迁移诊断错误。
	ContextLimit int `yaml:"context_limit" json:"context_limit"`

	// Description 是给 scheduler 看的一句话角色描述（人工撰写的语义提示词）。
	//
	// 用途：scheduler 在派发任务时，把所有 kind 的 description 集成到 board snapshot
	// 的 agent_capabilities 段，让 scheduler LLM 据此选择把任务派给哪个 kind。
	//
	// 写作建议：
	//   - 单句话，动作导向。例：「广度优先的网络调研代理，不写文件，仅返回 Markdown 文字回复」
	//   - 强调能力边界（"能 / 不能"）和典型工作风格（输出形态、深度倾向）
	//   - 避免和 Tools 列表重复——后者已经是机器可读的离散信息
	//
	// 为空时降级到 bootstrap 自动生成的 "kind=X（监听 event_type=...）" 字符串，
	// 保持向后兼容。未来 Capabilities 离散类型化时会与本字段并存（Description 用于
	// 语义优选，Capabilities 用于硬性筛选）。
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

// SchedulerKind scheduler 独立块（v4 §11.5.5）。
// 工具集 / 系统提示词 / replicas 仍由 internal/scheduler 固定；模型可覆盖。
type SchedulerKind struct {
	Model string `yaml:"model,omitempty" json:"model,omitempty"`
	// AgentMaxLoops 已于 V6 移除（与 agents[*].agent_max_loops 同步删除）。
	// 保留解析位以便 Validate() 对显式设置的旧配置给出迁移诊断。
	AgentMaxLoops                int `yaml:"agent_max_loops,omitempty" json:"agent_max_loops,omitempty"`
	EnforceCompactTokenThreshold int `yaml:"enforce_compact_token_threshold,omitempty" json:"enforce_compact_token_threshold,omitempty"`
	// ContextLimit 已于 V6 移除（与 agents[*].context_limit 同步删除）。
	// 保留解析位以便 Validate() 对显式设置的旧配置给出迁移诊断。
	ContextLimit int `yaml:"context_limit,omitempty" json:"context_limit,omitempty"`
}

// ModesConfig 两轴工作模式声明（modes: 块，v5 三轴模式；gate 轴已于 V6 C6c 整体移除）。
//
// 两轴相互正交、可任意组合：
//   - exec：执行权限轴 —— normal（默认）/ strict / readonly / yolo
//   - topo：编排拓扑轴 —— team（默认）/ solo
//
// 字段为空 = 该轴取默认值。
//
// Gate 字段仅为迁移诊断保留解析位：gate 轴已整体移除（执行前审阅改由
// Graph approval 节点承担），显式设置任何非空值都在 Validate 报 V6 迁移诊断。
type ModesConfig struct {
	Gate string `yaml:"gate,omitempty" json:"gate,omitempty"`
	Exec string `yaml:"exec,omitempty" json:"exec,omitempty"`
	Topo string `yaml:"topo,omitempty" json:"topo,omitempty"`
}

// AgentTemplatesConfig controls the optional external AgentTemplate catalogs
// and the process-wide limit for agents provisioned from templates. Builtin
// templates are always available and do not need to be listed here.
//
// Enabled 是整套动态组队机制的总开关（2026-08-20 起默认关闭）：关闭时
// Scheduler 不注册 list_agent_templates / provision_agent_team，资源快照
// 不含 agent_templates，图节点只能路由静态 YAML Agent；模板机制在静态
// Agent 流程验证稳定前有意搁置，重新开放时需同时恢复提示词组队教程。
//
// UserDirs are loaded into the user/* namespace. ProjectDirs are resolved
// relative to ProjectRoot and loaded into project/*. Missing directories are
// ignored; malformed templates in an existing directory fail startup.
type AgentTemplatesConfig struct {
	Enabled          bool     `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	UserDirs         []string `yaml:"user_dirs,omitempty" json:"user_dirs,omitempty"`
	ProjectDirs      []string `yaml:"project_dirs,omitempty" json:"project_dirs,omitempty"`
	MaxRuntimeAgents int      `yaml:"max_runtime_agents,omitempty" json:"max_runtime_agents,omitempty"`
}

// InfraConfig 非 Agent 运行时基础设施（v4 §11.4）。
// 子类型独立命名（不用匿名嵌套 struct），便于单测、扩展与 IDE 跳转。
type InfraConfig struct {
	Watchdog     WatchdogConfig     `yaml:"watchdog" json:"watchdog"`
	MailNotifier MailNotifierConfig `yaml:"mail_notifier" json:"mail_notifier"`
	Store        StoreConfig        `yaml:"store" json:"store"`
	Roster       RosterConfig       `yaml:"roster" json:"roster"`
}

type WatchdogConfig struct {
	IntervalSec int `yaml:"interval_sec" json:"interval_sec"`
	// ProgressHeartbeatGraceSec 是新 Loop 的 checkpoint lease。超过此窗口
	// Watchdog 只发布 typed heartbeat_stalled observation，不迁移 Task 状态。
	ProgressHeartbeatGraceSec int `yaml:"progress_heartbeat_grace_sec" json:"progress_heartbeat_grace_sec"`
	// PendingAlertGraceSec is the age of one pending queue lease after which Watchdog
	// reports claim starvation. A runnable route is only alerted, never failed.
	PendingAlertGraceSec int `yaml:"pending_alert_grace_sec" json:"pending_alert_grace_sec"`
	// UnroutableGraceSec is a separate observation window that starts only
	// after a Task is otherwise claimable but has no compatible runtime route.
	// Expiry moves that Task to blocked with a structured reason.
	UnroutableGraceSec int `yaml:"unroutable_grace_sec" json:"unroutable_grace_sec"`
}

type MailNotifierConfig struct {
	Enabled     bool `yaml:"enabled" json:"enabled"`
	IntervalSec int  `yaml:"interval_sec" json:"interval_sec"`
}

type StoreConfig struct {
	EventChannelBuffer int `yaml:"event_channel_buffer" json:"event_channel_buffer"`
	FIFOLimit          int `yaml:"fifo_limit" json:"fifo_limit"`
	DefaultConcurrency int `yaml:"default_concurrency" json:"default_concurrency"`
	// DefaultTimeoutSec 保留旧配置键，实际只填充 Task.ExpectedDuration（SLO/UI）。
	// 它不生成 deadline；新 Loop 的控制权来自 RunContract/ProgressCheckpoint。
	// Deprecated: 配置键 default_timeout_sec 仅作兼容别名。
	DefaultTimeoutSec int `yaml:"default_timeout_sec" json:"default_timeout_sec"`
}

type RosterConfig struct {
	WaitTimeoutSec int `yaml:"wait_timeout_sec" json:"wait_timeout_sec"`
}

// UIConfig 是 UI Hub 的前端配置块（v4 风格嵌套）。
//
// Frontends 声明启用哪些前端：tui（终端 UI，默认唯一启用）/ web（Web
// Dashboard，含受 token 保护的控制端点）。多个前端可同时启用——UI Hub 是事件源的唯一消费者，
// 前端只是订阅者。
type UIConfig struct {
	Frontends []string    `yaml:"frontends" json:"frontends"`
	Web       WebUIConfig `yaml:"web" json:"web"`
}

// WebUIConfig 是 Web Dashboard（internal/dashboard）的监听配置。
//
// Token 为空表示不启用鉴权——仅在监听地址为 loopback 时允许（默认
// 127.0.0.1:8399）。绑定到非 loopback 地址必须设置 token，否则
// Validate 拒绝启动（本系统具备 shell 执行能力，其管理面绝不能无鉴权
// 暴露到局域网/公网）。
type WebUIConfig struct {
	Listen string `yaml:"listen" json:"listen"`
	Token  string `yaml:"token" json:"token"`
	// AutoOpen 控制启动后是否自动用默认浏览器打开 Web 控制台。
	// 指针三态：未设置（nil）= 默认开；显式 false 关闭。
	AutoOpen *bool `yaml:"auto_open,omitempty" json:"auto_open,omitempty"`
}

// AutoOpenEnabled 报告是否启用"启动后自动打开浏览器"（缺省为关）。
func (c WebUIConfig) AutoOpenEnabled() bool {
	return c.AutoOpen != nil && *c.AutoOpen
}

// HasFrontend 报告指定前端是否启用（frontends 去重后的成员判断）。
func (c UIConfig) HasFrontend(name string) bool {
	for _, f := range c.Frontends {
		if f == name {
			return true
		}
	}
	return false
}

// AgentRuntimeConfig 内部使用，由 Bootstrap 从 AgentKind + LLMConfig 合成后注入到
// agent runner（v4 §11.4 + §11.6.1）。不出现在 YAML 中。
//
// LLM 客户端不在此结构中——Bootstrap 单独构造 llm.Client 并通过 deps 注入。
// 本结构的 Model 字段仅作为运行时元数据使用——主要用途是 HistoryEntry.Model 记录
// （详见 nextUpgrade_v4.md §11.7.3 模型切换基准重置）与运行时日志。
type AgentRuntimeConfig struct {
	InstanceID     string
	Kind           string
	EventType      string
	AllowedTools   []string
	Model          string
	SystemPrompt   string
	TaskMaxRetries int
	// IdleThreshold 对应全局 agent_idle_threshold：agent 连续 N 次空闲轮询后
	// 退出 goroutine；0 = 永不空闲退出（生产推荐，见 Config.AgentIdleThreshold）。
	// AgentKind 没有 per-kind 覆盖字段，各 AgentRuntimeConfig 构造点统一填全局值；
	// scheduler 路径刻意不消费本字段（scheduler 是必须常驻的预制代理，
	// 见 internal/scheduler/scheduler.go 中 a.IdleThreshold 的注释）。
	IdleThreshold int
	// TeamAwareness 是团队能力感知提示词，描述系统中所有 Agent 类型的能力边界。
	// 由 Bootstrap 在启动时构建，注入到每个 Agent 的任务描述前，避免跨 kind 的
	// 能力假设错误（如 verifier 假设 gatherer 有 write_file）。
	TeamAwareness string
}

type Config struct {
	// ============================================================
	// v4 配置块（nextUpgrade_v4.md §11.4）—— 唯一受支持的格式。
	// v3 顶层字段（worker_count / agent_max_loops / llm_base_url / mirrorV4ToV3 等）
	// 已在 2026-04-26 commit 中整体删除——若旧 setting.yaml 仍含这些字段，
	// yaml/json 解析时会被默默忽略（不影响启动），但不再产生任何运行时效果。
	// 用户必须改写为本结构体顶层 yaml 形态：llm: / agents: / infra: / scheduler: / 等。
	// ============================================================
	LLM                       LLMConfig            `yaml:"llm" json:"llm"`
	Scheduler                 SchedulerKind        `yaml:"scheduler" json:"scheduler"`
	Modes                     ModesConfig          `yaml:"modes,omitempty" json:"modes,omitempty"`
	Agents                    []AgentKind          `yaml:"agents" json:"agents"`
	AgentTemplates            AgentTemplatesConfig `yaml:"agent_templates,omitempty" json:"agent_templates,omitempty"`
	Infra                     InfraConfig          `yaml:"infra" json:"infra"`
	UI                        UIConfig             `yaml:"ui" json:"ui"`
	StartupProbe              string               `yaml:"startup_probe,omitempty" json:"startup_probe,omitempty"`
	StartupProbeTimeoutSec    int                  `yaml:"startup_probe_timeout_sec,omitempty" json:"startup_probe_timeout_sec,omitempty"`
	StartupProbeFailureAction string               `yaml:"startup_probe_failure_action,omitempty" json:"startup_probe_failure_action,omitempty"`

	// ============================================================
	// 顶层杂项字段（v4 仍保留在顶层，与 setting.v4.yaml 对应）
	// ============================================================
	HashlineEnabled *bool  `yaml:"hashline_enabled,omitempty" json:"hashline_enabled,omitempty"`
	ProjectRoot     string `yaml:"project_root" json:"project_root"`
	MaxSubtaskDepth int    `yaml:"max_subtask_depth" json:"max_subtask_depth"`
	ShellTimeoutSec int    `yaml:"shell_timeout_sec" json:"shell_timeout_sec"`

	// ProgressNotifyEnabled 控制进度通知功能是否启用。启用后，agent 在完成
	// 文件写入或子任务发布时，通过 mailbox 向相关 Agent 发送轻量级进度消息。
	ProgressNotifyEnabled bool `yaml:"progress_notify_enabled" json:"progress_notify_enabled"`

	// AgentIdleThreshold 是 agent runner 在连续 N 次空闲轮询后退出 goroutine 的阈值。
	// 默认 0 = 永不空闲退出（生产环境推荐）。
	AgentIdleThreshold int `yaml:"agent_idle_threshold,omitempty" json:"agent_idle_threshold,omitempty"`

	SearchAPIProvider string `yaml:"search_api_provider" json:"search_api_provider"`
	SearchAPIURL      string `yaml:"search_api_url" json:"search_api_url"`
	SearchAPIKey      string `yaml:"search_api_key" json:"search_api_key"`

	// Shell 命令拦截配置（追加到默认规则）
	ShellBlacklist []string `yaml:"shell_blacklist" json:"shell_blacklist"`
	ShellGreylist  []string `yaml:"shell_greylist" json:"shell_greylist"`
	// AllowProjectShellRuleRemovals 是受信任主配置中的显式降级开关。
	// false（默认）时 .agentgo/project_rules.yaml 只能追加规则，不能移除系统
	// 默认或主配置追加的黑/灰名单；true 时才恢复旧 remove 语义。
	AllowProjectShellRuleRemovals bool `yaml:"allow_project_shell_rule_removals,omitempty" json:"allow_project_shell_rule_removals,omitempty"`

	// ToolProfiles 命名工具集：profile_name → [tool_name, ...]
	// 由 agents[*].profile 引用。直接列工具走 agents[*].tools 字段。
	ToolProfiles map[string][]string `yaml:"tool_profiles" json:"tool_profiles"`

	// ReactorsFile 是用户 YAML Reactor 配置文件路径（v5 Phase 5）。
	// 空值时跳过加载；非空时由 internal/reactor/userdef.LoadFromFile 解析。
	// 路径解析为相对当前工作目录或绝对路径，prompt 文件必须在 ProjectRoot 内。
	ReactorsFile string `yaml:"reactors_file,omitempty" json:"reactors_file,omitempty"`

	// SessionRetentionDays 是 Session 保留天数。超过此天数的已关闭 Session 将被归档。
	SessionRetentionDays int `yaml:"session_retention_days" json:"session_retention_days"`
	// SessionArchiveMax 是最大归档 Session 数。超过此数量时删除最旧的归档。
	SessionArchiveMax int `yaml:"session_archive_max" json:"session_archive_max"`
	// SessionResumeMaxIdleSec 已废弃（2026-08）：启动永远是全新 Session，
	// 不再自动恢复 active-session，陈旧恢复保护随之移除；进入历史会话
	// （--resume / 运行时切换）时非终态任务一律阻断为 blocked，不自动续跑。
	// 字段保留解析/校验仅为配置兼容，设置它不再产生任何行为。
	SessionResumeMaxIdleSec int `yaml:"session_resume_max_idle_sec" json:"session_resume_max_idle_sec"`
	// SessionSnapshotIntervalSec 是运行期完整快照间隔；0 表示只在切换/关闭时保存。
	SessionSnapshotIntervalSec int `yaml:"session_snapshot_interval_sec" json:"session_snapshot_interval_sec"`
}

// time.Duration is an int64 nanosecond count. Values above this many seconds
// overflow when bootstrap converts the configuration and may make NewTicker
// panic or invert the stale-resume comparison.
const maxSessionDurationSeconds = 9_223_372_036

// ResolveProfile 根据 profile 名称从 ToolProfiles 中查找工具列表。
//   - name 为空 → 返回 nil（意为"允许全部工具"，向后兼容）
//   - name 不存在于 ToolProfiles → 返回 error（配置笔误应立即暴露）
//
// 这是 profile → 工具列表的唯一权威解析入口（D3）：生产路径（runner 构建、
// team awareness、静态 route 注册）全部委托本函数或其纯函数形态
// ResolveToolProfile，不再各自内联 map 查找。
func (c *Config) ResolveProfile(name string) ([]string, error) {
	if name == "" {
		return nil, nil
	}
	if c.ToolProfiles == nil {
		return nil, fmt.Errorf("tool profile %q 未找到：tool_profiles 未定义", name)
	}
	return ResolveToolProfile(c.ToolProfiles, name)
}

// ResolveToolProfile 是 ResolveProfile 的纯函数形态，供只持有 tool_profiles 表
// （而非完整 *Config）的调用方使用，语义与 ResolveProfile 一致：
//   - name 为空 → 返回 nil（"允许全部工具"，向后兼容）
//   - name 不存在于 profiles → 返回 error（配置笔误应立即暴露）
func ResolveToolProfile(profiles map[string][]string, name string) ([]string, error) {
	if name == "" {
		return nil, nil
	}
	tools, ok := profiles[name]
	if !ok {
		return nil, fmt.Errorf("tool profile %q 未找到，可用的 profile: %v", name, profileKeys(profiles))
	}
	return tools, nil
}

// profileKeys 返回 map 的所有 key（用于错误消息）。
func profileKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// DefaultConfig 返回内嵌默认值（仅顶层 + Infra 嵌套块）。v4 启动校验要求
// agents / llm 必须在 yaml 中显式声明——这里不填占位值，避免给空 yaml 制造
// 看似能跑实则不可用的配置。
// ptrTo 返回指向 v 的指针。用于 *bool 等指针字段的默认值构造。
func ptrTo[T any](v T) *T { return &v }

func DefaultConfig() *Config {
	return &Config{
		ProjectRoot:                ".",
		Scheduler:                  SchedulerKind{},
		ShellTimeoutSec:            30,
		MaxSubtaskDepth:            1,
		ProgressNotifyEnabled:      true, // §8.6 进度通知默认启用
		AgentIdleThreshold:         0,
		SearchAPIProvider:          "duckduckgo_html",
		SessionRetentionDays:       30,
		SessionArchiveMax:          50,
		SessionResumeMaxIdleSec:    3600,
		SessionSnapshotIntervalSec: 30,
		AgentTemplates: AgentTemplatesConfig{
			MaxRuntimeAgents: 8,
		},
		Infra: InfraConfig{
			Watchdog: WatchdogConfig{
				IntervalSec:               30,
				ProgressHeartbeatGraceSec: 120,
				PendingAlertGraceSec:      300,
				UnroutableGraceSec:        300,
			},
			MailNotifier: MailNotifierConfig{Enabled: true, IntervalSec: 5},
			Store: StoreConfig{
				EventChannelBuffer: 64,
				FIFOLimit:          100,
				DefaultConcurrency: 2,
				DefaultTimeoutSec:  3600,
			},
			Roster: RosterConfig{WaitTimeoutSec: 30},
		},
		UI: UIConfig{
			Frontends: []string{"tui"},
			Web:       WebUIConfig{Listen: "127.0.0.1:8399", Token: ""},
		},
	}
}

// LoadConfig 加载配置文件。
// explicit 为 true 表示用户显式指定了路径：文件不存在或格式不支持时直接报错。
// explicit 为 false 表示使用默认路径：文件不存在或格式不支持时使用默认配置。
//
// nextUpgrade_v4.md §11.3 / S1：反序列化前对原始 YAML/JSON 文本做一次
// os.ExpandEnv，支持 ${ENV_VAR} 替换（Twelve-factor app 标准做法，避免把 API key
// 提交到版本库）。未引用 env var 的字段不受影响——os.ExpandEnv 仅替换 $name 与 ${name}
// 形式的 token，其他字面值原样保留。
// decodeIfUTF16 检测 UTF-16 BOM（FF FE / FE FF）并把内容转码为 UTF-8。
// 无 BOM 时原样返回（按 UTF-8 处理）。奇数长度截掉尾部残缺字节，容忍手滑编辑。
func decodeIfUTF16(data []byte) []byte {
	if len(data) < 2 {
		return data
	}
	var bigEndian bool
	switch {
	case data[0] == 0xFF && data[1] == 0xFE:
		bigEndian = false
	case data[0] == 0xFE && data[1] == 0xFF:
		bigEndian = true
	default:
		return data
	}
	body := data[2:]
	if len(body)%2 == 1 {
		body = body[:len(body)-1]
	}
	u16 := make([]uint16, len(body)/2)
	for i := range u16 {
		if bigEndian {
			u16[i] = uint16(body[2*i])<<8 | uint16(body[2*i+1])
		} else {
			u16[i] = uint16(body[2*i]) | uint16(body[2*i+1])<<8
		}
	}
	return []byte(string(utf16.Decode(u16)))
}

func LoadConfig(path string, explicit bool) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if explicit {
				return nil, fmt.Errorf("配置文件不存在: %s", path)
			}
			fmt.Fprintf(os.Stderr, "[警告] 默认配置文件 %s 不存在，使用内置默认配置\n", path)
			return cfg, nil
		}
		return nil, err
	}

	// UTF-16 转码必须先于环境变量展开：Windows 记事本等编辑器默认存
	// UTF-16LE，交错 NUL 字节会让 os.ExpandEnv 匹配不到 ${VAR}，配置里的
	// 环境变量引用被静默保留为字面量（E1）。
	data = decodeIfUTF16(data)

	// 环境变量展开（v4 §11.3 末尾"环境变量替换"段）
	expanded := []byte(os.ExpandEnv(string(data)))

	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".yaml", ".yml":
		if err := yaml.Unmarshal(expanded, cfg); err != nil {
			return nil, err
		}
	case ".json":
		if err := json.Unmarshal(expanded, cfg); err != nil {
			return nil, err
		}
	default:
		if explicit {
			return nil, fmt.Errorf("不支持的配置文件格式: %s（仅支持 .yaml/.yml/.json）", ext)
		}
		return cfg, nil
	}

	// 路径不做 filepath.FromSlash 自动转换——会污染 Validate 的反斜杠检查
	// （在 Windows 上 FromSlash("prompts/worker.md") → "prompts\\worker.md"，
	// 再被 Validate 拒绝）。Windows 的 os.ReadFile 接受 forward slash 路径，无需
	// normalize。Validate 看到的就是用户写在 YAML 里的原始字符串。

	// §7：hashline_enabled 未显式设置时默认 true
	if cfg.HashlineEnabled == nil {
		cfg.HashlineEnabled = ptrTo(true)
	}

	return cfg, nil
}

// Validate 在 Bootstrap 主流程中执行启动配置校验，任一检查失败即返回 non-nil
// error 终止启动。实际执行的检查（按代码顺序；括号内为 nextUpgrade_v4.md
// §11.5.3 的历史规则编号，编号不连续——规则 1-2 不存在，规则 10 拆在两处）：
//  1. 路径风格红线：project_root、agents[*].system_prompt_file 不得含反斜杠；
//     agent_templates 的 user_dirs/project_dirs 同样不得含反斜杠（规则 9 及附加项）；
//  2. 模型解析：scheduler.model 缺省回落 llm.default_model，两者皆空即报错；
//     静态 agents 每项须有自有 model 或全局 llm.default_model；scheduler.model
//     显式出现（非空串）时不得为纯空白；Scheduler 两项可选行为预算不得为负
//     （规则 10 的两半及后续扩展）；
//  3. agent_templates.max_runtime_agents 须在 0..32 之间（0 或省略 = 默认 8）；
//  4. agents[*].kind 非空且在列表内唯一（规则 3 + 12）；
//  5. agents[*].replicas >= 1（规则 4）；
//  6. profile 与 tools 互斥且恰一（规则 5）；profile 引用必须存在于
//     tool_profiles（规则 6；工具名合法性由 bootstrap 的 tools.ValidateToolNames
//     单独校验，不在此处）；
//  7. agents[*].system_prompt_file 必填且存在可读（规则 8）；
//  8. 行为参数全部 > 0：task_max_retries /
//     enforce_compact_token_threshold（规则 11）；
//     agent_max_loops 与 context_limit 已于 V6 移除，显式设置（非零）
//     返回迁移诊断错误；
//  9. startup_probe 取值合法（tcp/off）、失败动作合法（warn/exit）、
//     startup_probe_timeout_sec 非负（validateStartupProbe，后加的独立检查）；
//  10. ui 块：frontends ∈ {tui, web}（去重）；web.listen 为合法 host:port；
//     非 loopback 监听必须设置 web.token（validateUI）；
//  11. modes 块：gate 轴已于 V6 C6c 整体移除（显式设置任何非空值报迁移
//     诊断）、exec ∈ {normal, strict, readonly, yolo}、topo ∈ {team, solo}；
//     字段为空 = 该轴取默认值（validateModes）。
//
// agents 可以为空，此时系统以 Scheduler-only 模式启动，并可在运行期从
// AgentTemplate provision Team。只要 agents 非空，原有静态 kind 的全部严格
// 校验仍然执行，非法配置不会静默降级。
func (c *Config) Validate() error {
	// llm.provider 已于 V6 移除：读到旧字段必须给出明确的迁移诊断，
	// 不允许静默忽略或回退（V6 升级决议，见 docs/nextUpgrade-V6.md）。
	if c.LLM.Provider != "" {
		return fmt.Errorf("llm.provider=%q 已于 V6 移除：AgentGo 只实现 OpenAI-compatible Chat Completions 请求路径，"+
			"不再区分 provider 适配分支；请从配置中删除 llm.provider 字段", c.LLM.Provider)
	}

	if c.LLM.ReasoningEffort != "" && !isOpenAIReasoningEffort(c.LLM.ReasoningEffort) {
		return fmt.Errorf("llm.reasoning_effort=%q 无效；允许值: %s",
			c.LLM.ReasoningEffort, strings.Join(OpenAIReasoningEfforts, ", "))
	}

	// 规则 9：所有 v4 路径字段不含反斜杠（路径风格红线）。
	// 覆盖范围：ProjectRoot + agents[*].system_prompt_file。
	// LLM.BaseURL 是 URL 不是文件路径，不纳入本条。
	if strings.Contains(c.ProjectRoot, "\\") {
		return fmt.Errorf("project_root 包含反斜杠（v4 仅允许 forward slash）: %q", c.ProjectRoot)
	}
	for i, k := range c.Agents {
		if strings.Contains(k.SystemPromptFile, "\\") {
			return fmt.Errorf("agents[%d].system_prompt_file 包含反斜杠（v4 仅允许 forward slash）: %q",
				i, k.SystemPromptFile)
		}
	}

	// 规则 10：scheduler 块约束。
	// 整块缺失 / 为空等价于 scheduler.model = llm.default_model，不报错。
	// 出现 model 字段时必须为非空字符串。

	// Scheduler 和内置 AgentTemplate 始终存在，因此即使配置了模型完整的
	// 静态 Agent，仍必须有一个全局或 Scheduler 模型。BaseURL/APIKey 保留
	// provider/SDK 的既有默认与环境变量语义，不在配置层强制。
	model := strings.TrimSpace(c.Scheduler.Model)
	if model == "" {
		model = strings.TrimSpace(c.LLM.DefaultModel)
	}
	if model == "" {
		return fmt.Errorf("Scheduler 配置缺少模型：请设置 llm.default_model 或 scheduler.model")
	}
	for i, agentKind := range c.Agents {
		if strings.TrimSpace(agentKind.Model) == "" && strings.TrimSpace(c.LLM.DefaultModel) == "" {
			return fmt.Errorf("agents[%d] (kind=%q) 缺少模型：请设置 agents[%d].model 或 llm.default_model；scheduler.model 只供 Scheduler 和默认 AgentTemplate 使用", i, agentKind.Kind, i)
		}
	}

	// 零值（直接构造或 YAML 显式填 0）由 TeamManager 解析为默认 8；
	// 负数和超过硬上限的值始终拒绝。
	if c.AgentTemplates.MaxRuntimeAgents < 0 || c.AgentTemplates.MaxRuntimeAgents > 32 {
		return fmt.Errorf("agent_templates.max_runtime_agents=%d 必须在 0..32 之间（0 或省略时默认 8）", c.AgentTemplates.MaxRuntimeAgents)
	}
	for i, dir := range append(append([]string(nil), c.AgentTemplates.UserDirs...), c.AgentTemplates.ProjectDirs...) {
		if strings.Contains(dir, "\\") {
			return fmt.Errorf("agent_templates 目录[%d] 包含反斜杠（仅允许 forward slash）: %q", i, dir)
		}
	}

	// 规则 3 + 12：每个 AgentKind.Kind 在列表内唯一且非空字符串
	seenKinds := make(map[string]bool, len(c.Agents))
	for i, k := range c.Agents {
		if k.Kind == "" {
			return fmt.Errorf("agents[%d].kind 不能为空字符串（v4 §11.5.3 规则 12）", i)
		}
		if seenKinds[k.Kind] {
			return fmt.Errorf("agents[%d].kind 重复: %q（每个 kind 在列表内必须唯一）", i, k.Kind)
		}
		seenKinds[k.Kind] = true
	}

	// 规则 4：agents[*].replicas >= 1
	for i, k := range c.Agents {
		if k.Replicas < 1 {
			return fmt.Errorf("agents[%d] (kind=%q).replicas=%d 必须 >= 1", i, k.Kind, k.Replicas)
		}
	}

	// 规则 5：profile 与 tools 互斥（恰一）
	for i, k := range c.Agents {
		hasProfile := k.Profile != ""
		hasTools := len(k.Tools) > 0
		if hasProfile && hasTools {
			return fmt.Errorf("agents[%d] (kind=%q) 同时声明了 profile=%q 和 tools=%v——必须二选一",
				i, k.Kind, k.Profile, k.Tools)
		}
		if !hasProfile && !hasTools {
			return fmt.Errorf("agents[%d] (kind=%q) 必须声明 profile 或 tools 之一", i, k.Kind)
		}
	}

	// 规则 6：profile 引用名称必须存在于 tool_profiles
	// 规则 7 工具名校验由 bootstrap 阶段调用 tools.ValidateToolNames 单独承接
	for i, k := range c.Agents {
		if k.Profile == "" {
			continue
		}
		resolved, ok := c.ToolProfiles[k.Profile]
		if !ok {
			return fmt.Errorf("agents[%d] (kind=%q) 引用了不存在的 profile: %q", i, k.Kind, k.Profile)
		}
		if len(resolved) == 0 {
			return fmt.Errorf("agents[%d] (kind=%q) 引用了空 profile %q：空工具白名单不允许；请至少声明任务所需工具", i, k.Kind, k.Profile)
		}
	}

	// 规则 8：每个 system_prompt_file 存在且可读
	for i, k := range c.Agents {
		if k.SystemPromptFile == "" {
			return fmt.Errorf("agents[%d] (kind=%q) 缺少 system_prompt_file（v4 必填）", i, k.Kind)
		}
		// 解析为相对/绝对路径——配置层允许绝对路径（用户启动权限域，详见 v4 §11.5.2）
		if _, err := os.Stat(k.SystemPromptFile); err != nil {
			return fmt.Errorf("agents[%d] (kind=%q) system_prompt_file=%q 不可读: %w",
				i, k.Kind, k.SystemPromptFile, err)
		}
	}

	// 规则 11：行为参数显式声明且 > 0
	for i, k := range c.Agents {
		// agent_max_loops 已于 V6 移除：读到显式设置的旧字段必须给出明确的
		// 迁移诊断，不允许静默忽略（与 llm.provider 同款模式）。零值视为未设置。
		if k.AgentMaxLoops != 0 {
			return fmt.Errorf("agents[%d] (kind=%q).agent_max_loops=%d 已于 V6 移除："+
				"Loop 不再因到达固定轮数而终止，由结构化终态、取消、deadline、预算与 emergency fuse 共同约束；"+
				"请从配置中删除该字段", i, k.Kind, k.AgentMaxLoops)
		}
		// context_limit 已于 V6 移除：固定上下文硬限截断层（含 history_truncated
		// 事件）一并删除，上下文适配由 L2 压缩与 L3 溢出重试承担。零值视为未设置。
		if k.ContextLimit != 0 {
			return fmt.Errorf("agents[%d] (kind=%q).context_limit=%d 已于 V6 移除："+
				"固定上下文硬限已删除，上下文适配由版本化 Context policy、ContentRef 与 Snapshot-pressure projection 承担；"+
				"请从配置中删除该字段", i, k.Kind, k.ContextLimit)
		}
		if k.TaskMaxRetries <= 0 {
			return fmt.Errorf("agents[%d] (kind=%q).task_max_retries 必须 > 0", i, k.Kind)
		}
		if k.EnforceCompactTokenThreshold != 0 {
			return fmt.Errorf("agents[%d] (kind=%q).enforce_compact_token_threshold=%d 已移除："+
				"累计完整 Prompt Token 会重复计算静态 system/tool schema 并造成频繁有损压缩；"+
				"Context v3 现按当前 Snapshot section 压力从 Raw History 派生 replay 视图，请删除该字段",
				i, k.Kind, k.EnforceCompactTokenThreshold)
		}
	}

	// Scheduler 行为预算是可选覆盖：0 表示使用内置默认，负数没有合理语义，
	// 启动时明确拒绝。agent_max_loops 与 context_limit 已于 V6 移除，显式设置即迁移诊断。
	if c.Scheduler.AgentMaxLoops != 0 {
		return fmt.Errorf("scheduler.agent_max_loops=%d 已于 V6 移除："+
			"Loop 不再因到达固定轮数而终止，由结构化终态、取消、deadline、预算与 emergency fuse 共同约束；"+
			"请从配置中删除该字段", c.Scheduler.AgentMaxLoops)
	}
	if c.Scheduler.ContextLimit != 0 {
		return fmt.Errorf("scheduler.context_limit=%d 已于 V6 移除："+
			"固定上下文硬限已删除，上下文适配由版本化 Context policy、ContentRef 与 Snapshot-pressure projection 承担；"+
			"请从配置中删除该字段", c.Scheduler.ContextLimit)
	}
	if c.Scheduler.EnforceCompactTokenThreshold != 0 {
		return fmt.Errorf("scheduler.enforce_compact_token_threshold=%d 已移除：Scheduler phase Prompt 不再按累计完整 Prompt spend 压缩 Raw History；请删除该字段",
			c.Scheduler.EnforceCompactTokenThreshold)
	}

	// 规则 10：scheduler.model 出现时必须为非空字符串。空整块 / 空 model 字段则缺省回落 LLM.DefaultModel
	if c.Scheduler.Model != "" && strings.TrimSpace(c.Scheduler.Model) == "" {
		return fmt.Errorf("scheduler.model 仅含空白字符——若要使用默认模型，请删除该字段")
	}
	if c.SessionResumeMaxIdleSec < 0 {
		return fmt.Errorf("session_resume_max_idle_sec=%d 不能为负", c.SessionResumeMaxIdleSec)
	}
	if int64(c.SessionResumeMaxIdleSec) > maxSessionDurationSeconds {
		return fmt.Errorf("session_resume_max_idle_sec=%d 超出 time.Duration 可表示范围", c.SessionResumeMaxIdleSec)
	}
	if c.SessionSnapshotIntervalSec < 0 {
		return fmt.Errorf("session_snapshot_interval_sec=%d 不能为负", c.SessionSnapshotIntervalSec)
	}
	if int64(c.SessionSnapshotIntervalSec) > maxSessionDurationSeconds {
		return fmt.Errorf("session_snapshot_interval_sec=%d 超出 time.Duration 可表示范围", c.SessionSnapshotIntervalSec)
	}
	if c.Infra.Watchdog.ProgressHeartbeatGraceSec < 0 {
		return fmt.Errorf("infra.watchdog.progress_heartbeat_grace_sec=%d 不能为负", c.Infra.Watchdog.ProgressHeartbeatGraceSec)
	}
	if int64(c.Infra.Watchdog.ProgressHeartbeatGraceSec) > maxSessionDurationSeconds {
		return fmt.Errorf("infra.watchdog.progress_heartbeat_grace_sec=%d 超出 time.Duration 可表示范围", c.Infra.Watchdog.ProgressHeartbeatGraceSec)
	}
	if c.Infra.Store.DefaultTimeoutSec < 0 {
		return fmt.Errorf("infra.store.default_timeout_sec=%d 不能为负（该 legacy 键只填充 ExpectedDuration）", c.Infra.Store.DefaultTimeoutSec)
	}
	if int64(c.Infra.Store.DefaultTimeoutSec) > maxSessionDurationSeconds {
		return fmt.Errorf("infra.store.default_timeout_sec=%d 超出 time.Duration 可表示范围", c.Infra.Store.DefaultTimeoutSec)
	}

	if err := c.validateUI(); err != nil {
		return err
	}

	if err := c.validateModes(); err != nil {
		return err
	}

	return c.validateStartupProbe()
}

// validateModes 校验 modes: 块两轴取值。字段为空 = 该轴取默认值，合法；
// 非空值必须能被 modes.ParseXxx 解析（容错大小写），否则启动报错。
// gate 轴已于 V6 C6c 整体移除：显式设置任何非空值一律报迁移诊断。
func (c *Config) validateModes() error {
	if v := c.Modes.Gate; v != "" {
		return fmt.Errorf("modes.gate=%q 非法: modes.gate 轴已于 V6 整体移除（执行前审阅改由 Graph approval 节点承担），请从配置中删除 gate 键", v)
	}
	if v := c.Modes.Exec; v != "" {
		if _, err := modes.ParseExecMode(v); err != nil {
			return fmt.Errorf("modes.exec=%q 非法: %w", v, err)
		}
	}
	if v := c.Modes.Topo; v != "" {
		if _, err := modes.ParseTopoMode(v); err != nil {
			return fmt.Errorf("modes.topo=%q 非法: %w", v, err)
		}
	}
	return nil
}

// ResolveModes 把 modes: 块解析为两轴初值；空字段回落默认值（normal / team）。
// 非法值同样回落默认——Validate 已在启动时先行拒绝非法值，此路径仅为防御。
func (c *Config) ResolveModes() (modes.ExecMode, modes.TopoMode) {
	exec := modes.ExecNormal
	if e, err := modes.ParseExecMode(c.Modes.Exec); err == nil {
		exec = e
	}
	topo := modes.TopoTeam
	if t, err := modes.ParseTopoMode(c.Modes.Topo); err == nil {
		topo = t
	}
	return exec, topo
}

// validateUI 校验 ui 块：
//  1. frontends 取值 ∈ {tui, web}，去重（保序，原地归一化）；空列表回落默认 [tui]；
//  2. web.listen 必须解析为 host:port，端口为 1..65535 的数字；
//  3. web.listen 绑定非 loopback 地址（0.0.0.0 / :: / 公网 IP 等）时必须设置
//     web.token——本系统具备 shell 执行能力，其管理面（含 POST 控制端点）
//     绝不能无鉴权暴露到局域网/公网。
func (c *Config) validateUI() error {
	if len(c.UI.Frontends) == 0 {
		c.UI.Frontends = []string{"tui"}
	}
	seen := make(map[string]bool, len(c.UI.Frontends))
	deduped := c.UI.Frontends[:0]
	for _, f := range c.UI.Frontends {
		if f != "tui" && f != "web" {
			return fmt.Errorf("ui.frontends 含非法取值 %q（仅允许 \"tui\" / \"web\"）", f)
		}
		if !seen[f] {
			seen[f] = true
			deduped = append(deduped, f)
		}
	}
	c.UI.Frontends = deduped

	if !c.UI.HasFrontend("web") {
		return nil
	}
	listen := strings.TrimSpace(c.UI.Web.Listen)
	if listen == "" {
		return fmt.Errorf("ui.web.listen 不能为空（启用 web 前端时必须显式给出 host:port）")
	}
	host, portStr, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("ui.web.listen=%q 不是合法的 host:port 地址: %w", c.UI.Web.Listen, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("ui.web.listen=%q 端口 %q 无效（须为 1..65535 的数字）", c.UI.Web.Listen, portStr)
	}
	if c.UI.Web.Token != "" {
		return nil
	}
	if isLoopbackHost(host) {
		return nil
	}
	return fmt.Errorf("ui.web.listen=%q 绑定了非 loopback 地址但未设置 ui.web.token："+
		"本系统具备 shell 执行能力，其 Web 管理面（含 POST 控制端点）"+
		"无鉴权暴露到局域网/公网等同于把命令执行入口开放给同网段任何人——"+
		"请设置 ui.web.token，或把 listen 改回 127.0.0.1 / ::1", c.UI.Web.Listen)
}

// isLoopbackHost 报告 host 是否仅指向本机回环：空串（":port" 表示全部网卡）
// 不算 loopback；"localhost" 与 127.0.0.0/8、::1 算 loopback。
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validateStartupProbe 校验 startup_probe / 失败动作字段取值合法。
// 字段缺失（空串）等价于默认值，不报错。
func (c *Config) validateStartupProbe() error {
	if c.StartupProbe != "" && c.StartupProbe != "tcp" && c.StartupProbe != "tool" && c.StartupProbe != "off" {
		return fmt.Errorf("startup_probe=%q 取值无效（仅允许 \"tool\" / \"tcp\" / \"off\"）", c.StartupProbe)
	}
	if c.StartupProbeFailureAction != "" &&
		c.StartupProbeFailureAction != "warn" &&
		c.StartupProbeFailureAction != "exit" {
		return fmt.Errorf("startup_probe_failure_action=%q 取值无效（仅允许 \"warn\" / \"exit\"）",
			c.StartupProbeFailureAction)
	}
	if c.StartupProbeTimeoutSec < 0 {
		return fmt.Errorf("startup_probe_timeout_sec=%d 不能为负", c.StartupProbeTimeoutSec)
	}
	return nil
}
