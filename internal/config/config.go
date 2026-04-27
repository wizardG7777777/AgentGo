package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
// Provider 是 v4 spec 之外的 AgentGo 现存能力（区分 openai / deepseek-v4 / deepseek-r1
// 等非标 endpoint），保留以兼容现有 internal/llm/provider 注册表。
type LLMConfig struct {
	BaseURL      string `yaml:"base_url" json:"base_url"`
	APIKey       string `yaml:"api_key" json:"api_key"`
	DefaultModel string `yaml:"default_model" json:"default_model"`
	TimeoutSec   int    `yaml:"timeout_sec" json:"timeout_sec"`
	Provider     string `yaml:"provider,omitempty" json:"provider,omitempty"`
}

// AgentKind 一个 agent 种类的声明（v4 §11.4）。
// 同 kind 的多个实例（replicas 个）完全同质——同工具集、同提示词、同模型。
// 异质化通过声明多个 kind 实现。
type AgentKind struct {
	Kind                         string   `yaml:"kind" json:"kind"`
	Replicas                     int      `yaml:"replicas" json:"replicas"`
	EventType                    string   `yaml:"event_type,omitempty" json:"event_type,omitempty"`
	Profile                      string   `yaml:"profile,omitempty" json:"profile,omitempty"`
	Tools                        []string `yaml:"tools,omitempty" json:"tools,omitempty"`
	Model                        string   `yaml:"model,omitempty" json:"model,omitempty"`
	SystemPromptFile             string   `yaml:"system_prompt_file" json:"system_prompt_file"`
	AgentMaxLoops                int      `yaml:"agent_max_loops" json:"agent_max_loops"`
	TaskMaxRetries               int      `yaml:"task_max_retries" json:"task_max_retries"`
	EnforceCompactTokenThreshold int      `yaml:"enforce_compact_token_threshold" json:"enforce_compact_token_threshold"`
	ContextLimit                 int      `yaml:"context_limit" json:"context_limit"`
}

// SchedulerKind scheduler 独立块（v4 §11.5.5）。
// 配置面刻意收窄——仅 model 字段允许外部覆盖；工具集 / 系统提示词 / 行为参数 /
// replicas 全部硬编码在 internal/scheduler 包。
type SchedulerKind struct {
	Model string `yaml:"model,omitempty" json:"model,omitempty"`
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
}

type MailNotifierConfig struct {
	Enabled     bool `yaml:"enabled" json:"enabled"`
	IntervalSec int  `yaml:"interval_sec" json:"interval_sec"`
}

type StoreConfig struct {
	EventChannelBuffer int `yaml:"event_channel_buffer" json:"event_channel_buffer"`
	FIFOLimit          int `yaml:"fifo_limit" json:"fifo_limit"`
	DefaultConcurrency int `yaml:"default_concurrency" json:"default_concurrency"`
	// DefaultTimeoutSec 是任务级默认超时（watchdog 据此判定 unclaimed 任务何时算超时）。
	// v3 旧名 cfg.DefaultTimeoutSec，已下沉到 Infra.Store 块下，与 store 容量参数同居。
	DefaultTimeoutSec int `yaml:"default_timeout_sec" json:"default_timeout_sec"`
}

type RosterConfig struct {
	WaitTimeoutSec int `yaml:"wait_timeout_sec" json:"wait_timeout_sec"`
}

// AgentRuntimeConfig 内部使用，由 Bootstrap 从 AgentKind + LLMConfig 合成后注入到
// agent runner（v4 §11.4 + §11.6.1）。不出现在 YAML 中。
//
// LLM 客户端不在此结构中——Bootstrap 单独构造 llm.Client 并通过 deps 注入。
// 本结构的 Model 字段仅作为运行时元数据使用——主要用途是 HistoryEntry.Model 记录
// （详见 nextUpgrade_v4.md §11.7.3 模型切换基准重置）与运行时日志。
type AgentRuntimeConfig struct {
	InstanceID                   string
	Kind                         string
	EventType                    string
	AllowedTools                 []string
	Model                        string
	SystemPrompt                 string
	AgentMaxLoops                int
	TaskMaxRetries               int
	EnforceCompactTokenThreshold int
	ContextLimit                 int
}

