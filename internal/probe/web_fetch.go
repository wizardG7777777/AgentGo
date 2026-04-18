package probe

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// DefaultFetchProbeURL is the default probe target for web_fetch health checks.
// Chosen for high availability, small response body, and no geo-restrictions.
const DefaultFetchProbeURL = "https://www.google.com/robots.txt"

// allowLoopbackForTests is a test-only flag. When true, the SSRF validation
// in this package does not reject loopback/private/link-local addresses,
// allowing httptest.NewServer (bound to 127.0.0.1) to be used in tests.
//
// Production code must never modify this variable directly; tests use
// AllowLoopbackForTests(t) which resets via t.Cleanup.
var allowLoopbackForTests bool

// AllowLoopbackForTests temporarily disables the SSRF loopback/private/link-local
// rejection in this package so that httptest servers on 127.0.0.1 are reachable.
//
// Usage:
//
//	func TestSomething(t *testing.T) {
//	    probe.AllowLoopbackForTests(t)
//	    srv := httptest.NewServer(handler)
//	    defer srv.Close()
//	    // probe functions can now reach srv.URL without SSRF rejection
//	}
//
// The flag is automatically reset when the test finishes (via t.Cleanup).
// Do not use with t.Parallel — the flag is a package-level global.
func AllowLoopbackForTests(t interface {
	Helper()
	Cleanup(func())
}) {
	t.Helper()
	allowLoopbackForTests = true
	t.Cleanup(func() { allowLoopbackForTests = false })
}

// NewWebFetchProbe creates a web_fetch health probe that verifies outbound
// HTTP connectivity. An empty probeURL defaults to DefaultFetchProbeURL.
//
// The probe uses HTTP HEAD first; if the server returns 405 Method Not Allowed,
// it falls back to GET. Status codes 2xx and 3xx are treated as available.
//
// SSRF protection mirrors webtool.validateURL: private, loopback, and
// link-local addresses are rejected unless allowLoopbackForTests is set.
func NewWebFetchProbe(probeURL string) Probe {
	if probeURL == "" {
		probeURL = DefaultFetchProbeURL
	}

	return func(ctx context.Context) ProbeResult {
		start := time.Now()

		// SSRF validation before making any HTTP request.
		if err := validateFetchURL(probeURL); err != nil {
			return ProbeResult{
				Tool:    "web_fetch",
				Error:   fmt.Sprintf("SSRF 拒绝: %v", err),
				Latency: time.Since(start),
			}
		}

		// Try HEAD first.
		result := doFetchRequest(ctx, "HEAD", probeURL, start)
		// If HEAD returns 405 Method Not Allowed, fall back to GET.
		if !result.Available && result.Error != "" &&
			containsStatusCode(result.Error, http.StatusMethodNotAllowed) {
			result = doFetchRequest(ctx, "GET", probeURL, start)
		}
		return result
	}
}

// doFetchRequest performs a single HTTP request and returns a ProbeResult for web_fetch.
func doFetchRequest(ctx context.Context, method, targetURL string, start time.Time) ProbeResult {
	req, err := http.NewRequestWithContext(ctx, method, targetURL, nil)
	if err != nil {
		return ProbeResult{
			Tool:    "web_fetch",
			Error:   fmt.Sprintf("创建请求失败: %v", err),
			Latency: time.Since(start),
		}
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ProbeResult{
			Tool:    "web_fetch",
			Error:   fmt.Sprintf("请求失败: %v", err),
			Latency: time.Since(start),
		}
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	// 2xx or 3xx → available.
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return ProbeResult{
			Tool:      "web_fetch",
			Available: true,
			Latency:   time.Since(start),
		}
	}

	return ProbeResult{
		Tool:    "web_fetch",
		Error:   fmt.Sprintf("HTTP %d: %s", resp.StatusCode, resp.Status),
		Latency: time.Since(start),
	}
}

// containsStatusCode checks whether an error string contains a specific HTTP status code.
func containsStatusCode(errMsg string, code int) bool {
	needle := fmt.Sprintf("HTTP %d:", code)
	return len(errMsg) >= len(needle) && contains(errMsg, needle)
}

// contains is a simple substring check (avoids importing strings for one call).
func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// SSRF protection — mirrors webtool.validateURL / isPrivateOrLoopback
// ---------------------------------------------------------------------------

// isPrivateOrLoopback checks whether an IP is loopback, private, or link-local.
func isPrivateOrLoopback(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	// 169.254.x.x (AWS metadata endpoint, etc.)
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 169 && ip4[1] == 254 {
		return true
	}
	return false
}

// validateFetchURL parses the URL and verifies the target is not a private/loopback
// address, consistent with webtool.validateURL.
func validateFetchURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("URL 解析失败: %w", err)
	}
	host := u.Hostname()

	// Direct IP literal.
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateOrLoopback(ip) && !allowLoopbackForTests {
			return fmt.Errorf("拒绝访问内网地址: %s", rawURL)
		}
		return nil
	}

	// Domain name — resolve and check each IP.
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil // DNS failure → let the HTTP request report the real error
	}
	for _, ip := range ips {
		if isPrivateOrLoopback(ip) && !allowLoopbackForTests {
			return fmt.Errorf("拒绝访问内网地址: %s (解析到 %s)", rawURL, ip)
		}
	}
	return nil
}
