package contextruntime_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 架构回归检查真实生产源码；测试夹具不能成为保留旧执行路径的理由。
func TestProductionArchitectureHasOneInvocationPath(t *testing.T) {
	retired := map[string]bool{}
	for _, name := range []string{"PromptSource", "PromptIdentityProvider", "compilePromptBuild", "withPromptBuild", "promptBuildFromContext", "buildLegacyMessages", "buildLegacyContextManifest", "InvokeLegacy", "ContextBinding", "WithModelOverride", "WithStreamHandler", "NewLegacyLLMCompleter", "StreamOutput", "NewLLMExecutor", "NewSwappableLLMExecutor"} {
		retired[name] = true
	}
	invocations := 0
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testmodel" || d.Name() == "testagent" || d.Name() == "testhttp" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		owner := filepath.Base(filepath.Dir(path))
		for _, im := range file.Imports {
			value, _ := strconv.Unquote(im.Path.Value)
			if strings.Contains(value, "openai-go") && owner != "llm" {
				t.Errorf("SDK 逃出 L1: %s", path)
			}
			for _, old := range []string{"prompt", "invocation", "contextadapter"} {
				if value == "agentgo/internal/"+old {
					t.Errorf("仍引用退役包: %s", path)
				}
			}
			if owner == "llm" || owner == "contextruntime" {
				for _, forbidden := range []string{"agent", "graph", "ui", "tui", "dashboard"} {
					if value == "agentgo/internal/"+forbidden {
						t.Errorf("层级反向依赖: %s -> %s", path, value)
					}
				}
			}
			if owner == "llm" && (value == "agentgo/internal/memory" || value == "agentgo/internal/taskmem") {
				t.Errorf("L1 读取记忆: %s", path)
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && retired[id.Name] {
				t.Errorf("退役符号仍在生产代码: %s %s", path, id.Name)
			}
			if c, ok := n.(*ast.CallExpr); ok {
				if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Invoke" {
					invocations++
					if owner != "contextruntime" {
						t.Errorf("模型调用绕过 L2: %s", path)
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if invocations != 1 {
		t.Fatalf("生产调用入口数量=%d，期望 1", invocations)
	}
}
