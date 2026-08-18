package config

import (
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"agentgo/internal/tools"
)

// ============================================================
// config doctor —— 配置静态诊断
//
// 背景：LoadConfig + Validate 只校验配置的结构合法性（模型存在、kind 唯一、
// profile/tools 互斥等），不校验"每个 kind 的 system_prompt_file 里承诺的工具
// 能力"与"该 kind 实际解析出的工具白名单"是否一致。例如 prompt 写了"使用
// write_file"而 profile 并未授权该工具，启动期不会报任何错，只在运行期
// 以 "tool not found" 的形式暴露。doctor 把这类不一致提前到启动前可见。
// ============================================================

// DiagLevel 是 doctor 诊断的严重级别。
type DiagLevel int

const (
	// DiagError 表示 prompt 提及的工具不在该 kind 的白名单中（承诺了未授权能力）。
	DiagError DiagLevel = iota
	// DiagWarning 表示配置冗余（未被引用的 profile）或 prompt 文件不可读。
	DiagWarning
	// DiagInfo 表示白名单中的工具未在 prompt 中提及（能力未向模型声明，不强制）。
	DiagInfo
)

// label 返回打印用的中文级别名。
func (l DiagLevel) label() string {
	switch l {
	case DiagError:
		return "错误"
	case DiagWarning:
		return "警告"
	default:
		return "提示"
	}
}

// Diag 是一条 doctor 诊断。Kind 为空表示该诊断与具体 agent kind 无关
// （例如 tool_profiles 中存在未被引用的条目）。
type Diag struct {
	Level   DiagLevel
	Kind    string
	Message string
}

// DoctorReport 汇总一次 config doctor 的全部诊断。
type DoctorReport struct {
	Diags []Diag
}

// add 追加一条诊断。
func (r *DoctorReport) add(level DiagLevel, kind, message string) {
	r.Diags = append(r.Diags, Diag{Level: level, Kind: kind, Message: message})
}

// Count 统计指定级别的诊断条数。
func (r *DoctorReport) Count(level DiagLevel) int {
	n := 0
	for _, d := range r.Diags {
		if d.Level == level {
			n++
		}
	}
	return n
}

// HasError 报告是否存在 error 级诊断（决定进程退出码）。
func (r *DoctorReport) HasError() bool {
	return r.Count(DiagError) > 0
}

// Print 按 错误 → 警告 → 提示 分级逐条打印诊断，末尾输出一行汇总计数。
func (r *DoctorReport) Print(out io.Writer) {
	for _, level := range []DiagLevel{DiagError, DiagWarning, DiagInfo} {
		for _, d := range r.Diags {
			if d.Level != level {
				continue
			}
			if d.Kind != "" {
				fmt.Fprintf(out, "[%s] kind=%s：%s\n", level.label(), d.Kind, d.Message)
			} else {
				fmt.Fprintf(out, "[%s] %s\n", level.label(), d.Message)
			}
		}
	}
	if len(r.Diags) == 0 {
		fmt.Fprintln(out, "未发现问题：各 kind 的 prompt 承诺与工具白名单一致。")
	}
	fmt.Fprintf(out, "汇总：错误 %d，警告 %d，提示 %d\n",
		r.Count(DiagError), r.Count(DiagWarning), r.Count(DiagInfo))
}

// promptToolNamePattern 按词边界匹配所有已知工具名。
//
// AllToolNames 只含小写字母 / 数字 / 下划线，无正则元字符，可直接拼接。
// Go regexp 的 \b 是 ASCII 词边界（[0-9A-Za-z_] 视为词字符），因此：
//   - "my_read_file" / "read_files" 不会误命中 read_file；
//   - 中文与工具名直接相邻（"使用read_file读取"）能正常命中。
var promptToolNamePattern = regexp.MustCompile(`\b(` + strings.Join(tools.AllToolNames, "|") + `)\b`)

