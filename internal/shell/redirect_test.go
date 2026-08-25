package shell

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"agentgo/internal/interaction"
	"agentgo/internal/modes"
)

// 重定向写文件硬规则：双方言正例（= 拦截）。Check 必须走 block 通道并带
// RedirectWritePatternPrefix 前缀（与黑名单同一通道，不进灰名单 Interaction）。
func TestDetectRedirectWrite_Blocked(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		// ---- Unix sh ----
		{"覆盖写", "echo hello > out.txt"},
		{"追加写", "echo hello >> out.txt"},
		{"无空格", "echo hello>out.txt"},
		{"stderr 重定向到文件", "go build 2> build.err"},
		{"stderr 追加到文件", "go test 2>> test.log"},
		{"任意 fd 写文件", "cmd 3> debug.log"},
		{"fd 复制后仍写文件", "make 2>&1 > build.log"},
		{"先写文件再 fd 复制", "make > build.log 2>&1"},
		{"bash 合并重定向", "go build &> all.log"},
		{"bash 合并追加", "go build &>> all.log"},
		{"noclobber 强制写", "echo x >| forced.txt"},
		{"目标带引号", `echo a > "my file.txt"`},
		{"目标是变量", "echo x > $out"},
		{"here-doc 同行重定向", "cat <<EOF > out.txt\nhello\nEOF"},
		{"here-doc 结束后重定向", "cat <<EOF\nhello\nEOF\n> out.txt"},
		{"管道后重定向", "grep err a.log | sort > errors.txt"},
		{"分号后重定向", "echo a; echo b > b.txt"},
		// ---- Windows / PowerShell ----
		{"PS 覆盖写", `"hello" > greeting.txt`},
		{"PS 追加写", `"hello" >> greeting.txt`},
		{"PS 流重定向", "go build 2> build.log"},
		{"PS 全流重定向", "go test *> all.log"},
		{"Out-File", "Get-Content a.txt | Out-File b.txt"},
		{"Out-File 小写", "echo hi | out-file b.txt"},
		{"Out-File 带参数", "ls | Out-File -FilePath out.txt -Encoding utf8"},
		{"tee", "go build 2>&1 | tee build.log"},
		{"Tee-Object", "ls | Tee-Object -FilePath out.txt"},
		{"Tee-Object 小写", "ls | tee-object out.txt"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if detail := detectRedirectWrite(test.command); detail == "" {
				t.Fatalf("detectRedirectWrite(%q) 未命中，应拦截", test.command)
			}
			action, pattern := NewCommandFilter(nil, nil).Check(test.command)
			if action != "block" {
				t.Fatalf("Check(%q) action = %q, want block", test.command, action)
			}
			if !strings.HasPrefix(pattern, RedirectWritePatternPrefix) {
				t.Fatalf("Check(%q) pattern = %q, want 前缀 %q", test.command, pattern, RedirectWritePatternPrefix)
			}
		})
	}
}

// 重定向写文件硬规则：反例（= 放行）——比较运算符、丢弃输出、fd 复制、
// 引号字面量、here-doc 正文、行注释、输入重定向。
func TestDetectRedirectWrite_Allowed(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{"test 比较", "test 1 -gt 0"},
		{"[ ] 比较", "[ 5 -ge 3 ] && echo yes"},
		{"PS 比较", "if ($x -gt 5) { echo big }"},
		{"双引号内字面 >", `echo "a > b"`},
		{"单引号内字面 >", "awk '$1 > 5' data.txt"},
		{"引号内 Out-File", `echo "Out-File 用法说明"`},
		{"stderr 丢弃 /dev/null", "go build 2>/dev/null"},
		{"stderr 丢弃 /dev/null 带空格", "go build 2> /dev/null"},
		{"stdout 丢弃 /dev/null", "cmd > /dev/null"},
		{"stderr 丢弃 $null", "go build 2>$null"},
		{"stderr 丢弃 $NULL 大写", "go build 2> $NULL"},
		{"fd 复制 2>&1", "go build 2>&1"},
		{"fd 复制接管道", "go build 2>&1 | grep error"},
		{"fd 复制 >&2", "echo err >&2"},
		{"fd 关闭 >&-", "cmd >&-"},
		{"转义的 >", `echo a \> b`},
		{"here-doc 正文含 >", "python3 <<'EOF'\nprint(1 > 0)\nEOF"},
		{"here-doc 正文含 >>", "cat <<EOF\na >> b\nEOF"},
		{"here-doc <<- 剥 tab", "cat <<-EOF\n\tcontent > x\n\tEOF"},
		{"here-doc 正文含 Out-File", "cat <<EOF\nOut-File 说明\nEOF"},
		{"here-string", `cat <<< "a > b"`},
		{"输入重定向", "sort < input.txt"},
		{"行注释含 >", "echo hi # 把结果 > 写进去"},
		{"行尾孤立 >", "echo =>"},
		{"普通命令", "go test ./..."},
		{"git status", "git status"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if detail := detectRedirectWrite(test.command); detail != "" {
				t.Fatalf("detectRedirectWrite(%q) = %q, 应放行", test.command, detail)
			}
			if action, _ := NewCommandFilter(nil, nil).Check(test.command); action != "allow" {
				t.Fatalf("Check(%q) = %q, want allow", test.command, action)
			}
		})
	}
}

