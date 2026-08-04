package scheduler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentgo/internal/config"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/roster"
	"agentgo/internal/store"
)

// newExecModeTestBundle 与 newSoloTestBundle 同形态，但允许指定 ProjectRoot，
// 供 exec 轴装配断言（strict 写工具审批 / run_shell 短路）使用。
// interactions 为 nil：strict 下写工具与 run_shell 的审批路径应 fail-closed。
func newExecModeTestBundle(t *testing.T, modeStore *modes.Store, projectRoot string, mockLLM llm.Client) (*Bundle, *store.MemoryTaskStore, *model.Task) {
	t.Helper()
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	r := roster.NewMemoryRoster()
	mb := mailbox.NewRegistry(8)
	cfg := config.DefaultConfig()
	cfg.Agents = []config.AgentKind{{Kind: "worker", Replicas: 1}}
	cfg.ProjectRoot = projectRoot

	bundle := New(s, r, mockLLM, ch, cfg, nil, mb, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, modeStore, nil, nil, nil)

	task := &model.Task{Description: "exec 模式装配测试任务", EventType: "__scheduler__"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("发布 scheduler 任务失败: %v", err)
	}
	if err := s.ClaimTask(bundle.Agent.ID, task.ID); err != nil {
		t.Fatalf("认领 scheduler 任务失败: %v", err)
	}
	bundle.Agent.OnTaskStart(task.ID)
	t.Cleanup(func() { bundle.Agent.OnTaskEnd(task.ID, true) })
	return bundle, s, task
}

// 装配断言：exec=strict 时 scheduler 的 write_file 进入审批链路——
// Interaction 服务缺失时 fail-closed（证明 WrapHandler 已在 scheduler.New 生效，
// solo 拓扑下 scheduler 亲自写文件的路径同样被覆盖）。
func TestSchedulerStrictWriteFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.txt")
	mockLLM := &scriptedLLM{responses: []llm.Response{{
		ToolCalls: []llm.ToolCall{{
			ID:        "call_1",
			Name:      "write_file",
			Arguments: map[string]any{"path": target, "content": "x"},
		}},
	}}}
	bundle, _, task := newExecModeTestBundle(t,
		modes.NewStore(modes.ExecStrict, modes.TopoSolo), dir, mockLLM)

	content := executeOneRound(t, bundle, task)
	if !strings.Contains(content, "Interaction 服务不可用") {
		t.Fatalf("strict 下 scheduler 的 write_file 应 fail-closed，实际: %s", content)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("被拦截的写入不应落盘: %v", err)
	}
}

// 装配断言：exec=strict 时 scheduler 的 run_shell 对普通命令也进入审批链路
// （ShellGroup.Modes 已接线）；Interaction 服务缺失时 fail-closed。
func TestSchedulerStrictRunShellFailsClosed(t *testing.T) {
	mockLLM := &scriptedLLM{responses: []llm.Response{{
		ToolCalls: []llm.ToolCall{{
			ID:        "call_1",
			Name:      "run_shell",
			Arguments: map[string]any{"command": "echo hello"},
		}},
	}}}
	bundle, _, task := newExecModeTestBundle(t,
		modes.NewStore(modes.ExecStrict, modes.TopoTeam), t.TempDir(), mockLLM)

	content := executeOneRound(t, bundle, task)
	if !strings.Contains(content, "Interaction 服务不可用") {
		t.Fatalf("strict 下 scheduler 的 run_shell 普通命令应 fail-closed，实际: %s", content)
	}
}

// 对照组：exec=normal 时 scheduler 的 write_file 透传执行（包装器不影响正常档位）。
func TestSchedulerNormalWriteFilePassthrough(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ok.txt")
	mockLLM := &scriptedLLM{responses: []llm.Response{{
		ToolCalls: []llm.ToolCall{{
			ID:        "call_1",
			Name:      "write_file",
			Arguments: map[string]any{"path": target, "content": "hello"},
		}},
	}}}
	bundle, _, task := newExecModeTestBundle(t,
		modes.NewStore(modes.ExecNormal, modes.TopoTeam), dir, mockLLM)

	content := executeOneRound(t, bundle, task)
	if !strings.Contains(content, "文件已写入") {
		t.Fatalf("normal 下 write_file 应成功，实际: %s", content)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "hello" {
		t.Fatalf("落盘内容 = %q, err=%v", data, err)
	}
}

// 上下文兜底：scheduler.New 的 nil modeStore 回落 DefaultStore（normal），
// 写工具审批包装器不拦截。
func TestSchedulerNilModesWriteFilePassthrough(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "nil-modes.txt")
	mockLLM := &scriptedLLM{responses: []llm.Response{{
		ToolCalls: []llm.ToolCall{{
			ID:        "call_1",
			Name:      "write_file",
			Arguments: map[string]any{"path": target, "content": "z"},
		}},
	}}}
	bundle, _, task := newExecModeTestBundle(t, nil, dir, mockLLM)

	content := executeOneRound(t, bundle, task)
	if !strings.Contains(content, "文件已写入") {
		t.Fatalf("nil modeStore 应等价 normal，实际: %s", content)
	}
}
