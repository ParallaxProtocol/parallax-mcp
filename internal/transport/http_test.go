package transport

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ParallaxProtocol/parallax-mcp/pkg/config"
)

func TestClientIP(t *testing.T) {
	proxies := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	for _, tc := range []struct {
		remote, xff, want string
	}{
		{"203.0.113.5:1234", "", "203.0.113.5"},
		{"203.0.113.5:1234", "1.1.1.1", "203.0.113.5"},                       // untrusted peer cannot set XFF
		{"10.0.0.2:1234", "198.51.100.7", "198.51.100.7"},                    // trusted proxy
		{"10.0.0.2:1234", "6.6.6.6, 198.51.100.7, 10.0.0.9", "198.51.100.7"}, // right-most non-proxy wins
		{"10.0.0.2:1234", "garbage", "10.0.0.2"},
		{"[::ffff:203.0.113.5]:1", "", "203.0.113.5"},
	} {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		r.RemoteAddr = tc.remote
		if tc.xff != "" {
			r.Header.Set("X-Forwarded-For", tc.xff)
		}
		if got := ClientIP(r, proxies, false).String(); got != tc.want {
			t.Errorf("%s / %q: got %s, want %s", tc.remote, tc.xff, got, tc.want)
		}
	}
}

func TestRateLimitPerClient(t *testing.T) {
	cfg := config.Default()
	cfg.RateLimit, cfg.RateBurst = 0.001, 2
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	h := Handler(cfg, srv, slog.New(slog.NewTextHandler(io.Discard, nil)))

	post := func(remote string) int {
		r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		r.RemoteAddr = remote
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for i := range 2 {
		if code := post("198.51.100.1:1"); code == http.StatusTooManyRequests {
			t.Fatalf("request %d limited too early", i)
		}
	}
	if code := post("198.51.100.1:1"); code != http.StatusTooManyRequests {
		t.Fatalf("third request: want 429, got %d", code)
	}
	if code := post("198.51.100.2:1"); code == http.StatusTooManyRequests {
		t.Fatal("another client must have its own budget")
	}
}

func TestHealthz(t *testing.T) {
	h := Handler(config.Default(), mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != 200 || w.Body.String() != "ok\n" {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
}
