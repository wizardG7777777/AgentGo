package executionfacts

import (
	"agentgo/internal/model"
	"agentgo/internal/store"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type WorkspaceRevisionResolver interface {
	ResolveWorkspaceRevision(task *model.Task, taskStore store.TaskStore) (
		ref string, effectRefs []string, handled bool, err error,
	)
}

// WorkspaceRevision 优先从注入的 Delivery workspace authority 计算
// 累积 candidate revision；普通 Task 仍从当前 Attempt 的 settled
// write/edit ToolCall 构造稳定版本。
func WorkspaceRevision(task *model.Task, taskStore store.TaskStore,
	resolvers ...WorkspaceRevisionResolver) (string, []string, error) {
	if task == nil || taskStore == nil || task.AttemptID == "" {
		return "", nil, fmt.Errorf("workspace revision 缺少 Task/Store/Attempt")
	}
	for _, resolver := range resolvers {
		if resolver == nil {
			continue
		}
		ref, effectRefs, handled, err := resolver.ResolveWorkspaceRevision(task, taskStore)
		if err != nil || handled {
			return ref, effectRefs, err
		}
	}
	records, err := taskStore.QueryToolCalls(task.ID, "")
	if err != nil {
		return "", nil, err
	}
	var identities []string
	for _, record := range records {
		if record.AttemptID != task.AttemptID || !record.Success ||
			(record.ToolName != "apply_change") {
			continue
		}
		encoded, _ := json.Marshal(record.Args)
		identities = append(identities, record.CallID+"\x00"+record.ToolName+"\x00"+string(encoded))
	}
	sort.Strings(identities)
	if len(identities) == 0 {
		return "workspace:empty", nil, nil
	}
	sum := sha256.Sum256([]byte(strings.Join(identities, "\x00")))
	refs := make([]string, 0, len(identities))
	for _, identity := range identities {
		callID, _, _ := strings.Cut(identity, "\x00")
		refs = append(refs, "tool-call:"+callID)
	}
	return "workspace:sha256:" + hex.EncodeToString(sum[:]), refs, nil
}