// negationMarkers 是否定/劝阻语境的启发式行级标记集合。
// 命中任一标记的整行被视为"否定语境"，该行工具名命中不计入 mentioned。
//
// 已知局限（刻意接受的保守取舍）：
//   - 以行为单位判断，不做跨行语义分析，也不区分否定对象是否就是该工具名；
//   - 短标记存在误伤："别" 会命中 "特别/区别/识别"，"而非" 会命中正常对比句——
//     代价是该行工具名被漏记（可能少报 error），而不会多报；
//   - "只有……才能使用 X" 这类条件授权句不含上述标记，仍计入 mentioned
//     （worker.md 的 publish_task 条款即属此类，doctor 不该吞掉它）。
var negationMarkers = []string{"没有", "不要", "禁止", "不存在", "严禁", "而非", "不用", "别"}

// lineHasNegation 报告该行是否包含否定/劝阻标记。
func lineHasNegation(line string) bool {
	for _, m := range negationMarkers {
		if strings.Contains(line, m) {
			return true
		}
	}
	return false
}

// scanPromptToolNames 逐行扫描 prompt 文本中出现的已知工具名，返回去重后的
// 命中列表（按工具名字典序，保证输出稳定）。
//
// 否定语境启发式：含 negationMarkers 任一标记的行被整行跳过——"没有 report_done
// 工具"、"不要用 send_message …" 这类劝阻式提及不算"prompt 承诺的能力"。
// 被否定行排除的命中既不产生 error，也不抑制 info：某工具若仅被否定提及且不在
// 白名单 → 零诊断；若恰在白名单 → 仍会报"未声明"info（保守行为，刻意接受）。
// 同一工具只要在任一非否定行出现，仍正常计入 mentioned。
func scanPromptToolNames(promptText string) []string {
	// 边界归一化 CRLF，避免 \r 干扰行切分（跨平台约束）
	text := strings.ReplaceAll(promptText, "\r\n", "\n")
	seen := make(map[string]bool)
	names := make([]string, 0)
	for _, line := range strings.Split(text, "\n") {
		if lineHasNegation(line) {
			continue
		}
		for _, h := range promptToolNamePattern.FindAllString(line, -1) {
			if !seen[h] {
				seen[h] = true
				names = append(names, h)
			}
		}
	}
	sort.Strings(names)
	return names
}

// resolveAllowlist 解析该 kind 的实际工具白名单：tools 直列优先，否则按
// profile 引用 tool_profiles 表。返回 (nil, nil) 只保留给未经过 Config.Validate
// 的兼容调用；生产配置会拒绝缺失或空 profile，非 nil 空 allowlist 也不会全开。
func (k *AgentKind) resolveAllowlist(profiles map[string][]string) ([]string, error) {
	if len(k.Tools) > 0 {
		return k.Tools, nil
	}
	return ResolveToolProfile(profiles, k.Profile)
}