func TestHasPipelineDistinguishesPipelineFromLiteralsAndLogicalOr(t *testing.T) {
	tests := []struct {
		command string
		want    bool
	}{
		{"pytest -q 2>&1 | tail -20", true},
		{"Get-Content a | Select-String error", true},
		{"false || echo recovered", false},
		{`echo "a | b"`, false},
		{`echo a \| b`, false},
		{"cat <<'EOF'\na | b\nEOF", false},
		{"echo hi # a | b", false},
	}
	for _, test := range tests {
		if got := HasPipeline(test.command); got != test.want {
			t.Errorf("HasPipeline(%q)=%v want %v", test.command, got, test.want)
		}
	}
}

// 运行时白名单（allow_session）只能短路灰名单，不能覆盖重定向硬规则。
func TestCommandFilter_RedirectWriteNotBypassedByWhitelist(t *testing.T) {
	filter := NewCommandFilter(nil, []string{`^echo`})
	if err := filter.AddRuntimeWhitelist(`^echo`); err != nil {
		t.Fatal(err)
	}
	if action, _ := filter.Check("echo hello > out.txt"); action != "block" {
		t.Fatalf("运行时白名单不得覆盖重定向硬规则, got %q", action)
	}
	// 同命令去掉重定向：白名单正常短路灰名单。
	if action, _ := filter.Check("echo hello"); action != "allow" {
		t.Fatalf("白名单应短路灰名单, got %q", action)
	}
}

// WrapShellTool 集成：重定向写文件与黑名单同通道硬拒——不执行、不创建
// Interaction，拒绝消息指引改用 write_file / edit_file。
func TestWrapShellTool_RedirectWriteBlocked(t *testing.T) {
	service := interaction.NewService(nil)
	var executed atomic.Bool
	wrapper := testWrapper(t, NewCommandFilter(nil, nil), service,
		func(context.Context, map[string]any) (string, error) {
			executed.Store(true)
			return "", nil
		}, nil)
	_, err := wrapper(context.Background(), map[string]any{"command": "echo hello > out.txt"})
	if err == nil || !strings.Contains(err.Error(), "重定向") ||
		!strings.Contains(err.Error(), "write_file") || !strings.Contains(err.Error(), "edit_file") {
		t.Fatalf("拒绝消息应说明重定向并指引 write_file / edit_file: %v", err)
	}
	if executed.Load() {
		t.Fatal("重定向写文件命令不应执行")
	}
	if pending := waitPendingCount(t, service); pending != 0 {
		t.Fatalf("重定向硬拒不应创建 Interaction，pending=%d", pending)
	}
}

// 重定向硬拒不受 exec 轴影响：strict / yolo 下都直接拦截，行为同黑名单。
func TestWrapShellTool_RedirectWriteBlockedInStrictAndYolo(t *testing.T) {
	for _, entry := range []struct {
		name string
		mode modes.ExecMode
	}{
		{"strict", modes.ExecStrict},
		{"yolo", modes.ExecYolo},
	} {
		t.Run(entry.name, func(t *testing.T) {
			service := interaction.NewService(nil)
			modeStore := modes.NewStore(entry.mode, modes.TopoTeam)
			var executed atomic.Bool
			wrapper := WrapShellTool(func(context.Context, map[string]any) (string, error) {
				executed.Store(true)
				return "", nil
			}, NewCommandFilter(nil, nil), service,
				func() string { return "session-test" }, "worker-1", nil, modeStore)
			_, err := wrapper(context.Background(), map[string]any{"command": "echo x > out.txt"})
			if err == nil || !strings.Contains(err.Error(), "write_file") {
				t.Fatalf("%s 下重定向应硬拒并指引 write_file: %v", entry.name, err)
			}
			if executed.Load() {
				t.Fatalf("%s 下重定向命令不得执行", entry.name)
			}
			if pending := waitPendingCount(t, service); pending != 0 {
				t.Fatalf("%s 下重定向硬拒不应创建 Interaction，pending=%d", entry.name, pending)
			}
		})
	}
}
