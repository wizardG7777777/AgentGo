package llm

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
)

func contentDataURL(p ContentPart) string {
	return "data:" + p.MediaType + ";base64," + base64.StdEncoding.EncodeToString(p.Data)
}

func contentWire(parts []ContentPart, protocol Protocol) []map[string]any {
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		var v map[string]any
		switch p.Kind {
		case "text":
			kind := "text"
			if protocol == ProtocolResponses {
				kind = "input_text"
			}
			v = map[string]any{"type": kind, "text": p.Text}
		case "image":
			url := p.URL
			if p.Data != nil {
				url = contentDataURL(p)
			}
			detail := p.Detail
			if detail == "" {
				detail = "auto"
			}
			if protocol == ProtocolResponses {
				v = map[string]any{"type": "input_image", "image_url": url, "detail": detail}
			} else {
				v = map[string]any{"type": "image_url", "image_url": map[string]any{"url": url, "detail": detail}}
			}
		case "file":
			file := map[string]any{}
			if p.FileID != "" {
				file["file_id"] = p.FileID
			}
			if p.URL != "" {
				file["file_url"] = p.URL
			}
			if p.Data != nil {
				file["file_data"] = contentDataURL(p)
				file["filename"] = p.Filename
			}
			if protocol == ProtocolResponses {
				file["type"] = "input_file"
				v = file
			} else {
				v = map[string]any{"type": "file", "file": file}
			}
		}
		out = append(out, v)
	}
	return out
}

func chatParts(m Message) (openai.ChatCompletionMessageParamUnion, error) {
	raw, err := json.Marshal(map[string]any{"role": m.Role, "content": contentWire(m.Parts, ProtocolChatCompletions)})
	if err != nil {
		return openai.ChatCompletionMessageParamUnion{}, err
	}
	return param.Override[openai.ChatCompletionMessageParamUnion](json.RawMessage(raw)), nil
}
func responseParts(m Message) (responses.ResponseInputItemUnionParam, error) {
	raw, err := json.Marshal(map[string]any{"type": "message", "role": m.Role, "content": contentWire(m.Parts, ProtocolResponses)})
	if err != nil {
		return responses.ResponseInputItemUnionParam{}, err
	}
	return param.Override[responses.ResponseInputItemUnionParam](json.RawMessage(raw)), nil
}

// MeasureRequest 使用真实协议编码器测量，不通过文本长度猜测二进制请求大小。
func MeasureRequest(spec RequestSpec) (int64, error) {
	var raw []byte
	var err error
	switch spec.Options.Protocol {
	case ProtocolResponses:
		p, e := responsesParams(spec.Options, spec.Messages, spec.Tools)
		if e != nil {
			return 0, e
		}
		raw, err = json.Marshal(p)
	case ProtocolChatCompletions:
		p, e := chatParams(spec.Options, spec.Messages, spec.Tools)
		if e != nil {
			return 0, e
		}
		raw, err = json.Marshal(p)
	default:
		return 0, fmt.Errorf("未知请求协议")
	}
	if err != nil {
		return 0, err
	}
	// SDK 的 NewStreaming 在同一对象追加 stream:true。
	return int64(len(raw) + len(`,"stream":true`)), nil
}
