package contextruntime_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"agentgo/internal/contextcontract"
	"agentgo/internal/contextruntime"
	"agentgo/internal/llm"
	"agentgo/internal/testmodel"
)

type inputReader struct {
	calls int
	data  []byte
	deny  bool
}

func (r *inputReader) ReadModelInput(ctx context.Context, id llm.Identity, ref string, max int64) ([]byte, error) {
	r.calls++
	if r.deny || id.TaskID != "task-1" || ref != "authorized-ref" {
		return nil, errors.New("L3 引用权限拒绝")
	}
	return r.data, nil
}
func mediaCapabilities() llm.InputCapability {
	return llm.InputCapability{Images: true, Files: true, MaxPartBytes: 512 << 10, MaxTotalBytes: 512 << 10, TokensPerImage: 1024, TokensPerFile: 4096}
}

func TestMediaUsesSeparateBudgetAndPreservesWholeContent(t *testing.T) {
	for _, protocol := range []llm.Protocol{llm.ProtocolResponses, llm.ProtocolChatCompletions} {
		t.Run(string(protocol), func(t *testing.T) {
			runtime := testmodel.Runtime(t)
			input := inputFixture()
			input.Options.Protocol = protocol
			input.Options.InputCapability = mediaCapabilities()
			data := bytes.Repeat([]byte("附件数据\n"), 16000)
			input.Conversation = []contextruntime.ConversationItem{{Message: &contextruntime.MessageBinding{Message: llm.Message{Role: "user", Parts: []llm.ContentPart{{Kind: "text", Text: "检查附件"}, {Kind: "file", Data: data, Filename: "data.txt", MediaType: "text/plain"}}}, Kind: contextcontract.FragmentUserTask, Section: contextcontract.SectionTaskContract, SourceRef: "attachment-1", Scope: contextcontract.ScopeTask, Authority: contextcontract.AuthorityInformational, Freshness: contextcontract.FreshnessSnapshot}}}
			compiled, err := runtime.Compile(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			messages := compiled.Request().Spec().Messages
			last := messages[len(messages)-1]
			if len(last.Parts) != 2 || !bytes.Equal(last.Parts[1].Data, data) {
				t.Fatal("媒体内容被裁剪或丢失")
			}
			found := false
			for _, fragment := range compiled.Snapshot().Fragments {
				if fragment.Kind == contextcontract.FragmentUserMedia {
					found = true
					if fragment.EstimatedTokens > 4200 {
						t.Fatal("二进制被误当普通文本估算")
					}
				}
			}
			if !found {
				t.Fatal("缺少独立媒体预算分区")
			}
		})
	}
}

func TestMediaReferencesRequireInjectedL3Authority(t *testing.T) {
	r := testmodel.Runtime(t)
	input := inputFixture()
	input.Options.InputCapability = mediaCapabilities()
	input.References = []contextruntime.InputReference{{SourceRef: "authorized-ref", Part: llm.ContentPart{Kind: "file", Filename: "data.txt", MediaType: "text/plain"}}}
	if _, err := r.Compile(context.Background(), input); err == nil {
		t.Fatal("没有 L3 读取端口却读取引用")
	}
	reader := &inputReader{data: []byte("已授权内容"), deny: true}
	r.InputReader = reader
	if _, err := r.Compile(context.Background(), input); err == nil {
		t.Fatal("忽略 L3 引用拒绝")
	}
	reader.deny = false
	if _, err := r.Compile(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	input.Options.InputCapability = llm.InputCapability{}
	before := reader.calls
	if _, err := r.Compile(context.Background(), input); err == nil || reader.calls != before {
		t.Fatal("缺少媒体能力时仍读取引用")
	}
}
