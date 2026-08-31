package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/checkstore"
	"agentgo/internal/contentstore"
	"agentgo/internal/contextcontract"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/shell"
	"agentgo/internal/store"
	"agentgo/internal/tools/schema"
)

var checkKinds = []string{"test", "build", "lint", "typecheck", "verification"}

// CheckGroup 把受约束 Shell 执行投影为 durable CheckRecord。它复用 ShellGroup
// 的路径、审批、过滤、timeout 与 Effect 边界，不形成第二套命令执行实现。
type CheckGroup struct {
	Shell        ShellGroup
	TaskStore    store.TaskStore
	Checks       *checkstore.Store
	ContentStore *contentstore.Store
	Holder       TaskHolder
	SessionID    func() string
	Workspaces   checkstore.WorkspaceRevisionResolver
}

func (g CheckGroup) Register(r *agent.ToolRegistry) {
	params := schema.Object().
		String("check_id", "GraphContract 中声明的检查 ID", true).
		Enum("kind", "检查类型", checkKinds, true).
		String("command", "不含 pipeline/重定向的完整检查命令", true).
		String("working_dir", "项目根内工作目录，默认 .", false).
		Build()
	r.Register("run_check", "运行一次受约束检查并生成 durable CheckRecord；只有 whole-command exit=0 才是 pass，后续文件修改会使该检查 stale", params, g.run)
}

