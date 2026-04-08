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

// extractDomain 从 URL 中提取域名部分（如 "techcrunch.com"）。
// 解析失败时返回空串。
func extractDomain(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	// 去掉 "www." 前缀
	if strings.HasPrefix(host, "www.") {
		host = host[4:]
	}
	return host
}

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

// allowLoopbackForTests 是仅供测试使用的开关。
// 当为 true 时，validateURL 不再拒绝 loopback / private / link-local 地址。
// 生产代码绝不应直接修改此变量；测试通过 AllowLoopbackForTests(t) 临时启用。
//
// 注意：该变量在并行测试中是全局共享的——避免对相同进程内的多个测试同时启用。
// AllowLoopbackForTests 通过 t.Cleanup 在测试结束时自动复位为 false。
var allowLoopbackForTests bool

// validateURL 解析 URL 并校验目标地址不是内网，防止 SSRF 攻击。
func validateURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("URL 解析失败: %w", err)
	}
	host := u.Hostname()

	// 直接 IP
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateOrLoopback(ip) && !allowLoopbackForTests {
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
		if isPrivateOrLoopback(ip) && !allowLoopbackForTests {
			return fmt.Errorf("拒绝访问内网地址: %s (解析到 %s)", rawURL, ip)
		}
	}
	return nil
}

// FetchURL 获取指定 URL 的页面文本内容（使用默认 auto 模式）。
func FetchURL(ctx context.Context, rawURL string) (string, error) {
	return FetchURLWithMode(ctx, rawURL, "auto")
}

// FetchURLWithMode 获取指定 URL 的页面文本内容，支持可选的内容提取模式。
// mode 可选值：
//   - "auto"（默认）：智能判断，有 <article>/<main> 则提取正文，否则全页面
//   - "article"：只提取正文区域（过滤导航/页脚噪音）
//   - "full"：全页面文本
func FetchURLWithMode(ctx context.Context, rawURL string, mode string) (string, error) {
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

	htmlStr := string(body)

	// 提取页面元数据（标题、发布时间）
	prefix := extractPageMeta(htmlStr, rawURL)

	var text string
	switch mode {
	case "article":
		text = ExtractArticle(htmlStr)
	case "full":
		text = ExtractText(htmlStr)
	default: // "auto"
		text = ExtractArticle(htmlStr)
	}

	if len(text) > maxOutputChars {
		text = text[:maxOutputChars] + "\n... (截断)"
	}
	if prefix != "" {
		return prefix + text, nil
	}
	return text, nil
}

// extractPageMeta 从 HTML 中提取结构化前缀（标题、发布时间、来源域名）。
func extractPageMeta(htmlStr, rawURL string) string {
	var sb strings.Builder

	// 提取 <title>
	titleRe := regexp.MustCompile(`(?i)<title[^>]*>(.*?)</title>`)
	if m := titleRe.FindStringSubmatch(htmlStr); len(m) > 1 {
		title := StripTags(m[1])
		if title != "" {
			sb.WriteString("[标题] " + title + "\n")
		}
	}

	// 尝试提取 og:article:published_time 或 datePublished
	pubRe := regexp.MustCompile(`(?i)(?:article:published_time|datePublished)[^>]*content="([^"]+)"`)
	if m := pubRe.FindStringSubmatch(htmlStr); len(m) > 1 {
		sb.WriteString("[发布] " + m[1] + "\n")
	}

	// 来源域名
	if domain := extractDomain(rawURL); domain != "" {
		sb.WriteString("[来源] " + domain + "\n")
	}

	if sb.Len() > 0 {
		return sb.String() + "---\n"
	}
	return ""
}

// ExtractArticle 从 HTML 中提取正文区域（<article>、<main>、role="main"）。
// 找不到语义标签时回退到全页面 ExtractText。
func ExtractArticle(htmlStr string) string {
	// 优先查找 <article> 标签
	articleRe := regexp.MustCompile(`(?is)<article[^>]*>(.*?)</article>`)
	if m := articleRe.FindStringSubmatch(htmlStr); len(m) > 1 {
		return ExtractText(m[1])
	}
	// 查找 <main> 标签
	mainRe := regexp.MustCompile(`(?is)<main[^>]*>(.*?)</main>`)
	if m := mainRe.FindStringSubmatch(htmlStr); len(m) > 1 {
		return ExtractText(m[1])
	}
	// 查找 role="main"
	roleRe := regexp.MustCompile(`(?is)<[^>]+role="main"[^>]*>(.*?)</(div|section|main)>`)
	if m := roleRe.FindStringSubmatch(htmlStr); len(m) > 1 {
		return ExtractText(m[1])
	}
	// 回退到全页面
	return ExtractText(htmlStr)
}

// SearchWeb 使用 DuckDuckGo HTML 搜索并返回格式化结果。
// 兼容性包装函数，内部使用 DuckDuckGoProvider。
// 新代码建议通过 SearchProvider 接口调用。
func SearchWeb(ctx context.Context, query string) (string, error) {
	provider := &DuckDuckGoProvider{}
	results, err := provider.Search(ctx, query, nil)
	if err != nil {
		return "", err
	}
	return FormatResults(results), nil
}

// SearchResult 表示一条搜索结果。
type SearchResult struct {
	Title       string
	URL         string
	Snippet     string
	PublishedAt string  // 发布时间（RFC3339 或近似字符串，后端不支持时为空串）
	Source      string  // 来源域名（如 "techcrunch.com"，后端不支持时为空串）
	Score       float64 // 相关性分数（0~1，后端不支持时为 0）
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
