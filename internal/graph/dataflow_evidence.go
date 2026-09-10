package graph

// 证据是普通任务结果的一部分，不授予某类节点特殊执行权限。
const (
	EvidenceIdentityMaxRunes = 512
	EvidenceSummaryMaxRunes  = 200
	EvidenceCommandMaxRunes  = 4096
	EvidencePathMaxRunes     = 4096
	InputInlineMaxBytes      = 32 << 10
)

type EvidenceEntry struct {
	Ref                  string `json:"ref"`
	Kind                 string `json:"kind"`
	Summary              string `json:"summary,omitempty"`
	CallID               string `json:"call_id,omitempty"`
	ToolName             string `json:"tool_name,omitempty"`
	Success              *bool  `json:"success,omitempty"`
	Command              string `json:"command,omitempty"`
	CommandTruncated     bool   `json:"command_truncated,omitempty"`
	ExitCode             *int   `json:"exit_code,omitempty"`
	ExitCodeScope        string `json:"exit_code_scope,omitempty"`
	Path                 string `json:"path,omitempty"`
	PathTruncated        bool   `json:"path_truncated,omitempty"`
	WorkspaceRevisionRef string `json:"workspace_revision_ref,omitempty"`
	OutputRef            string `json:"output_ref,omitempty"`
}
