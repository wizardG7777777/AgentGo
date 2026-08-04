package eval

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"
)

// 默认路径约定（eval/ 目录整体 gitignored）。
// 模板默认直接复用 setting.yaml：密钥与模型只有一份事实来源，免去重复配置；
// 占位符模式（eval/config.template.yaml + ${VAR}）经 -template 覆盖仍可用。
const (
	defaultSuitePath        = "eval/suite.yaml"
	defaultOfflineSuitePath = "eval/offline.yaml"
	defaultTemplatePath     = "setting.yaml"
	defaultBaselinePath     = "eval/baseline.json"
	defaultCandidatePath    = "eval/baseline.candidate.json" // record 的产出；promote 后才成为 accepted baseline
	reportsDir              = "eval/reports"
	compareBand             = 0.3 // 数值指标对比宽容带 ±30%
)

// CLI 实现独立开发工具 agentgo-eval 的子命令族入口（不启动主系统）。
// V6 起该入口不再编入普通 agentgo 二进制（docs/nextUpgrade-V6.md §7.6）。
// 退出码：0 = 通过；1 = 检查失败/存在失败任务或 hard 告警；2 = 用法/加载错误。
func CLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printEvalUsage(stderr)
		return 2
	}
	switch args[0] {
	case "preflight":
		return preflightCLI(args[1:], stdout, stderr)
	case "run":
		return runCLI(args[1:], stdout, stderr)
	case "record":
		return recordCLI(args[1:], stdout, stderr)
	case "promote":
		return promoteCLI(args[1:], stdout, stderr)
	case "offline":
		return offlineCLI(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "[错误] 未知 eval 子命令: %q\n", args[0])
		printEvalUsage(stderr)
		return 2
	}
}

// printEvalUsage 打印 eval 子命令族的用法。
func printEvalUsage(w io.Writer) {
	fmt.Fprintln(w, "用法: agentgo-eval <子命令> [选项]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "子命令:")
	fmt.Fprintln(w, "  preflight   评测凭证前置检查：环境变量注入 + LLM 密钥真实端点探测")
	fmt.Fprintln(w, "              选项: -template <路径>（默认 setting.yaml）")
	fmt.Fprintln(w, "  run         跑黄金任务套件并与基线对比（有基线时）")
	fmt.Fprintln(w, "              选项: -suite/-template/-task <名称>/-smoke/-binary <被测二进制>")
	fmt.Fprintln(w, "  record      跑整套件并把结果录制为基线候选（eval/baseline.candidate.json）")
	fmt.Fprintln(w, "              选项: -suite/-template/-binary <被测二进制>")
	fmt.Fprintln(w, "  promote     把 review 过的候选晋升为 accepted baseline（eval/baseline.json）")
	fmt.Fprintln(w, "  offline     离线 fake-LLM E2E：脚本化假端点驱动真实 agentgo 主链（全程离线）")
	fmt.Fprintln(w, "              选项: -suite（默认 eval/offline.yaml）/-template/-task <名称>/-binary")
}

// preflightCLI 实现 `agentgo-eval preflight [-template 路径]`。
func preflightCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("eval preflight", flag.ContinueOnError)
	fs.SetOutput(stderr)
	template := fs.String("template", defaultTemplatePath, "评测配置模板路径")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := Preflight(ctx, PreflightOptions{TemplatePath: *template}, stdout); err != nil {
		fmt.Fprintf(stderr, "[eval preflight] %v\n", err)
		return 1
	}
	return 0
}

// evalFlags 是 run / record 共用的参数集。
type evalFlags struct {
	suite    string
	template string
	task     string
	smoke    bool
	binary   string
}

func newEvalFlagSet(name string, stderr io.Writer, withTaskSelector bool) (*flag.FlagSet, *evalFlags) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	f := &evalFlags{}
	fs.StringVar(&f.suite, "suite", defaultSuitePath, "黄金任务套件路径")
	fs.StringVar(&f.template, "template", defaultTemplatePath, "评测配置模板路径")
	fs.StringVar(&f.binary, "binary", "", "被测 agentgo 二进制路径（默认依次探测 ./agentgo.exe、./agentgo）")
	if withTaskSelector {
		fs.StringVar(&f.task, "task", "", "只跑指定任务（按 name）")
		fs.BoolVar(&f.smoke, "smoke", false, "只跑 smoke 标记的便宜任务")
	}
	return fs, f
}

