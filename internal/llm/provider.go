package llm

import (
	"sync"

	"github.com/openai/openai-go/v3/option"
)

// Provider 封装各 LLM 后端的非标行为。
//
// 层 1（ExtraFields 通用透传）已经覆盖了"保留即可"类的扩展（DeepSeek V4 的
// reasoning_content 等）——provider 只需要处理 "变换型/负向" 差异：
//
//   - PrepareMessages：在发请求前改造 history。例如 DeepSeek R1 要求删除所有
//     assistant 消息里的 reasoning_content；Qwen QwQ 要求从 content 里拆出
//     <think> 标签。
//   - RequestOptions：返回该 provider 特有的 RequestOption（通常用
//     option.WithJSONSet 往请求 body 注入非标字段）。
//
// 新增模型家族 = 实现本接口 + 在 init() 里 RegisterProvider() 一行，
// 不需要改 client.go / agent.go 任何其他位置。
type Provider interface {
	Name() string
	PrepareMessages(history []Message) []Message
	RequestOptions() []option.RequestOption
}

var (
	providerRegistryMu sync.RWMutex
	providerRegistry   = map[string]Provider{}
)

// RegisterProvider 注册一个 Provider。重复名称会被覆盖。
// 通常在各 provider 文件的 init() 里调用。
func RegisterProvider(p Provider) {
	providerRegistryMu.Lock()
	defer providerRegistryMu.Unlock()
	providerRegistry[p.Name()] = p
}

// GetProvider 按名称查找 Provider。
// 名称为空或未知时返回 OpenAIProvider，即 no-op 行为，
// 保证向后兼容：旧配置（没有 llm_provider 字段）行为不变。
func GetProvider(name string) Provider {
	if name == "" {
		return &OpenAIProvider{}
	}
	providerRegistryMu.RLock()
	defer providerRegistryMu.RUnlock()
	if p, ok := providerRegistry[name]; ok {
		return p
	}
	return &OpenAIProvider{}
}

// RegisteredProviders 返回所有已注册的 provider 名称，便于启动时日志打印。
// 顺序不稳定。
func RegisteredProviders() []string {
	providerRegistryMu.RLock()
	defer providerRegistryMu.RUnlock()
	out := make([]string, 0, len(providerRegistry))
	for k := range providerRegistry {
		out = append(out, k)
	}
	return out
}
