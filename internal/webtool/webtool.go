package webtool

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	defaultTimeout   = 30 * time.Second
	maxResponseBytes = 1 << 20 // 1MB
	maxOutputChars   = 10000
)

// userAgent 避免被简单的 bot 检测拦截
const userAgent = "Mozilla/5.0 (compatible; AgentGo/1.0)"

// isPrivateOrLoopback 检查 IP 是否为内网、环回或链路本地地址。
func isPrivateOrLoopback(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	// 169.254.x.x (AWS metadata endpoint 等链路本地地址)
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 169 && ip4[1] == 254 {
		return true
	}
	return false
}

// validateURL 解析 URL 并校验目标地址不是内网，防止 SSRF 攻击。
func validateURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("URL 解析失败: %w", err)
	}
	host := u.Hostname()

	// 直接 IP
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateOrLoopback(ip) {
			return fmt.Errorf("拒绝访问内网地址: %s", rawURL)
		}
		return nil
	}

	// 域名解析
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil // DNS 解析失败时放行，让 HTTP 请求自己报错
	}
	for _, ip := range ips {
		if isPrivateOrLoopback(ip) {
			return fmt.Errorf("拒绝访问内网地址: %s (解析到 %s)", rawURL, ip)
		}
	}
	return nil
}

// FetchURL 获取指定 URL 的页面文本内容。
func FetchURL(ctx context.Context, rawURL string) (string, error) {
	if rawURL == "" {
		return "", fmt.Errorf("缺少 url 参数")
	}
	// 基础 URL 校验
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		rawURL = "https://" + rawURL
	}

	// SSRF 防护：校验目标地址不是内网
	if err := validateURL(rawURL); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)

	client := &http.Client{Timeout: defaultTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	// 限制读取大小
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", fmt.Errorf("读取响应失败: %w", err)
	}

	text := ExtractText(string(body))
	if len(text) > maxOutputChars {
		text = text[:maxOutputChars] + "\n... (截断)"
	}
	return text, nil
}

// SearchWeb 使用 DuckDuckGo HTML 搜索并返回格式化结果。
// 兼容性包装函数，内部使用 DuckDuckGoProvider。
// 新代码建议通过 SearchProvider 接口调用。
func SearchWeb(ctx context.Context, query string) (string, error) {
	provider := &DuckDuckGoProvider{}
	results, err := provider.Search(ctx, query)
	if err != nil {
		return "", err
	}
	return FormatResults(results), nil
}

// SearchResult 表示一条搜索结果。
type SearchResult struct {
	Title   string
	URL     string
	Snippet string
}

// ParseSearchResults 从 DuckDuckGo HTML 响应中提取搜索结果。
// DuckDuckGo HTML 页面的结果链接格式: <a rel="nofollow" class="result__a" href="...">Title</a>
// 摘要在 <a class="result__snippet" ...>...</a>
func ParseSearchResults(htmlStr string) []SearchResult {
	var results []SearchResult

	// 匹配 result__a 链接（标题 + URL）
	titleRe := regexp.MustCompile(`<a[^>]*class="result__a"[^>]*href="([^"]*)"[^>]*>(.*?)</a>`)
	snippetRe := regexp.MustCompile(`<a[^>]*class="result__snippet"[^>]*>(.*?)</a>`)

	titleMatches := titleRe.FindAllStringSubmatch(htmlStr, 10)
	snippetMatches := snippetRe.FindAllStringSubmatch(htmlStr, 10)

	for i, m := range titleMatches {
		r := SearchResult{
			URL:   StripTags(m[1]),
			Title: StripTags(m[2]),
		}
		if i < len(snippetMatches) {
			r.Snippet = StripTags(snippetMatches[i][1])
		}
		results = append(results, r)
	}

	return results
}

// ExtractText 从 HTML 中提取可见文本。
var tagRe = regexp.MustCompile(`<[^>]*>`)
var spaceRe = regexp.MustCompile(`\s+`)

func ExtractText(htmlStr string) string {
	// 移除 script 和 style 标签及内容
	scriptRe := regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	styleRe := regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	text := scriptRe.ReplaceAllString(htmlStr, "")
	text = styleRe.ReplaceAllString(text, "")
	// 移除所有 HTML 标签
	text = tagRe.ReplaceAllString(text, " ")
	// HTML 实体解码（常见的）
	text = strings.ReplaceAll(text, "&amp;", "&")
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	text = strings.ReplaceAll(text, "&quot;", "\"")
	text = strings.ReplaceAll(text, "&#39;", "'")
	text = strings.ReplaceAll(text, "&nbsp;", " ")
	// 压缩空白
	text = spaceRe.ReplaceAllString(text, " ")
	return strings.TrimSpace(text)
}

// StripTags 移除字符串中的 HTML 标签。
func StripTags(s string) string {
	return strings.TrimSpace(tagRe.ReplaceAllString(s, ""))
}