// Doctor 对配置执行"prompt 承诺能力 vs 实际工具白名单"的一致性诊断。
// 调用方需先完成 LoadConfig + Validate（本函数不做结构校验，只对账）。
//
// 检查项：
//   - 逐 kind 读取 system_prompt_file（配置层路径，允许绝对路径，见
//     AGENTS.md "Asymmetry with config-layer paths"），扫描其中出现的已知工具名
//     （含否定/劝阻标记的行按启发式整行排除，见 scanPromptToolNames）；
//   - error：prompt 提及的工具不在该 kind 解析出的白名单中；
//   - info：白名单中的工具未在 prompt 中提及；
//   - warning：system_prompt_file 不可读（跳过该 kind 对账）；tool_profiles
//     中存在未被任何 kind 引用的条目。
func (c *Config) Doctor() *DoctorReport {
	rep := &DoctorReport{}
	referencedProfiles := make(map[string]bool, len(c.Agents))

	for i := range c.Agents {
		k := &c.Agents[i]
		if k.Profile != "" {
			referencedProfiles[k.Profile] = true
		}

		content, err := os.ReadFile(k.SystemPromptFile)
		if err != nil {
			rep.add(DiagWarning, k.Kind, fmt.Sprintf(
				"system_prompt_file=%q 不存在或不可读: %v（已跳过该 kind 的 prompt 对账）",
				k.SystemPromptFile, err))
			continue
		}

		allowlist, err := k.resolveAllowlist(c.ToolProfiles)
		if err != nil {
			// Validate 规则 6 已拦截不存在的 profile，此处为防御性兜底。
			rep.add(DiagWarning, k.Kind, fmt.Sprintf(
				"无法解析工具白名单: %v（已跳过该 kind 的 prompt 对账）", err))
			continue
		}
		if allowlist == nil {
			// nil 白名单表示"允许全部工具"：不存在"承诺了未授权能力"的可能，
			// info 方向（白名单未声明）也无从枚举，跳过该 kind。
			continue
		}

		mentioned := scanPromptToolNames(string(content))
		mentionedSet := make(map[string]bool, len(mentioned))
		for _, name := range mentioned {
			mentionedSet[name] = true
		}
		allowSet := make(map[string]bool, len(allowlist))
		for _, name := range allowlist {
			allowSet[name] = true
		}

		// error：prompt 提及但白名单未授权（mentioned 已按字典序排序）
		for _, name := range mentioned {
			if !allowSet[name] {
				rep.add(DiagError, k.Kind, fmt.Sprintf(
					"prompt 提及工具 %q，但该 kind 解析出的工具白名单未包含它（prompt 承诺了未授权的能力）", name))
			}
		}
		// info：白名单已授权但 prompt 从未提及（按白名单声明顺序）
		for _, name := range allowlist {
			if !mentionedSet[name] {
				rep.add(DiagInfo, k.Kind, fmt.Sprintf(
					"白名单工具 %q 未在 prompt 中提及（能力未向模型声明，不强制要求）", name))
			}
		}
	}

	// warning：tool_profiles 中未被任何 kind 引用的条目（按名称字典序，输出稳定）
	unused := make([]string, 0, len(c.ToolProfiles))
	for name := range c.ToolProfiles {
		if !referencedProfiles[name] {
			unused = append(unused, name)
		}
	}
	sort.Strings(unused)
	for _, name := range unused {
		rep.add(DiagWarning, "", fmt.Sprintf("tool_profiles 中的 %q 未被任何 kind 引用", name))
	}

	return rep
}

// CLI 是 `agentgo config` 子命令族的入口，返回进程退出码。
// 与 trace 子命令一样只做静态检查，不启动主系统。
func CLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printConfigUsage(stderr)
		return 2
	}
	switch args[0] {
	case "doctor":
		return doctorCLI(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "[错误] 未知 config 子命令: %q\n", args[0])
		printConfigUsage(stderr)
		return 2
	}
}

// printConfigUsage 打印 config 子命令族的用法。
func printConfigUsage(w io.Writer) {
	fmt.Fprintln(w, "用法: agentgo config <子命令> [选项]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "子命令:")
	fmt.Fprintln(w, "  doctor    检查配置中 prompt 承诺能力与工具白名单的一致性")
	fmt.Fprintln(w, "            选项: -config <路径>（默认 setting.yaml）")
}

// doctorCLI 实现 `agentgo config doctor [-config 路径]`。
// 退出码：0 = 无 error 级诊断；1 = 存在 error 级诊断；2 = 用法 / 加载 / 校验失败。
func doctorCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("config doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "setting.yaml", "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// 与主流程一致的 explicit 语义：显式指定路径时文件缺失即报错
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			explicit = true
		}
	})

	cfg, err := LoadConfig(*configPath, explicit)
	if err != nil {
		fmt.Fprintf(stderr, "[错误] 配置加载失败: %v\n", err)
		return 2
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(stderr, "[错误] 配置校验失败: %v\n", err)
		return 2
	}

	rep := cfg.Doctor()
	rep.Print(stdout)
	if rep.HasError() {
		return 1
	}
	return 0
}
