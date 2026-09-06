package contextruntime

import (
	"context"
	"fmt"

	"agentgo/internal/contextcontract"
	"agentgo/internal/llm"
)

// InputContentReader 是 L3 授权读取端口；L2 不解析磁盘路径或自行放宽权限。
type InputContentReader interface {
	ReadModelInput(context.Context, llm.Identity, string, int64) ([]byte, error)
}
type InputReference struct {
	SourceRef string
	Part      llm.ContentPart
}

func (r Runtime) materializeInputs(ctx context.Context, in *Input) error {
	if len(in.References) == 0 {
		return nil
	}
	if r.InputReader == nil {
		return fmt.Errorf("非文本引用缺少 L3 授权读取端口")
	}
	cap := in.Options.InputCapability
	if err := cap.Validate(); err != nil {
		return err
	}
	for _, ref := range in.References {
		part := ref.Part
		if part.Kind == "image" && (!cap.Images || cap.TokensPerImage <= 0) || part.Kind == "file" && (!cap.Files || cap.TokensPerFile <= 0) {
			return fmt.Errorf("模型未声明引用内容能力或预算")
		}
		if part.Kind != "image" && part.Kind != "file" || ref.SourceRef == "" || cap.MaxPartBytes <= 0 {
			return fmt.Errorf("非文本引用契约无效")
		}
		if part.Data != nil || part.URL != "" || part.FileID != "" {
			return fmt.Errorf("内容引用不能同时指定其它来源")
		}
		data, err := r.InputReader.ReadModelInput(ctx, in.Identity, ref.SourceRef, cap.MaxPartBytes)
		if err != nil {
			return err
		}
		part.Data = append([]byte(nil), data...)
		in.Conversation = append(in.Conversation, ConversationItem{Message: &MessageBinding{Message: llm.Message{Role: "user", Parts: []llm.ContentPart{part}}, Kind: contextcontract.FragmentUserMedia, Section: contextcontract.SectionInputMedia, SourceRef: ref.SourceRef, Scope: contextcontract.ScopeTask, Authority: contextcontract.AuthorityInformational, Freshness: contextcontract.FreshnessSnapshot}})
	}
	return nil
}

// mediaBudget 在独立媒体分区计费，不扩大普通 Prompt/历史片段上限。
func mediaBudget(conversation []ConversationItem, options llm.Options, policy *contextcontract.ContextBudgetPolicy) ([]ConversationItem, error) {
	out := append([]ConversationItem(nil), conversation...)
	var tokens int64
	for i, item := range out {
		if item.Message == nil {
			continue
		}
		binding := *item.Message
		media := false
		for _, part := range binding.Message.Parts {
			switch part.Kind {
			case "image":
				media = true
				tokens += options.InputCapability.TokensPerImage
			case "file":
				media = true
				tokens += options.InputCapability.TokensPerFile
			case "text":
				tokens += int64(len([]rune(part.Text)))
			}
		}
		if media {
			binding.Kind = contextcontract.FragmentUserMedia
			binding.Section = contextcontract.SectionInputMedia
			out[i].Message = &binding
		}
	}
	if tokens == 0 {
		return out, nil
	}
	cap := options.InputCapability
	if cap.MaxTotalBytes <= 0 || cap.MaxTotalBytes > policy.SnapshotInputBudget.SerializedBytes {
		return nil, fmt.Errorf("媒体字节预算必须位于当前 Context 总预算内")
	}
	if tokens > policy.SnapshotInputBudget.EstimatedTokens {
		return nil, fmt.Errorf("媒体 token 预算超过当前 Context")
	}
	bytes := cap.MaxTotalBytes*4/3 + 4096
	policy.FragmentRules[contextcontract.FragmentUserMedia] = contextcontract.FragmentBudgetRule{MaxSerializedBytes: bytes, MaxEstimatedTokens: tokens, AllowedDispositions: []contextcontract.Disposition{contextcontract.DispositionInline, contextcontract.DispositionRejected}, RetentionClass: contextcontract.RetentionEphemeralRequest, Priority: 100}
	policy.SectionBudgets[contextcontract.SectionInputMedia] = contextcontract.Budget{SerializedBytes: bytes, EstimatedTokens: tokens}
	return out, nil
}
