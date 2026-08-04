package eval

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// 评测套件（suite.yaml）的声明式定义：黄金任务 + fixtures + 确定性 judges。
// 套件文件与其引用的 prompt/fixtures 全部在 eval/ 目录内，不入库。

// Suite 一份评测套件。
type Suite struct {
	Name     string       `yaml:"name"`
	Defaults SuiteDefault `yaml:"defaults"`
	Tasks    []TaskDef    `yaml:"tasks"`

	// Dir 是套件文件所在目录（非 YAML 字段），prompt_file 相对它解析。
	Dir string `yaml:"-"`
}

// SuiteDefault 任务级默认值。
type SuiteDefault struct {
	TimeoutSec int `yaml:"timeout_sec"`
}

// TaskDef 一个黄金任务的完整声明。
type TaskDef struct {
	Name       string      `yaml:"name"`
	Smoke      bool        `yaml:"smoke"`       // true = 进 --smoke 层（便宜、快）
	Skip       bool        `yaml:"skip"`        // true = 本轮跳过（结果状态 skipped，不算 pass）
	Prompt     string      `yaml:"prompt"`      // 内联提示词（与 prompt_file 二选一）
	PromptFile string      `yaml:"prompt_file"` // 相对套件目录的提示词文件
	Fixtures   []Fixture   `yaml:"fixtures"`    // 运行前铺进临时 project_root 的文件
	Judges     []JudgeSpec `yaml:"judges"`      // 确定性判据
	TimeoutSec int         `yaml:"timeout_sec"` // 0 = 用 suite defaults
	// LLMScript 是 offline 子命令专用的 fake-LLM 脚本路径（相对套件目录）；
	// live 套件不设置。offline case 缺脚本 = unqualified（资格不全，不得算 pass）。
	LLMScript string `yaml:"llm_script"`
}

// Fixture 运行前写入临时 project_root 的一个文件。
type Fixture struct {
	Path    string `yaml:"path"`    // 相对 project_root，禁止 .. 与绝对路径
	Content string `yaml:"content"` // 原样字节（建议 LF 行尾）
}

// JudgeSpec 一条确定性判据。字段按 type 取用，其余忽略。
type JudgeSpec struct {
	Type    string `yaml:"type"`
	Path    string `yaml:"path,omitempty"`    // file_* 判据：相对 project_root
	Pattern string `yaml:"pattern,omitempty"` // file_contains：子串或正则
	Regex   bool   `yaml:"regex,omitempty"`
	SHA256  string `yaml:"sha256,omitempty"` // file_hash：期望 hex（normalize 后口径）
	// TrimTrailingBlank 仅 file_hash 使用：比对前把文件尾部空白行归一到
	// 单个换行（"…line10\n\n" → "…line10\n"）。LLM 写文件尾多一个换行是
	// 已观察到的无害变体（smoke 通过、全量首跑即翻车），精确 hash 会在
	// 这种噪声上慢性误报；行级内容契约仍由 hash 主体保证。
	TrimTrailingBlank bool     `yaml:"trim_trailing_blank,omitempty"`
	Kind              string   `yaml:"kind,omitempty"`   // event_count / event_absent / event_field：trace 事件 kind
	Kinds             []string `yaml:"kinds,omitempty"`  // event_order：按序应出现的事件 kind 清单
	Field             string   `yaml:"field,omitempty"`  // event_field：事件 JSON 点路径（如 graph_id / lease.digest）
	Equals            string   `yaml:"equals,omitempty"` // event_field：期望值（字符串化比对）
	NonEmpty          bool     `yaml:"non_empty,omitempty"` // event_field：只要求字段非空
	Metric            string   `yaml:"metric,omitempty"` // metric_bounds：指标名
	Min               *float64 `yaml:"min,omitempty"`
	Max               *float64 `yaml:"max,omitempty"`
}

// knownJudgeTypes 是支持的判据类型全集（全部确定性，无 LLM 裁判）。
var knownJudgeTypes = map[string]bool{
	"task_completed": true, // 运行正常收敛（非超时/启动失败）
	"file_exists":    true, // 产物文件存在
	"file_absent":    true, // 文件不存在（禁止行为防线，如 finalizing fence 拦截的写）
	"file_contains":  true, // 产物含子串/正则
	"file_hash":      true, // 产物 SHA256 精确匹配
	"file_min_bytes": true, // 产物字节数下界（长文截断防线）
	"event_count":    true, // 某 kind trace 事件次数 ∈ [min, max]
	"event_absent":   true, // 某 kind trace 事件为零（证据不完整时结论 trace_incomplete）
	"event_order":    true, // kinds 清单的首现顺序严格递增
	"event_field":    true, // 某 kind 事件的字段满足 equals / non_empty
	"glob_count":     true, // project_root 相对 glob 命中文件数 ∈ [min, max]（如 graph 分片）
	"metric_bounds":  true, // 数值指标 ∈ [min, max]
}

// LoadSuite 加载并校验套件：prompt_file 在此解析为内联 Prompt，
// 让「套件目录可以整体搬走」与「运行期不再读套件外文件」同时成立。
func LoadSuite(path string) (*Suite, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取套件失败: %w", err)
	}
	var s Suite
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("套件 YAML 解析失败: %w", err)
	}
	s.Dir = filepath.Dir(path)

	if s.Name == "" {
		return nil, fmt.Errorf("套件缺少 name")
	}
	if len(s.Tasks) == 0 {
		return nil, fmt.Errorf("套件 tasks 为空")
	}
	if s.Defaults.TimeoutSec <= 0 {
		s.Defaults.TimeoutSec = 900
	}

	seen := map[string]bool{}
	for i := range s.Tasks {
		t := &s.Tasks[i]
		if t.Name == "" {
			return nil, fmt.Errorf("tasks[%d] 缺少 name", i)
		}
		if seen[t.Name] {
			return nil, fmt.Errorf("任务名重复: %q", t.Name)
		}
		seen[t.Name] = true

		if t.Prompt == "" && t.PromptFile == "" {
			return nil, fmt.Errorf("任务 %q 缺少 prompt / prompt_file", t.Name)
		}
		if t.Prompt != "" && t.PromptFile != "" {
			return nil, fmt.Errorf("任务 %q 的 prompt 与 prompt_file 只能二选一", t.Name)
		}
		if t.PromptFile != "" {
			content, err := os.ReadFile(filepath.Join(s.Dir, t.PromptFile))
			if err != nil {
				return nil, fmt.Errorf("任务 %q 的 prompt_file 读取失败: %w", t.Name, err)
			}
			t.Prompt = string(content)
		}
		if t.TimeoutSec <= 0 {
			t.TimeoutSec = s.Defaults.TimeoutSec
		}
		for _, f := range t.Fixtures {
			if err := validateRelPath(f.Path); err != nil {
				return nil, fmt.Errorf("任务 %q 的 fixture 路径非法: %w", t.Name, err)
			}
		}
		for _, j := range t.Judges {
			if !knownJudgeTypes[j.Type] {
				return nil, fmt.Errorf("任务 %q 含未知判据类型: %q", t.Name, j.Type)
			}
		}
	}
	return &s, nil
}

// validateRelPath 拒绝绝对路径与越出 project_root 的 .. 穿越。
func validateRelPath(p string) error {
	if p == "" || filepath.IsAbs(p) {
		return fmt.Errorf("路径必须为非空相对路径: %q", p)
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("路径不得越出 project_root: %q", p)
	}
	return nil
}