func (g CheckGroup) run(ctx context.Context, args map[string]any) (string, error) {
	if g.TaskStore == nil || g.Holder == nil {
		return "", fmt.Errorf("run_check 缺少 TaskStore/Holder")
	}
	command, _ := args["command"].(string)
	if strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("run_check 缺少 command")
	}
	if shell.HasPipeline(command) {
		return "", fmt.Errorf("reason_code=check_pipeline_forbidden：run_check 禁止 pipeline；请运行能由单一 whole-command exit code 判定的命令")
	}
	if shell.HasFileRedirect(command) {
		return "", fmt.Errorf("reason_code=check_redirect_forbidden：run_check 禁止输出重定向；检查不得借此通道改写业务文件")
	}
	task, err := g.TaskStore.GetTask(g.Holder.Get())
	if err != nil || task == nil || task.Status != model.TaskStatusProcessing {
		return "", fmt.Errorf("run_check 缺少 processing Task: %v", err)
	}
	checkID := strings.TrimSpace(fmt.Sprint(args["check_id"]))
	if (task.FulfillmentContract != nil && len(task.FulfillmentContract.RequiredCheckIDs) > 0) ||
		(task.RunContract != nil && len(task.RunContract.CheckContracts) > 0) {
		allowedSet := make(map[string]struct{})
		allowed := make([]string, 0)
		valid := false
		if task.FulfillmentContract != nil {
			for _, raw := range task.FulfillmentContract.RequiredCheckIDs {
				id := strings.TrimSpace(raw)
				if id == "" {
					continue
				}
				allowedSet[id] = struct{}{}
			}
		}
		if task.RunContract != nil {
			for _, contract := range task.RunContract.CheckContracts {
				allowedSet[strings.TrimSpace(contract.CheckID)] = struct{}{}
			}
		}
		for id := range allowedSet {
			allowed = append(allowed, id)
			valid = valid || id == checkID
		}
		sort.Strings(allowed)
		if !valid {
			return "", fmt.Errorf("reason_code=check_id_not_declared：check_id=%q 不在当前 GraphContract 允许集 %v；请从 tool schema enum 逐字复制", checkID, allowed)
		}
	}
	if task.RunContract != nil {
		for _, contract := range task.RunContract.CheckContracts {
			if strings.TrimSpace(contract.CheckID) != checkID {
				continue
			}
			kind := strings.TrimSpace(fmt.Sprint(args["kind"]))
			if kind != contract.Kind {
				return "", fmt.Errorf("reason_code=check_kind_contract_mismatch：check_id=%q 要求 kind=%q，实际=%q", checkID, contract.Kind, kind)
			}
			if contract.ExactCommand != "" && command != contract.ExactCommand {
				return "", fmt.Errorf("reason_code=check_command_contract_mismatch：check_id=%q 必须逐字执行冻结 exact_command=%q", checkID, contract.ExactCommand)
			}
			break
		}
	}
	if g.Checks == nil || g.ContentStore == nil {
		return "", fmt.Errorf("run_check 缺少 CheckStore/ContentStore")
	}
	workspaceRef, _, err := checkstore.WorkspaceRevision(task, g.TaskStore, g.Workspaces)
	if err != nil {
		return "", err
	}
	started := time.Now().UTC()
	registry := agent.NewToolRegistry()
	g.Shell.Register(registry)
	shellArgs := map[string]any{"command": command}
	if workingDir, _ := args["working_dir"].(string); strings.TrimSpace(workingDir) != "" {
		shellArgs["working_dir"] = workingDir
	}
	output, shellErr := registry.Dispatch(ctx, llm.ToolCall{ID: "run-check-inner", Name: "run_shell", Arguments: shellArgs})
	exitCode, scope := parseCheckShellResult(output)
	status := checkstore.StatusFailed
	if shellErr == nil && exitCode == 0 && scope == string(store.ShellExitCodeScopeWholeCommand) {
		status = checkstore.StatusPass
	}
	sessionID := ""
	if g.SessionID != nil {
		sessionID = strings.TrimSpace(g.SessionID())
	}
	if sessionID == "" {
		sessionID = string(task.RunID)
	}
	contentRef, putErr := g.ContentStore.Put(ctx, contentstore.PutRequest{
		Content: []byte(output), MediaType: "text/plain; charset=utf-8",
		RetentionClass: contextcontract.RetentionTaskLifetime,
		Authority:      contextcontract.AuthorityInformational,
		Scope:          contentstore.Scope{Kind: contentstore.ScopeTask, SessionID: sessionID, GraphID: task.GraphID, TaskID: task.ID},
		ExpiresAt: func() time.Time {
			if task.RunContract != nil {
				return task.RunContract.DeadlineAt
			}
			return time.Time{}
		}(),
	})
	if putErr != nil {
		return "", fmt.Errorf("run_check 保存输出失败: %w", putErr)
	}
	record := checkstore.Record{
		Schema: checkstore.SchemaV1, RunID: string(task.RunID), GraphID: task.GraphID,
		TaskID: task.ID, AttemptID: task.AttemptID, ActivationID: task.ActivationID,
		CheckID: checkID, Kind: strings.TrimSpace(fmt.Sprint(args["kind"])),
		CommandDigest: checkstore.CommandDigest(command), Status: status, ExitCode: exitCode,
		ExitCodeScope: scope, WorkspaceRevisionRef: workspaceRef, OutputRef: contentRef.RefID,
		StartedAt: started, SettledAt: time.Now().UTC(),
	}
	checkRef, recordErr := g.Checks.Put(record)
	if recordErr != nil {
		return "", recordErr
	}
	preview := output
	if len(preview) > 8<<10 {
		preview = preview[:8<<10] + "\n…（完整输出见 output_ref）"
	}
	receipt, _ := json.Marshal(map[string]any{
		"schema": checkstore.SchemaV1, "check_ref": checkRef, "check_id": record.CheckID,
		"kind": record.Kind, "status": status, "exit_code": exitCode,
		"exit_code_scope": scope, "workspace_revision_ref": workspaceRef,
		"output_ref": contentRef.RefID, "preview": preview,
	})
	if shellErr != nil {
		return string(receipt), shellErr
	}
	return string(receipt), nil
}

func parseCheckShellResult(output string) (int, string) {
	exitCode := -1
	scope := ""
	for _, line := range strings.Split(output, "\n")[:min(3, len(strings.Split(output, "\n")))] {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "exit_code:"); ok {
			exitCode, _ = strconv.Atoi(strings.TrimSpace(value))
		}
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "exit_code_scope:"); ok {
			scope = strings.TrimSpace(value)
		}
	}
	return exitCode, scope
}
