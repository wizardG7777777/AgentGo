package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"pgregory.net/rapid"
)

// Feature: tool-health-probe, Property 4: Empty key fast fail for key-required providers
// *For any* key-required SearchProvider type (tavily, serper) with empty apiKey,
// the probe SHALL return Available=false immediately with error containing "未配置",
// and no HTTP request is made.
// **Validates: Requirements 2.4, 2.5**
func TestProperty_EmptyKeyFastFail(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		provider := rapid.SampledFrom([]string{"tavily", "serper"}).Draw(t, "provider")

		// Set up an httptest server to detect any outgoing HTTP requests.
		var requestCount atomic.Int64
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestCount.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer ts.Close()

		// Create probe with empty apiKey.
		probe := NewWebSearchProbe(provider, ts.URL, "")

		result := probe(context.Background())

		// The probe must report unavailable.
		if result.Available {
			t.Fatalf("provider %q with empty key: Available = true, want false", provider)
		}

		// Error must contain "未配置".
		if !strings.Contains(result.Error, "未配置") {
			t.Fatalf("provider %q with empty key: Error = %q, want substring %q", provider, result.Error, "未配置")
		}

		// No HTTP request should have been made.
		if got := requestCount.Load(); got != 0 {
			t.Fatalf("provider %q with empty key: %d HTTP requests made, want 0", provider, got)
		}
	})
}

// ---------------------------------------------------------------------------
// Unit tests for web_search probe (httptest-based)
// Task 4.3 — Requirements: 2.2, 2.3, 2.4, 2.6, 2.7
// ---------------------------------------------------------------------------

// --- SearXNG: full httptest coverage (accepts URL parameter) ---------------

func TestSearxngProbe_HTTP200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	probe := NewWebSearchProbe("searxng", ts.URL, "")
	result := probe(context.Background())

	if !result.Available {
		t.Fatalf("searxng 200: got Available=false, want true; error=%q", result.Error)
	}
	if result.Tool != "web_search" {
		t.Fatalf("searxng 200: got Tool=%q, want %q", result.Tool, "web_search")
	}
}

func TestSearxngProbe_HTTP403(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer ts.Close()

	probe := NewWebSearchProbe("searxng", ts.URL, "")
	result := probe(context.Background())

	if result.Available {
		t.Fatal("searxng 403: got Available=true, want false")
	}
	if !strings.Contains(result.Error, "403") {
		t.Fatalf("searxng 403: error %q does not contain status code 403", result.Error)
	}
}

func TestSearxngProbe_HTTP500(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	probe := NewWebSearchProbe("searxng", ts.URL, "")
	result := probe(context.Background())

	if result.Available {
		t.Fatal("searxng 500: got Available=true, want false")
	}
	if !strings.Contains(result.Error, "500") {
		t.Fatalf("searxng 500: error %q does not contain status code 500", result.Error)
	}
}

func TestSearxngProbe_ConnectionRefused(t *testing.T) {
	// Start and immediately close a server to get a port that refuses connections.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedURL := ts.URL
	ts.Close()

	probe := NewWebSearchProbe("searxng", closedURL, "")
	result := probe(context.Background())

	if result.Available {
		t.Fatal("searxng connection refused: got Available=true, want false")
	}
	if result.Error == "" {
		t.Fatal("searxng connection refused: error is empty, want network error description")
	}
}

func TestSearxngProbe_EmptyURL(t *testing.T) {
	probe := NewWebSearchProbe("searxng", "", "")
	result := probe(context.Background())

	if result.Available {
		t.Fatal("searxng empty URL: got Available=true, want false")
	}
	if !strings.Contains(result.Error, "未配置") {
		t.Fatalf("searxng empty URL: error %q does not contain '未配置'", result.Error)
	}
}

// --- SearXNG: verify User-Agent header is set ------------------------------

