package eval

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"time"
)

// 默认路径约定（eval/ 目录整体 gitignored）。
// 模板默认直接复用 setting.yaml：密钥与模型只有一份事实来源，免去重复配置；
// 占位符模式（eval/config.template.yaml + ${VAR}）经 -template 覆盖仍可用。
const (
	defaultSuitePath    = "eval/suite.yaml"
	defaultTemplatePath = "setting.yaml"
	defaultBaselinePath = "eval/baseline.json"
	reportsDir          = "eval/reports"
	compareBand         = 0.3 // 数值指标对比宽容带 ±30%
)

// CLI 实现 agentgo eval 子命令族入口（不启动主系统）。
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
	default:
		fmt.Fprintf(stderr, "[错误] 未知 eval 子命令: %q\n", args[0])
		printEvalUsage(stderr)
		return 2
	}
}

// printEvalUsage 打印 eval 子命令族的用法。
func printEvalUsage(w io.Writer) {
	fmt.Fprintln(w, "用法: agentgo eval <子命令> [选项]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "子命令:")
	fmt.Fprintln(w, "  preflight   评测凭证前置检查：环境变量注入 + LLM 密钥真实端点探测")
	fmt.Fprintln(w, "              选项: -template <路径>（默认 setting.yaml）")
	fmt.Fprintln(w, "  run         跑黄金任务套件并与基线对比（有基线时）")
	fmt.Fprintln(w, "              选项: -suite/-template/-task <名称>/-smoke")
	fmt.Fprintln(w, "  record      跑整套件并把结果录制为新基线（eval/baseline.json）")
	fmt.Fprintln(w, "              选项: -suite/-template")
}

// preflightCLI 实现 `agentgo eval preflight [-template 路径]`。
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
}

func newEvalFlagSet(name string, stderr io.Writer, withTaskSelector bool) (*flag.FlagSet, *evalFlags) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	f := &evalFlags{}
	fs.StringVar(&f.suite, "suite", defaultSuitePath, "黄金任务套件路径")
	fs.StringVar(&f.template, "template", defaultTemplatePath, "评测配置模板路径")
	if withTaskSelector {
		fs.StringVar(&f.task, "task", "", "只跑指定任务（按 name）")
		fs.BoolVar(&f.smoke, "smoke", false, "只跑 smoke 标记的便宜任务")
	}
	return fs, f
}

// runCLI 实现 `agentgo eval run`：跑套件 → 落报告 → 有基线则对比。
func runCLI(args []string, stdout, stderr io.Writer) int {
	fs, f := newEvalFlagSet("eval run", stderr, true)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rep, code := executeSuite(f, stdout, stderr)
	if rep == nil {
		return code
	}

	// 基线对比：环境三元组不匹配时拒绝对比（参照系不同，比了也是误导）
	base, err := LoadBaseline(defaultBaselinePath)
	if err != nil {
		fmt.Fprintf(stderr, "[警告] 基线读取失败，跳过对比: %v\n", err)
	} else if base == nil {
		rep.CompareSkipped = "基线不存在（用 agentgo eval record 录制首份基线）"
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
	if rep.AllPassed() {
		return 0
	}
	return 1
}

// recordCLI 实现 `agentgo eval record`：跑整套件并录制新基线。
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
	if err := SaveBaseline(defaultBaselinePath, base); err != nil {
		fmt.Fprintf(stderr, "[错误] 基线落盘失败: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "基线已录制: %s（%d 个任务）\n", defaultBaselinePath, len(base.Tasks))
	if !rep.AllPassed() {
		fmt.Fprintf(stderr, "[警告] 本次运行存在失败任务——基线以失败为基准，后续对比将以此为锚，确认这是你要的参照系\n")
		return 1
	}
	return 0
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

	// Ctrl+C 级联取消全部子进程
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	runner := NewRunner(RunnerOptions{
		SuitePath:    f.suite,
		TemplatePath: f.template,
		OnlyTask:     f.task,
		SmokeOnly:    f.smoke,
		Stdout:       stdout,
	})
	rep, err := runner.Run(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "[错误] 套件运行失败: %v\n", err)
		return nil, 2
	}
	return rep, 0
}
