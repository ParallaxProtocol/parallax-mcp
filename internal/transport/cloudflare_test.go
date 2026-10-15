package transport

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ParallaxProtocol/parallax-mcp/pkg/config"
)

func TestClientIPBehindCloudflare(t *testing.T) {
	traefik := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	for _, tc := range []struct {
		name            string
		remote, xff, cf string
		trusted         []netip.Prefix
		cloudflare      bool
		want            string
	}{
		{"cloudflare direct", "104.16.1.1:443", "", "198.51.100.9", nil, true, "198.51.100.9"},
		{"forged header, not from cloudflare", "203.0.113.5:1", "", "198.51.100.9", nil, true, "203.0.113.5"},
		{"cloudflare then traefik", "10.0.1.2:1", "198.51.100.9, 104.16.1.1", "198.51.100.9", traefik, true, "198.51.100.9"},
		{"traefik replaced xff with the edge", "10.0.1.2:1", "162.158.4.4", "2001:db8::1", traefik, true, "2001:db8::1"},
		{"direct to traefik, forged header", "10.0.1.2:1", "203.0.113.5", "198.51.100.9", traefik, true, "203.0.113.5"},
		{"flag off", "104.16.1.1:443", "", "198.51.100.9", nil, false, "104.16.1.1"},
		{"garbage header", "104.16.1.1:443", "", "not-an-ip", nil, true, "104.16.1.1"},
	} {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		r.RemoteAddr = tc.remote
		if tc.xff != "" {
			r.Header.Set("X-Forwarded-For", tc.xff)
		}
		r.Header.Set("CF-Connecting-IP", tc.cf)
		if got := ClientIP(r, tc.trusted, tc.cloudflare).String(); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestKeepAliveOnlyOnEventStreams(t *testing.T) {
	defer func(d time.Duration) { keepAliveInterval = d }(keepAliveInterval)
	keepAliveInterval = 50 * time.Millisecond

	serve := func(contentType string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ka := newKeepAlive(w)
			defer ka.stop()
			ka.Header().Set("Content-Type", contentType)
			ka.WriteHeader(http.StatusOK)
			ka.Flush()
			time.Sleep(300 * time.Millisecond)
			_, _ = ka.Write([]byte("event: message\ndata: {}\n\n"))
		}))
		defer srv.Close()
		resp, err := http.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	body := serve("text/event-stream")
	if !strings.Contains(body, ": keepalive\n\n") || !strings.HasSuffix(body, "event: message\ndata: {}\n\n") {
		t.Fatalf("event stream: %q", body)
	}
	if body := serve("application/json"); strings.Contains(body, "keepalive") {
		t.Fatalf("json response got a keep-alive: %q", body)
	}
}

// A slow tool over real HTTP: the client must read through the keep-alive
// comments to the result.
func TestKeepAliveEndToEnd(t *testing.T) {
	defer func(d time.Duration) { keepAliveInterval = d }(keepAliveInterval)
	keepAliveInterval = 50 * time.Millisecond

	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "slow", Description: "sleeps"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		time.Sleep(400 * time.Millisecond)
		return nil, map[string]string{"ok": "yes"}, nil
	})
	cfg := config.Default()
	cfg.Transport = config.TransportHTTP
	hs := httptest.NewServer(Handler(cfg, srv, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer hs.Close()

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil).Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: hs.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "slow"})
	if err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}
}
