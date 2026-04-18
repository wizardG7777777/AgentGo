package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// userAgent matches webtool.userAgent to ensure probe results reflect real tool behavior.
const userAgent = "Mozilla/5.0 (compatible; AgentGo/1.0)"

// NewWebSearchProbe creates a web_search health probe based on the configured search provider.
//
// Provider matching is case-insensitive with leading/trailing whitespace trimmed.
// An empty providerName returns a probe that reports web_search as unavailable
// (search provider not configured).
//
// Probe strategies by provider:
//   - "duckduckgo_html": HTTP GET https://duckduckgo.com/, verify 2xx
//   - "searxng":         HTTP GET {apiURL}, verify 2xx
//   - "tavily":          verify apiKey non-empty, then HTTP POST https://api.tavily.com/search
//   - "serper":          verify apiKey non-empty, then HTTP POST https://google.serper.dev/search
func NewWebSearchProbe(providerName, apiURL, apiKey string) Probe {
	provider := strings.ToLower(strings.TrimSpace(providerName))

	switch provider {
	case "duckduckgo_html":
		return duckduckgoProbe()
	case "searxng":
		return searxngProbe(apiURL)
	case "tavily":
		return tavilyProbe(apiKey)
	case "serper":
		return serperProbe(apiKey)
	case "":
		// Empty provider → report unavailable (provider not configured).
		return func(ctx context.Context) ProbeResult {
			return ProbeResult{
				Tool:      "web_search",
				Available: false,
				Error:     "搜索提供者未配置",
			}
		}
	default:
		// Unknown provider → report unavailable.
		return func(ctx context.Context) ProbeResult {
			return ProbeResult{
				Tool:      "web_search",
				Available: false,
				Error:     fmt.Sprintf("未知的搜索提供者: %s", provider),
			}
		}
	}
}

// duckduckgoProbe returns a probe that sends HTTP GET to https://duckduckgo.com/.
func duckduckgoProbe() Probe {
	return func(ctx context.Context) ProbeResult {
		start := time.Now()
		req, err := http.NewRequestWithContext(ctx, "GET", "https://duckduckgo.com/", nil)
		if err != nil {
			return ProbeResult{
				Tool:    "web_search",
				Error:   fmt.Sprintf("创建请求失败: %v", err),
				Latency: time.Since(start),
			}
		}
		req.Header.Set("User-Agent", userAgent)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return ProbeResult{
				Tool:    "web_search",
				Error:   fmt.Sprintf("请求失败: %v", err),
				Latency: time.Since(start),
			}
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return ProbeResult{
				Tool:      "web_search",
				Available: true,
				Latency:   time.Since(start),
			}
		}
		return ProbeResult{
			Tool:    "web_search",
			Error:   fmt.Sprintf("HTTP %d: %s", resp.StatusCode, resp.Status),
			Latency: time.Since(start),
		}
	}
}

// searxngProbe returns a probe that sends HTTP GET to the configured SearXNG base URL.
func searxngProbe(apiURL string) Probe {
	return func(ctx context.Context) ProbeResult {
		start := time.Now()
		if apiURL == "" {
			return ProbeResult{
				Tool:    "web_search",
				Error:   "search_api_url 未配置",
				Latency: time.Since(start),
			}
		}

		req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
		if err != nil {
			return ProbeResult{
				Tool:    "web_search",
				Error:   fmt.Sprintf("创建请求失败: %v", err),
				Latency: time.Since(start),
			}
		}
		req.Header.Set("User-Agent", userAgent)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return ProbeResult{
				Tool:    "web_search",
				Error:   fmt.Sprintf("请求失败: %v", err),
				Latency: time.Since(start),
			}
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return ProbeResult{
				Tool:      "web_search",
				Available: true,
				Latency:   time.Since(start),
			}
		}
		return ProbeResult{
			Tool:    "web_search",
			Error:   fmt.Sprintf("HTTP %d: %s", resp.StatusCode, resp.Status),
			Latency: time.Since(start),
		}
	}
}

// tavilyProbe returns a probe that verifies the Tavily API key is set and
// sends a minimal HTTP POST to https://api.tavily.com/search.
func tavilyProbe(apiKey string) Probe {
	return func(ctx context.Context) ProbeResult {
		start := time.Now()
		if apiKey == "" {
			return ProbeResult{
				Tool:    "web_search",
				Error:   "search_api_key 未配置",
				Latency: time.Since(start),
			}
		}

		body, _ := json.Marshal(map[string]any{
			"query":       "ping",
			"api_key":     apiKey,
			"max_results": 1,
		})
		req, err := http.NewRequestWithContext(ctx, "POST", "https://api.tavily.com/search", bytes.NewReader(body))
		if err != nil {
			return ProbeResult{
				Tool:    "web_search",
				Error:   fmt.Sprintf("创建请求失败: %v", err),
				Latency: time.Since(start),
			}
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", userAgent)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return ProbeResult{
				Tool:    "web_search",
				Error:   fmt.Sprintf("请求失败: %v", err),
				Latency: time.Since(start),
			}
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return ProbeResult{
				Tool:      "web_search",
				Available: true,
				Latency:   time.Since(start),
			}
		}
		return ProbeResult{
			Tool:    "web_search",
			Error:   fmt.Sprintf("HTTP %d: %s", resp.StatusCode, resp.Status),
			Latency: time.Since(start),
		}
	}
}

// serperProbe returns a probe that verifies the Serper API key is set and
// sends a minimal HTTP POST to https://google.serper.dev/search.
func serperProbe(apiKey string) Probe {
	return func(ctx context.Context) ProbeResult {
		start := time.Now()
		if apiKey == "" {
			return ProbeResult{
				Tool:    "web_search",
				Error:   "search_api_key 未配置",
				Latency: time.Since(start),
			}
		}

		body, _ := json.Marshal(map[string]any{"q": "ping"})
		req, err := http.NewRequestWithContext(ctx, "POST", "https://google.serper.dev/search", bytes.NewReader(body))
		if err != nil {
			return ProbeResult{
				Tool:    "web_search",
				Error:   fmt.Sprintf("创建请求失败: %v", err),
				Latency: time.Since(start),
			}
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-KEY", apiKey)
		req.Header.Set("User-Agent", userAgent)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return ProbeResult{
				Tool:    "web_search",
				Error:   fmt.Sprintf("请求失败: %v", err),
				Latency: time.Since(start),
			}
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return ProbeResult{
				Tool:      "web_search",
				Available: true,
				Latency:   time.Since(start),
			}
		}
		return ProbeResult{
			Tool:    "web_search",
			Error:   fmt.Sprintf("HTTP %d: %s", resp.StatusCode, resp.Status),
			Latency: time.Since(start),
		}
	}
}
