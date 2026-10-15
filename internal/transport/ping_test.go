package transport

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ParallaxProtocol/parallax-mcp/pkg/config"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/server"
)

// A slow tool's event stream must start with pings before the result, so
// proxies that wait for response headers or bytes keep it open.
func TestSlowToolCallsArePinged(t *testing.T) {
	defer func(d time.Duration) { server.PingInterval = d }(server.PingInterval)
	server.PingInterval = 100 * time.Millisecond

	cfg := config.Default()
	cfg.Transport = config.TransportHTTP
	cfg.Groups = []string{config.GroupChain}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := server.New(context.Background(), server.Deps{Cfg: cfg, Log: log}, func(r *server.Registrar) {
		server.Add(r, server.Spec{Group: config.GroupChain, Name: "slow", Description: "sleeps"}, func(ctx context.Context, _ struct{}) (any, error) {
			time.Sleep(450 * time.Millisecond)
			return map[string]string{"ok": "yes"}, nil
		})
	})
	hs := httptest.NewServer(Handler(cfg, reg.MCP(), log))
	defer hs.Close()

	post := func(body, session string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, hs.URL+"/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	init := post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`, "")
	session := init.Header.Get("Mcp-Session-Id")
	_, _ = io.Copy(io.Discard, init.Body)
	init.Body.Close()
	post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`, session).Body.Close()

	start := time.Now()
	resp := post(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"slow","arguments":{}}}`, session)
	defer resp.Body.Close()
	firstByte := time.Since(start)
	if firstByte > 300*time.Millisecond {
		t.Fatalf("response headers came after %s; the call's stream should start with a ping", firstByte)
	}
	var pings int
	var result bool
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.Contains(line, `"method":"ping"`) {
			pings++
		}
		if strings.Contains(line, `"id":2,"result"`) {
			result = true
			break
		}
	}
	if pings == 0 || !result {
		t.Fatalf("pings %d, result %v", pings, result)
	}
}
