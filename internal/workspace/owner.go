package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const ownerSchemaV1 = "agentgo.workspace-owner/v1"
const ownerFileName = ".workspace-owner.json"

type OwnerKind string

const (
	OwnerTask     OwnerKind = "task"
	OwnerDelivery OwnerKind = "delivery"
)

// Owner 是 workspace 生命周期的持久化所有权。目录名只是跨平台物理身份，
// Watchdog 不得再把 delivery-* 目录名猜成 TaskID。
type Owner struct {
	Schema     string    `json:"schema"`
	Kind       OwnerKind `json:"kind"`
	TaskID     string    `json:"task_id,omitempty"`
	DeliveryID string    `json:"delivery_id,omitempty"`
	RunID      string    `json:"run_id,omitempty"`
	GraphID    string    `json:"graph_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type Record struct {
	WorkspaceID string `json:"workspace_id"`
	Owner       Owner  `json:"owner"`
	Legacy      bool   `json:"legacy,omitempty"`
}

func TaskOwner(taskID string) Owner {
	return Owner{Schema: ownerSchemaV1, Kind: OwnerTask, TaskID: taskID, CreatedAt: time.Now().UTC()}
}

func DeliveryOwner(taskID, deliveryID, runID, graphID string) Owner {
	return Owner{Schema: ownerSchemaV1, Kind: OwnerDelivery, TaskID: taskID,
		DeliveryID: deliveryID, RunID: runID, GraphID: graphID, CreatedAt: time.Now().UTC()}
}

func (o Owner) validate(workspaceID string) error {
	if o.Schema != ownerSchemaV1 || o.CreatedAt.IsZero() {
		return fmt.Errorf("workspace owner schema/created_at 无效")
	}
	switch o.Kind {
	case OwnerTask:
		if strings.TrimSpace(o.TaskID) == "" || workspaceID != o.TaskID || o.DeliveryID != "" || o.GraphID != "" {
			return fmt.Errorf("task workspace owner identity 不一致")
		}
	case OwnerDelivery:
		if strings.TrimSpace(o.DeliveryID) == "" || strings.TrimSpace(o.RunID) == "" ||
			strings.TrimSpace(o.GraphID) == "" || workspaceID != DeliveryWorkspaceID(o.DeliveryID) {
			return fmt.Errorf("delivery workspace owner identity 不一致")
		}
	default:
		return fmt.Errorf("workspace owner kind=%q 无效", o.Kind)
	}
	return nil
}

func sameOwner(left, right Owner) bool {
	if left.Schema != right.Schema || left.Kind != right.Kind {
		return false
	}
	if left.Kind == OwnerDelivery {
		// repair/acceptance activation 会换 TaskID，但必须复用同一
		// Run/Graph/Delivery。TaskID 是当前使用者，不是 workspace 所有权。
		return left.DeliveryID == right.DeliveryID && left.RunID == right.RunID && left.GraphID == right.GraphID
	}
	return left.TaskID == right.TaskID
}

func loadOwner(root string) (Owner, error) {
	data, err := os.ReadFile(filepath.Join(root, ownerFileName))
	if err != nil {
		return Owner{}, err
	}
	var owner Owner
	if err := json.Unmarshal(data, &owner); err != nil {
		return Owner{}, fmt.Errorf("解析 workspace owner: %w", err)
	}
	return owner, nil
}

func persistOwner(root, workspaceID string, owner Owner) error {
	if err := owner.validate(workspaceID); err != nil {
		return err
	}
	path := filepath.Join(root, ownerFileName)
	if existing, err := loadOwner(root); err == nil {
		if !sameOwner(existing, owner) {
			return fmt.Errorf("workspace %s 已绑定另一 owner", workspaceID)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	encoded, err := json.MarshalIndent(owner, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(root, ".workspace-owner-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpPath) }
	if _, err = tmp.Write(encoded); err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil {
		cleanup()
		return err
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return closeErr
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}
