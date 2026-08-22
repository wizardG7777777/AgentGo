package proposalacceptance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"agentgo/internal/graph"
	"agentgo/internal/llm"
)

const (
	maxIssueCode    = 64
	maxIssuePath    = 256
	maxIssueMessage = 512
)

const proposalVerdictToolName = "submit_proposal_verdict"

func proposalVerdictTool() llm.ToolDef {
	return llm.ToolDef{
		Name:        proposalVerdictToolName,
		Description: "Submit the only typed Graph Proposal Acceptance verdict. This schema records a decision and performs no side effect.",
		Parameters: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"verdict":    map[string]any{"type": "string", "enum": []string{"pass", "fixable", "blocked", "failed"}},
				"issue_code": map[string]any{"type": "string", "maxLength": maxIssueCode},
				"message":    map[string]any{"type": "string", "maxLength": maxIssueMessage},
			},
			"required": []string{"verdict"},
		},
	}
}

type verifierOutput struct {
	Verdict  string                  `json:"verdict"`
	Issues   []graph.ValidationIssue `json:"issues"`
	Warnings []graph.ValidationIssue `json:"warnings"`
}

type verifierOutputWire struct {
	Verdict   *string `json:"verdict"`
	IssueCode *string `json:"issue_code"`
	Message   *string `json:"message"`
}

func parseVerifierOutput(raw string, maxBytes int) (verifierOutput, error) {
	if maxBytes <= 0 {
		maxBytes = defaultMaxOutputBytes
	}
	if len([]byte(raw)) > maxBytes {
		return verifierOutput{}, fmt.Errorf("proposal verifier 输出超过 %d bytes", maxBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(raw)))
	decoder.DisallowUnknownFields()
	var wire verifierOutputWire
	if err := decoder.Decode(&wire); err != nil {
		return verifierOutput{}, fmt.Errorf("proposal verifier 输出不是严格 JSON object: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return verifierOutput{}, fmt.Errorf("proposal verifier 输出含多余 JSON 内容")
	}
	if wire.Verdict == nil {
		return verifierOutput{}, fmt.Errorf("proposal verifier 输出缺少 verdict 必填字段")
	}
	output := verifierOutput{Verdict: *wire.Verdict}
	output.Verdict = strings.TrimSpace(output.Verdict)
	switch output.Verdict {
	case string(graph.ProposalAcceptancePass), string(graph.ProposalAcceptanceFixable),
		string(graph.ProposalAcceptanceBlocked), string(graph.ProposalAcceptanceFailed):
	default:
		return verifierOutput{}, fmt.Errorf("proposal verifier verdict=%q 不在封闭词表", output.Verdict)
	}
	issueCode, message := "", ""
	if wire.IssueCode != nil {
		issueCode = *wire.IssueCode
	}
	if wire.Message != nil {
		message = *wire.Message
	}
	primary := graph.ValidationIssue{
		Code: issueCode, Retryable: output.Verdict == string(graph.ProposalAcceptanceFixable), Message: message,
	}
	if output.Verdict == string(graph.ProposalAcceptancePass) {
		if strings.TrimSpace(primary.Code)+strings.TrimSpace(primary.Path)+strings.TrimSpace(primary.Message) != "" || primary.Retryable {
			return verifierOutput{}, fmt.Errorf("pass verdict 的主 issue 字段必须为空")
		}
	} else {
		if err := normalizeIssue(&primary); err != nil {
			return verifierOutput{}, fmt.Errorf("主 issue 无效: %w", err)
		}
		output.Issues = []graph.ValidationIssue{primary}
	}
	return output, nil
}

func parseVerifierArguments(arguments map[string]any, maxBytes int) (verifierOutput, error) {
	raw, err := json.Marshal(arguments)
	if err != nil {
		return verifierOutput{}, fmt.Errorf("proposal verifier verdict tool 参数无法编码: %w", err)
	}
	return parseVerifierOutput(string(raw), maxBytes)
}

func normalizeIssue(issue *graph.ValidationIssue) error {
	if issue == nil {
		return fmt.Errorf("issue 为空")
	}
	issue.Code = strings.TrimSpace(issue.Code)
	issue.Path = strings.TrimSpace(issue.Path)
	issue.Message = strings.TrimSpace(issue.Message)
	if !validIssueCode(issue.Code) {
		return fmt.Errorf("code=%q 非 UPPER_SNAKE_CASE 或超限", issue.Code)
	}
	if utf8.RuneCountInString(issue.Path) > maxIssuePath || containsControl(issue.Path) {
		return fmt.Errorf("path 超限或含控制字符")
	}
	if issue.Message == "" || utf8.RuneCountInString(issue.Message) > maxIssueMessage || containsControl(issue.Message) {
		return fmt.Errorf("message 为空、超限或含控制字符")
	}
	return nil
}

func validIssueCode(code string) bool {
	if code == "" || utf8.RuneCountInString(code) > maxIssueCode {
		return false
	}
	for index, r := range code {
		if index == 0 {
			if r < 'A' || r > 'Z' {
				return false
			}
			continue
		}
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

func containsControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