type Config struct {
	// ============================================================
	// v4 配置块（nextUpgrade_v4.md §11.4）—— 唯一受支持的格式。
	// v3 顶层字段（worker_count / agent_max_loops / llm_base_url / mirrorV4ToV3 等）
	// 已在 2026-04-26 commit 中整体删除——若旧 setting.yaml 仍含这些字段，
	// yaml/json 解析时会被默默忽略（不影响启动），但不再产生任何运行时效果。
	// 用户必须改写为本结构体顶层 yaml 形态：llm: / agents: / infra: / scheduler: / 等。
	// ============================================================
	LLM                       LLMConfig     `yaml:"llm" json:"llm"`
	Scheduler                 SchedulerKind `yaml:"scheduler" json:"scheduler"`
	Agents                    []AgentKind   `yaml:"agents" json:"agents"`
	Infra                     InfraConfig   `yaml:"infra" json:"infra"`
	StartupProbe              string        `yaml:"startup_probe,omitempty" json:"startup_probe,omitempty"`
	StartupProbeTimeoutSec    int           `yaml:"startup_probe_timeout_sec,omitempty" json:"startup_probe_timeout_sec,omitempty"`
	StartupProbeFailureAction string        `yaml:"startup_probe_failure_action,omitempty" json:"startup_probe_failure_action,omitempty"`

	// ============================================================
	// 顶层杂项字段（v4 仍保留在顶层，与 setting.v4.yaml 对应）
	// ============================================================
	HashlineEnabled       *bool `yaml:"hashline_enabled,omitempty" json:"hashline_enabled,omitempty"`
	ProjectRoot           string `yaml:"project_root" json:"project_root"`
	MaxSubtaskDepth       int    `yaml:"max_subtask_depth" json:"max_subtask_depth"`
	ShellTimeoutSec       int    `yaml:"shell_timeout_sec" json:"shell_timeout_sec"`

	// TransferNoteMaxTokens 是 TransferNote 单条最大 token 预算。agent 在生成
	// L1/L3 交接备忘时按此预算截断文本长度——按 1 token ≈ 2 runes 估算。
	// 默认 3000 对应 ~6000 字符中文或 ~12000 字符英文。
	TransferNoteMaxTokens int `yaml:"transfer_note_max_tokens" json:"transfer_note_max_tokens"`

	// ProgressNotifyEnabled 控制进度通知功能是否启用。启用后，agent 在完成
	// 文件写入、子任务发布或任务过半时，通过 mailbox 向相关 Agent 发送轻量级进度消息。
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

	// ToolProfiles 命名工具集：profile_name → [tool_name, ...]
	// 由 agents[*].profile 引用。直接列工具走 agents[*].tools 字段。
	ToolProfiles map[string][]string `yaml:"tool_profiles" json:"tool_profiles"`

	// SessionRetentionDays 是 Session 保留天数。超过此天数的已关闭 Session 将被归档。
	SessionRetentionDays int `yaml:"session_retention_days" json:"session_retention_days"`
	// SessionArchiveMax 是最大归档 Session 数。超过此数量时删除最旧的归档。
	SessionArchiveMax int `yaml:"session_archive_max" json:"session_archive_max"`
}

