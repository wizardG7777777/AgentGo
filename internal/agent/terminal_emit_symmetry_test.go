package agent

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestTerminalEmitSymmetry 是 nextUpgrade_v4.md §11.8 S11 的实现：
// 终结路径 trace.Emit 对称扫描——永久不变量回归测试。
//
// 守护的不变量（缺一即视为"装配漏接"）：
//
//  1. 每个 a.Store.FailTask(...) 调用所在的函数体内必须存在
//     trace.Emit{Kind: trace.KindTaskFailed} 调用——保证 panic-recovery /
//     terminateTask 等所有失败路径对 trace 观察者可见。
//  2. processTask 函数体内每个 case <-ctx.Done(): 终结分支必须伴随
//     trace.Emit{Kind: trace.KindTaskCancelled}（Run() 轮询循环 / sleep()
//     等非任务终结的 ctx.Done 通过函数名豁免）。
//  3. 文件级至少存在 2 处 KindTaskCompleted emit（FinalizationChecker 短路
//     路径 + 主成功路径），防止合并/重构时一次性丢掉成功 emit。
//
// 历史背景（CLAUDE.md "Shipping conventions" 第 1 条）：2026-04-19 一晚单测
// session 暴露 4 个"零件单测过 → 装配握手位无人测"缺陷（Trace CLI 路径脱钩 /
// history.jsonl 断链 / Finalization 短路 emit 漏 / Mail chain_depth 全程为 0）。
// 2026-04-26 实施本测试时又再发现一处 panic-recovery 路径 FailTask 后未
// emit KindTaskFailed（同 commit 已修），印证此类缺陷不是孤例。本测试是
// agent 终结路径上的永久护栏。
//
// 扫描目标：internal/agent/agent.go
// 注：v4.md §11.8 S11 原文写"扫 internal/agent/runner.go"，但 v4 抽出的
// internal/runner/runner.go 仅是薄壳调用 a.Run；终结状态转换仍完全位于
// internal/agent/agent.go 的 processTask + terminateTask + Run defer。所以
// 本测试扫 agent.go 才是真正的不变量边界——v4.md S11 措辞已同步修正。
func TestTerminalEmitSymmetry(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "agent.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 agent.go 失败: %v", err)
	}

	type emitSite struct {
		Kind string // 例如 "KindTaskFailed"
		Line int
		Func string // enclosing function name
	}
	type failSite struct {
		Line int
		Func string
	}

	var emits []emitSite
	var fails []failSite
	var processTaskCancelLines []int

	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		funcName := fd.Name.Name

		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if k := extractEmitKind(x); k != "" {
					emits = append(emits, emitSite{
						Kind: k,
						Line: fset.Position(x.Pos()).Line,
						Func: funcName,
					})
				} else if isFailTaskCall(x) {
					fails = append(fails, failSite{
						Line: fset.Position(x.Pos()).Line,
						Func: funcName,
					})
				}
			case *ast.CommClause:
				// 检测 case <-ctx.Done():
				if !isCtxDoneCase(x) {
					return true
				}
				if funcName == "processTask" {
					processTaskCancelLines = append(processTaskCancelLines, fset.Position(x.Pos()).Line)
				}
			}
			return true
		})
	}

	// 不变量 1：每个 FailTask 对应函数体内必须存在 KindTaskFailed emit。
	for _, fs := range fails {
		found := false
		for _, em := range emits {
			if em.Func == fs.Func && em.Kind == "KindTaskFailed" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("装配漏接：agent.go:%d 在函数 %s 内调用 a.Store.FailTask 但同函数体内未发现 trace.Emit{Kind: trace.KindTaskFailed}——参考 §11.8 S11 不变量 1",
				fs.Line, fs.Func)
		}
	}

	// 不变量 2：processTask 内每个 case <-ctx.Done(): 必须配 KindTaskCancelled emit。
	if len(processTaskCancelLines) > 0 {
		hasCancelEmit := false
		for _, em := range emits {
			if em.Func == "processTask" && em.Kind == "KindTaskCancelled" {
				hasCancelEmit = true
				break
			}
		}
		if !hasCancelEmit {
			t.Errorf("装配漏接：processTask 含 %d 个 case <-ctx.Done() 终结分支（行 %v）但函数体内未发现 trace.Emit{Kind: trace.KindTaskCancelled}——参考 §11.8 S11 不变量 2",
				len(processTaskCancelLines), processTaskCancelLines)
		}
	}

	// 不变量 3：文件级至少 2 处 KindTaskCompleted emit。
	completedCount := 0
	for _, em := range emits {
		if em.Kind == "KindTaskCompleted" {
			completedCount++
		}
	}
	if completedCount < 2 {
		t.Errorf("装配漏接：agent.go 中 trace.Emit{Kind: trace.KindTaskCompleted} 数量 = %d，预期 >= 2（FinalizationChecker 短路路径 + 主成功路径）——参考 §11.8 S11 不变量 3",
			completedCount)
	}

	// 启发式自检：测试本身被裁掉时也要红——空 emits 列表显然不正常。
	if len(emits) == 0 {
		t.Fatalf("AST 扫描未发现任何 trace.Emit 调用——测试本身可能损坏，请检查 isTraceEmit/extractEmitKind 是否仍匹配当前 agent.go 写法")
	}
	if len(fails) == 0 {
		t.Fatalf("AST 扫描未发现任何 a.Store.FailTask 调用——若属意删除该路径请同步评审本不变量；若不是请检查 isFailTaskCall 匹配规则")
	}
}

// extractEmitKind 若 call 形如 trace.Emit(trace.Event{Kind: trace.KindXxx, ...})
// 则返回 "KindXxx"，否则返回空串。
func extractEmitKind(call *ast.CallExpr) string {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "trace" || sel.Sel.Name != "Emit" {
		return ""
	}
	if len(call.Args) != 1 {
		return ""
	}
	cl, ok := call.Args[0].(*ast.CompositeLit)
	if !ok {
		return ""
	}
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		keyIdent, ok := kv.Key.(*ast.Ident)
		if !ok || keyIdent.Name != "Kind" {
			continue
		}
		valSel, ok := kv.Value.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		return valSel.Sel.Name
	}
	return ""
}

// isFailTaskCall 匹配形如 something.Store.FailTask(...) 的调用。
func isFailTaskCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "FailTask" {
		return false
	}
	inner, ok := sel.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	return inner.Sel.Name == "Store"
}

// isCtxDoneCase 判定 CommClause 是否为 `case <-ctx.Done():` 形态。
func isCtxDoneCase(cc *ast.CommClause) bool {
	if cc.Comm == nil {
		return false
	}
	expr, ok := cc.Comm.(*ast.ExprStmt)
	if !ok {
		return false
	}
	u, ok := expr.X.(*ast.UnaryExpr)
	if !ok || u.Op != token.ARROW {
		return false
	}
	callExpr, ok := u.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := callExpr.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	return sel.Sel.Name == "Done"
}
