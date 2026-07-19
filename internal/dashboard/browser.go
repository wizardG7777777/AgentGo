package dashboard

import (
	"context"
	"net/http"
	"os/exec"
	"runtime"
	"time"
)

// OpenBrowser 用系统默认浏览器打开 url。fire-and-forget：只负责拉起进程，
// 不等待浏览器（其生命周期与 agentgo 无关）。声明为变量便于测试替换。
//
// Windows 走 rundll32 而非 cmd /c start——后者对 URL query 里的 & 有转义
// 陷阱（带 ?token= 的地址会被截断），rundll32 直接按参数传递，无此问题。
var OpenBrowser = func(url string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}

// WaitReady 轮询 baseURL 的 /healthz，直到应答 200（服务真正开始监听）、
// 超时或 ctx 取消。供"启动成功后再动作"的语义使用（如自动打开浏览器）。
func WaitReady(ctx context.Context, baseURL string, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()

	client := &http.Client{Timeout: time.Second}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"healthz", nil)
		if err == nil {
			if resp, err := client.Do(req); err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return true
				}
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-tick.C:
		}
	}
}
