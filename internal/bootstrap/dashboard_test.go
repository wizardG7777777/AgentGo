package bootstrap

import (
	"context"
	"net/http"
	"testing"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/ui"
)

// TestSystem_Dashboard_StartsAndStops ui.frontends 含 "web" 时 startDashboard
// 启动 HTTP 服务器（127.0.0.1:0 临时端口），/healthz 应答；ctx 取消后
// 优雅退出（wg 归零，goroutine 不泄漏）。
func TestSystem_Dashboard_StartsAndStops(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.UI.Frontends = []string{"web"}
	cfg.UI.Web.Listen = "127.0.0.1:0"

	s := &System{Config: cfg, UIHub: ui.NewHub(ui.Deps{})}

	ctx, cancel := context.WithCancel(context.Background())
	s.startDashboard(ctx)

	if s.Dashboard == nil {
		t.Fatal("frontends 含 web 时 Dashboard 应被装配")
	}
	waitForUI(t, "Web Dashboard 完成监听", func() bool {
		return s.Dashboard.Addr() != ""
	})

	resp, err := http.Get("http://" + s.Dashboard.Addr() + "/healthz")
	if err != nil {
		t.Fatalf("healthz 请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d，期望 200", resp.StatusCode)
	}

	// 快照端点同样可达（Hub 未 Run 时返回零值快照，仍是合法 JSON）。
	resp2, err := http.Get("http://" + s.Dashboard.Addr() + "/api/snapshot")
	if err != nil {
		t.Fatalf("snapshot 请求失败: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("/api/snapshot status = %d，期望 200", resp2.StatusCode)
	}

	cancel()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("cancel 后 Web Dashboard 未退出（goroutine 泄漏）")
	}
}

// TestSystem_Dashboard_NotStartedWithoutWebFrontend frontends 不含 "web"
// （默认 [tui]）时 startDashboard 是 no-op。
func TestSystem_Dashboard_NotStartedWithoutWebFrontend(t *testing.T) {
	cfg := config.DefaultConfig() // 默认 frontends=[tui]

	s := &System{Config: cfg, UIHub: ui.NewHub(ui.Deps{})}
	s.startDashboard(context.Background())

	if s.Dashboard != nil {
		t.Fatal("frontends 不含 web 时不应装配 Dashboard")
	}
}