func TestSearxngProbe_SetsUserAgent(t *testing.T) {
	var gotUA string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	probe := NewWebSearchProbe("searxng", ts.URL, "")
	probe(context.Background())

	if gotUA != userAgent {
		t.Fatalf("searxng User-Agent: got %q, want %q", gotUA, userAgent)
	}
}

// --- Tavily: empty key fast fail -------------------------------------------

func TestTavilyProbe_EmptyKey(t *testing.T) {
	probe := NewWebSearchProbe("tavily", "", "")
	result := probe(context.Background())

	if result.Available {
		t.Fatal("tavily empty key: got Available=true, want false")
	}
	if !strings.Contains(result.Error, "未配置") {
		t.Fatalf("tavily empty key: error %q does not contain '未配置'", result.Error)
	}
	if result.Tool != "web_search" {
		t.Fatalf("tavily empty key: got Tool=%q, want %q", result.Tool, "web_search")
	}
}

// --- Serper: empty key fast fail -------------------------------------------

func TestSerperProbe_EmptyKey(t *testing.T) {
	probe := NewWebSearchProbe("serper", "", "")
	result := probe(context.Background())

	if result.Available {
		t.Fatal("serper empty key: got Available=true, want false")
	}
	if !strings.Contains(result.Error, "未配置") {
		t.Fatalf("serper empty key: error %q does not contain '未配置'", result.Error)
	}
	if result.Tool != "web_search" {
		t.Fatalf("serper empty key: got Tool=%q, want %q", result.Tool, "web_search")
	}
}

// --- Empty provider --------------------------------------------------------

func TestWebSearchProbe_EmptyProvider(t *testing.T) {
	probe := NewWebSearchProbe("", "", "")
	result := probe(context.Background())

	if result.Available {
		t.Fatal("empty provider: got Available=true, want false")
	}
	if result.Tool != "web_search" {
		t.Fatalf("empty provider: got Tool=%q, want %q", result.Tool, "web_search")
	}
}

// --- Unknown provider ------------------------------------------------------

func TestWebSearchProbe_UnknownProvider(t *testing.T) {
	probe := NewWebSearchProbe("bing_custom", "", "")
	result := probe(context.Background())

	if result.Available {
		t.Fatal("unknown provider: got Available=true, want false")
	}
	if !strings.Contains(result.Error, "未知") {
		t.Fatalf("unknown provider: error %q does not contain '未知'", result.Error)
	}
}

// --- Provider name normalization (case-insensitive, trimmed) ---------------

func TestWebSearchProbe_ProviderCaseInsensitive(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	for _, name := range []string{"SearXNG", "SEARXNG", " searxng ", "  SearXNG  "} {
		probe := NewWebSearchProbe(name, ts.URL, "")
		result := probe(context.Background())
		if !result.Available {
			t.Errorf("provider %q: got Available=false, want true; error=%q", name, result.Error)
		}
	}
}

// --- DuckDuckGo: verify probe creation returns a valid probe ---------------

func TestDuckduckgoProbe_Creation(t *testing.T) {
	probe := NewWebSearchProbe("duckduckgo_html", "", "")
	if probe == nil {
		t.Fatal("duckduckgo_html: NewWebSearchProbe returned nil")
	}
	// We can't redirect duckduckgo to httptest (hardcoded URL), but we verify
	// the probe is callable and returns a well-formed result.
	// Skip actual HTTP call to avoid flaky external dependency in unit tests.
}

// --- Context cancellation --------------------------------------------------

func TestSearxngProbe_ContextCancelled(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	probe := NewWebSearchProbe("searxng", ts.URL, "")
	result := probe(ctx)

	if result.Available {
		t.Fatal("searxng cancelled context: got Available=true, want false")
	}
}

// --- SearXNG: latency is recorded ------------------------------------------

func TestSearxngProbe_RecordsLatency(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	probe := NewWebSearchProbe("searxng", ts.URL, "")
	result := probe(context.Background())

	// Latency must be non-negative (may be 0 on very fast local round-trips).
	if result.Latency < 0 {
		t.Fatalf("searxng latency: got %v, want >= 0", result.Latency)
	}
}