// runCLI 实现 `agentgo-eval run`：跑套件 → 落报告 → 有基线则对比。
func runCLI(args []string, stdout, stderr io.Writer) int {
	fs, f := newEvalFlagSet("eval run", stderr, true)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rep, code := executeSuite(f, stdout, stderr)
	if rep == nil {
		return code
	}

	// 基线对比：可比性键不匹配时 Compare 产出 not_comparable 告警（不满足 Release gate）
	base, err := LoadBaseline(defaultBaselinePath)
	if err != nil {
		fmt.Fprintf(stderr, "[警告] 基线读取失败，跳过对比: %v\n", err)
	} else if base == nil {
		rep.CompareSkipped = "基线不存在（先 agentgo-eval record 录制候选，review 后 promote 晋升）"
	} else if alerts, err := base.Compare(rep.Environment, rep.Results, compareBand); err != nil {
		rep.CompareSkipped = err.Error()
	} else {
		rep.Alerts = alerts
	}

	reportPath := filepath.Join(reportsDir, rep.RunID+".json")
	if err := SaveReport(reportPath, rep); err != nil {
		fmt.Fprintf(stderr, "[警告] 报告落盘失败: %v\n", err)
	}
	PrintSummary(stdout, rep)
	fmt.Fprintf(stdout, "报告: %s\n", reportPath)
	// not_comparable 不满足任何 Release gate：比较失去可比性时 run 不得判通过
	for _, a := range rep.Alerts {
		if a.Level == "not_comparable" {
			return 1
		}
	}
	if rep.AllPassed() {
		return 0
	}
	return 1
}

// recordCLI 实现 `agentgo-eval record`：跑整套件并把结果录制为基线候选
// （eval/baseline.candidate.json，不覆盖 accepted baseline）。V6 §7.6：
// 只有全部必需 case 通过、trace 完整并经显式 promote 后，候选才成为 accepted baseline。
func recordCLI(args []string, stdout, stderr io.Writer) int {
	fs, f := newEvalFlagSet("eval record", stderr, false)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rep, code := executeSuite(f, stdout, stderr)
	if rep == nil {
		return code
	}
	PrintSummary(stdout, rep)

	base := NewBaseline(rep.Suite, rep.Environment, rep.Results)
	if err := SaveBaseline(defaultCandidatePath, base); err != nil {
		fmt.Fprintf(stderr, "[错误] 候选基线落盘失败: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "基线候选已录制: %s（%d 个任务）\n", defaultCandidatePath, len(base.Tasks))
	if !rep.AllPassed() {
		fmt.Fprintf(stderr, "[警告] 本次运行存在失败任务——候选不能晋升（promote 将拒绝），修复后重新 record\n")
		return 1
	}
	fmt.Fprintf(stdout, "请 review 候选内容后执行 agentgo-eval promote 使其成为 accepted baseline\n")
	return 0
}

// promoteCLI 实现 `agentgo-eval promote`：把 review 过的候选晋升为 accepted baseline。
// 候选缺失、含失败任务或不完整时拒绝——失败/不完整运行不能覆盖现有基线。
func promoteCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "[错误] promote 不接受参数\n")
		return 2
	}
	cand, err := LoadBaseline(defaultCandidatePath)
	if err != nil {
		fmt.Fprintf(stderr, "[错误] 候选基线读取失败: %v\n", err)
		return 1
	}
	if cand == nil {
		fmt.Fprintf(stderr, "[错误] 不存在基线候选（%s）——先 agentgo-eval record 录制\n", defaultCandidatePath)
		return 1
	}
	var failed []string
	for name, t := range cand.Tasks {
		if !t.JudgesPassed {
			failed = append(failed, name)
		}
	}
	if len(failed) > 0 {
		fmt.Fprintf(stderr, "[错误] 候选含 %d 个未通过任务（%s），不能晋升——修复后重新 record\n",
			len(failed), strings.Join(failed, ", "))
		return 1
	}
	if err := SaveBaseline(defaultBaselinePath, cand); err != nil {
		fmt.Fprintf(stderr, "[错误] 基线晋升落盘失败: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "候选已晋升为 accepted baseline: %s（%d 个任务）\n", defaultBaselinePath, len(cand.Tasks))
	return 0
}

// offlineCLI 实现 `agentgo-eval offline`：脚本化 fake-LLM 端点 + 与 run 相同的
// 进程外驱动，全程离线。不跑 preflight（无凭证）、不做基线对比（参照系不同）。
func offlineCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("eval offline", flag.ContinueOnError)
	fs.SetOutput(stderr)
	suite := fs.String("suite", defaultOfflineSuitePath, "离线套件路径")
	template := fs.String("template", defaultTemplatePath, "配置模板路径（llm 块被 fake 端点覆盖）")
	task := fs.String("task", "", "只跑指定 case（按 name）")
	binary := fs.String("binary", "", "被测 agentgo 二进制路径（默认依次探测 ./agentgo.exe、./agentgo）")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	resolved, err := resolveTestBinary(*binary)
	if err != nil {
		fmt.Fprintf(stderr, "[错误] %v\n", err)
		return 2
	}

	// Ctrl+C 级联取消全部子进程
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	runner := NewOfflineRunner(OfflineOptions{
		SuitePath:    *suite,
		TemplatePath: *template,
		OnlyCase:     *task,
		BinaryPath:   resolved,
		Stdout:       stdout,
	})
	rep, err := runner.Run(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "[错误] 离线套件运行失败: %v\n", err)
		return 2
	}

	reportPath := filepath.Join(reportsDir, rep.RunID+".json")
	if err := SaveReport(reportPath, rep); err != nil {
		fmt.Fprintf(stderr, "[警告] 报告落盘失败: %v\n", err)
	}
	PrintSummary(stdout, rep)
	fmt.Fprintf(stdout, "报告: %s\n", reportPath)
	if rep.AllPassed() {
		return 0
	}
	return 1
}