// ResolveProfile 根据 profile 名称从 ToolProfiles 中查找工具列表。
//   - name 为空 → 返回 nil（意为"允许全部工具"，向后兼容）
//   - name 不存在于 ToolProfiles → 返回 error（配置笔误应立即暴露）
func (c *Config) ResolveProfile(name string) ([]string, error) {
	if name == "" {
		return nil, nil
	}
	if c.ToolProfiles == nil {
		return nil, fmt.Errorf("tool profile %q 未找到：tool_profiles 未定义", name)
	}
	tools, ok := c.ToolProfiles[name]
	if !ok {
		return nil, fmt.Errorf("tool profile %q 未找到，可用的 profile: %v", name, profileKeys(c.ToolProfiles))
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
		ShellTimeoutSec:       30,
		MaxSubtaskDepth:       1,
		TransferNoteMaxTokens: 3000, // Sprint 3 #5 TransferNote 默认预算
		ProgressNotifyEnabled: true, // §8.6 进度通知默认启用
		AgentIdleThreshold:    0,
		SearchAPIProvider:     "duckduckgo_html",
		SessionRetentionDays:  30,
		SessionArchiveMax:     50,
		Infra: InfraConfig{
			Watchdog:     WatchdogConfig{IntervalSec: 30},
			MailNotifier: MailNotifierConfig{Enabled: true, IntervalSec: 5},
			Store: StoreConfig{
				EventChannelBuffer: 64,
				FIFOLimit:          100,
				DefaultConcurrency: 2,
				DefaultTimeoutSec:  300,
			},
			Roster: RosterConfig{WaitTimeoutSec: 30},
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

// Validate 在 Bootstrap 主流程中调用，对应 nextUpgrade_v4.md §11.5.3 全部启动校验
// 规则。任一规则失败即返回 non-nil error 终止启动。
//
// v4 唯一格式：cfg.Agents 必须非空。若 yaml 缺 agents: 列表，启动直接报错——
// 这是 v3 兼容层 2026-04-26 删除后的硬约束（详见 §11 设计原则第 10 条）。
func (c *Config) Validate() error {
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

	// 规则 2：agents 列表必须非空（v4 §11.5.3 第 2 条）。
	// v3 兼容层 2026-04-26 删除——空 agents 列表直接报错，没有 fallback。
	if len(c.Agents) == 0 {
		return fmt.Errorf("agents 列表为空：v4 配置必须声明至少一个 kind（详见 setting.v4.yaml 模板 / nextUpgrade_v4.md §11.3）")
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
		if _, ok := c.ToolProfiles[k.Profile]; !ok {
			return fmt.Errorf("agents[%d] (kind=%q) 引用了不存在的 profile: %q", i, k.Kind, k.Profile)
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
		if k.AgentMaxLoops <= 0 {
			return fmt.Errorf("agents[%d] (kind=%q).agent_max_loops 必须 > 0", i, k.Kind)
		}
		if k.TaskMaxRetries <= 0 {
			return fmt.Errorf("agents[%d] (kind=%q).task_max_retries 必须 > 0", i, k.Kind)
		}
		if k.EnforceCompactTokenThreshold <= 0 {
			return fmt.Errorf("agents[%d] (kind=%q).enforce_compact_token_threshold 必须 > 0", i, k.Kind)
		}
		if k.ContextLimit <= 0 {
			return fmt.Errorf("agents[%d] (kind=%q).context_limit 必须 > 0", i, k.Kind)
		}
	}

	// 规则 10：scheduler.model 出现时必须为非空字符串。空整块 / 空 model 字段则缺省回落 LLM.DefaultModel
	if c.Scheduler.Model != "" && strings.TrimSpace(c.Scheduler.Model) == "" {
		return fmt.Errorf("scheduler.model 仅含空白字符——若要使用默认模型，请删除该字段")
	}

	return c.validateStartupProbe()
}

// validateStartupProbe 校验 startup_probe / 失败动作字段取值合法。
// 字段缺失（空串）等价于默认值，不报错。
func (c *Config) validateStartupProbe() error {
	if c.StartupProbe != "" && c.StartupProbe != "tcp" && c.StartupProbe != "off" {
		return fmt.Errorf("startup_probe=%q 取值无效（仅允许 \"tcp\" / \"off\"）", c.StartupProbe)
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
