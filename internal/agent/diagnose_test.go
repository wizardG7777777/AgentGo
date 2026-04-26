package agent

import (
	"errors"
	"strings"
	"testing"

	"agentgo/internal/llm"
)

func TestDiagnoseLLMError_ModelNotFound(t *testing.T) {
	execErr := &llm.ErrUnrecoverable{
		Err:        errors.New("400 bad request"),
		StatusCode: 400,
		Code:       "model_not_found",
		Message:    "The model does not exist",
		Endpoint:   "https://api.deepseek.com/v1",
	}
	got := diagnoseLLMError(execErr, nil, "gpt-5")
	if !strings.Contains(got, "模型名") {
		t.Errorf("expected '模型名' in diagnosis, got: %s", got)
	}
	if !strings.Contains(got, "gpt-5") {
		t.Errorf("expected model name 'gpt-5' in diagnosis, got: %s", got)
	}
	if !strings.Contains(got, "api.deepseek.com") {
		t.Errorf("expected endpoint in diagnosis, got: %s", got)
	}
}

func TestDiagnoseLLMError_InvalidAPIKey(t *testing.T) {
	execErr := &llm.ErrUnrecoverable{
		Err:        errors.New("401 unauthorized"),
		StatusCode: 401,
		Code:       "invalid_api_key",
		Message:    "Incorrect API key",
	}
	got := diagnoseLLMError(execErr, nil, "gpt-4")
	if !strings.Contains(got, "API key 无效") {
		t.Errorf("expected 'API key 无效' in diagnosis, got: %s", got)
	}
}

func TestDiagnoseLLMError_InsufficientQuota(t *testing.T) {
	execErr := &llm.ErrUnrecoverable{
		Err:        errors.New("429 rate limit"),
		StatusCode: 429,
		Code:       "insufficient_quota",
		Message:    "You exceeded your current quota",
	}
	got := diagnoseLLMError(execErr, nil, "gpt-4")
	if !strings.Contains(got, "配额不足") {
		t.Errorf("expected '配额不足' in diagnosis, got: %s", got)
	}
}

func TestDiagnoseLLMError_ContextLengthExceeded(t *testing.T) {
	execErr := &llm.ErrUnrecoverable{
		Err:        errors.New("413 request entity too large"),
		StatusCode: 413,
		Code:       "context_length_exceeded",
		Message:    "This model's maximum context length is 8192 tokens",
	}
	history := []HistoryEntry{
		{AssistantContent: strings.Repeat("a", 3000)},
		{Output: strings.Repeat("b", 3000)},
	}
	got := diagnoseLLMError(execErr, history, "gpt-4")
	if !strings.Contains(got, "上下文上限") {
		t.Errorf("expected '上下文上限' in diagnosis, got: %s", got)
	}
	if !strings.Contains(got, "tokens") {
		t.Errorf("expected token count in diagnosis, got: %s", got)
	}
}

func TestDiagnoseLLMError_404Endpoint(t *testing.T) {
	execErr := &llm.ErrUnrecoverable{
		Err:        errors.New("404 not found"),
		StatusCode: 404,
		Code:       "",
		Message:    "Not Found",
		Endpoint:   "https://api.deepseek.com/v1/chat/completions",
	}
	got := diagnoseLLMError(execErr, nil, "gpt-4")
	if !strings.Contains(got, "端点返回 404") {
		t.Errorf("expected '端点返回 404' in diagnosis, got: %s", got)
	}
}

func TestDiagnoseLLMError_404Host(t *testing.T) {
	execErr := &llm.ErrUnrecoverable{
		Err:        errors.New("404 not found"),
		StatusCode: 404,
		Code:       "",
		Message:    "Not Found",
		Endpoint:   "https://invalid.example.com/v1",
	}
	got := diagnoseLLMError(execErr, nil, "gpt-4")
	if !strings.Contains(got, "无法连接到") {
		t.Errorf("expected '无法连接到' in diagnosis, got: %s", got)
	}
}

func TestDiagnoseLLMError_Fallback(t *testing.T) {
	execErr := &llm.ErrUnrecoverable{
		Err:        errors.New("418 i'm a teapot"),
		StatusCode: 418,
		Code:       "teapot",
		Message:    "I'm a teapot",
	}
	got := diagnoseLLMError(execErr, nil, "gpt-4")
	if !strings.Contains(got, "LLM 调用失败") {
		t.Errorf("expected 'LLM 调用失败' fallback, got: %s", got)
	}
	if !strings.Contains(got, "teapot") {
		t.Errorf("expected code in fallback, got: %s", got)
	}
}

func TestDiagnoseLLMError_NonLLMError(t *testing.T) {
	plain := errors.New("some random error")
	got := diagnoseLLMError(plain, nil, "gpt-4")
	if got != plain.Error() {
		t.Errorf("expected plain error unchanged, got: %s", got)
	}
}

// TestDiagnoseLLMError_ModelNotFound_MessageFallback 守的是 §9.4 第 1 条规则的
// 兜底分支："code == 'model_not_found' 或 message 含 'model'+'not found'"。
//
// Code 字段在 OpenAI 标准响应里有，但部分 provider（兼容 API、自建网关）可能不填，
// 只在 message 里写"The model X does not exist"。这条兜底确保：即便 Code 缺失，
// 只要 message 同时含 "model" 与 "not found"（小写比较），仍触发模型名诊断。
//
// 若有人重构 case 表达式漏掉这条 OR，本测试会立即变红。
func TestDiagnoseLLMError_ModelNotFound_MessageFallback(t *testing.T) {
	execErr := &llm.ErrUnrecoverable{
		Err:        errors.New("400 bad request"),
		StatusCode: 400,
		Code:       "", // 关键：不填 Code，只靠 message 兜底
		Message:    "The model `gpt-9` does not exist or you do not have access to it. Not Found.",
		Endpoint:   "https://example.com/v1",
	}
	got := diagnoseLLMError(execErr, nil, "gpt-9")
	if !strings.Contains(got, "模型名") {
		t.Errorf("message 含 'model'+'not found' 时应触发模型名诊断（兜底分支），got: %s", got)
	}
	if !strings.Contains(got, "gpt-9") {
		t.Errorf("expected model name 'gpt-9' in diagnosis, got: %s", got)
	}
}