// executeSuite 是 run/record 共用的执行链：preflight → 环境指纹 → 套件运行。
// 失败时打印错误并返回 (nil, 退出码)。
func executeSuite(f *evalFlags, stdout, stderr io.Writer) (*RunReport, int) {
	preflightCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := Preflight(preflightCtx, PreflightOptions{TemplatePath: f.template}, stdout); err != nil {
		fmt.Fprintf(stderr, "[eval] %v\n", err)
		return nil, 1
	}

	// 被测二进制必须显式解析：agentgo-eval 自身不是被测对象，
	// 不能再沿用 RunnerOptions.BinaryPath 为空时 os.Executable() 的回退。
	binary, err := resolveTestBinary(f.binary)
	if err != nil {
		fmt.Fprintf(stderr, "[错误] %v\n", err)
		return nil, 2
	}

	// Ctrl+C 级联取消全部子进程
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	runner := NewRunner(RunnerOptions{
		SuitePath:    f.suite,
		TemplatePath: f.template,
		OnlyTask:     f.task,
		SmokeOnly:    f.smoke,
		BinaryPath:   binary,
		Stdout:       stdout,
	})
	rep, err := runner.Run(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "[错误] 套件运行失败: %v\n", err)
		return nil, 2
	}
	return rep, 0
}

// resolveTestBinary 解析被测 agentgo 二进制路径：-binary 显式给定就用；
// 否则依次探测当前目录下的 agentgo.exe / agentgo。找不到即报错——
// agentgo-eval 是独立开发工具，os.Executable() 指向它自身，不能当回退。
// 返回绝对路径：Go exec 拒绝「相对当前目录的可执行文件」（exec.ErrDot
// 安全限制），裸文件名直接喂 exec.Command 会启动失败。
func resolveTestBinary(explicit string) (string, error) {
	abs := func(p string) (string, error) {
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("被测二进制不可读: %s: %w", p, err)
		}
		full, err := filepath.Abs(p)
		if err != nil {
			return "", fmt.Errorf("解析被测二进制绝对路径失败: %s: %w", p, err)
		}
		return full, nil
	}
	if explicit != "" {
		return abs(explicit)
	}
	for _, name := range []string{"agentgo.exe", "agentgo"} {
		if _, err := os.Stat(name); err == nil {
			return abs(name)
		}
	}
	return "", fmt.Errorf("未找到被测二进制（已探测 ./agentgo.exe、./agentgo）；请先 go build 或用 -binary 显式指定")
}
