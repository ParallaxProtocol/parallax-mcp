package node

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// upstream counts requests and answers every call with "0x1".
func upstream(t *testing.T, status *atomic.Int32, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if code := status.Load(); code != 0 {
			http.Error(w, "fail", int(code))
			return
		}
		body, _ := io.ReadAll(r.Body)
		if strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
			var reqs []struct{ ID json.RawMessage }
			_ = json.Unmarshal(body, &reqs)
			out := make([]map[string]any, len(reqs))
			for i, q := range reqs {
				out[i] = map[string]any{"jsonrpc": "2.0", "id": q.ID, "result": "0x1"}
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		var q struct{ ID json.RawMessage }
		_ = json.Unmarshal(body, &q)
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": q.ID, "result": "0x1"})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func dial(t *testing.T, url string, broadcast bool, retries int) *Client {
	t.Helper()
	c, err := Dial(Options{URL: url, Timeout: 2 * time.Second, Retries: retries, AllowBroadcast: broadcast})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestGuardRefusesDangerousMethods(t *testing.T) {
	var status, hits atomic.Int32
	srv := upstream(t, &status, &hits)
	c := dial(t, srv.URL, false, 0)
	ctx := context.Background()

	for _, call := range []struct {
		method string
		args   []any
	}{
		{"personal_unlockAccount", []any{"0x0000000000000000000000000000000000000001", "pw", 0}},
		{"personal_importRawKey", []any{"00", "pw"}},
		{"personal_sign", []any{"0x00", "0x0000000000000000000000000000000000000001", "pw"}},
		{"eth_sendTransaction", []any{map[string]any{}}},
		{"eth_sign", []any{"0x0000000000000000000000000000000000000001", "0x00"}},
		{"eth_signTransaction", []any{map[string]any{}}},
		{"eth_sendRawTransaction", []any{"0x00"}},
		{"admin_addPeer", []any{"enode://x"}},
		{"admin_setban", []any{"1.2.3.4", "add"}},
		{"admin_startHTTP", nil},
		{"admin_stop", nil},
		{"miner_start", nil},
		{"miner_setCoinbase", []any{"0x0000000000000000000000000000000000000001"}},
		{"debug_setHead", []any{"0x1"}},
		{"debug_dbGet", []any{"0x00"}},
		{"debug_chaindbCompact", nil},
		{"debug_startCPUProfile", []any{"/tmp/x"}},
		{"txpool_content", nil},
		{"xhash_submitWork", []any{"0x0", "0x0", "0x0"}},
	} {
		var out any
		err := c.Call(ctx, &out, call.method, call.args...)
		if !Denied(err) {
			t.Errorf("%s: want an allowlist refusal, got %v", call.method, err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("%d refused requests reached the node", n)
	}
}

func TestGuardTracers(t *testing.T) {
	var status, hits atomic.Int32
	srv := upstream(t, &status, &hits)
	c := dial(t, srv.URL, false, 0)
	ctx := context.Background()
	h := "0x" + strings.Repeat("ab", 32)
	var out any

	if err := c.Call(ctx, &out, "debug_traceTransaction", h, map[string]any{"tracer": "callTracer"}); err != nil {
		t.Fatalf("callTracer refused: %v", err)
	}
	for name, cfg := range map[string]any{
		"js tracer":  map[string]any{"tracer": "{step: function(){}, result: function(){return 1}}"},
		"no tracer":  map[string]any{},
		"no config":  nil,
		"bad tracer": map[string]any{"tracer": "muxTracer"},
	} {
		args := []any{h}
		if cfg != nil {
			args = append(args, cfg)
		}
		if err := c.Call(ctx, &out, "debug_traceTransaction", args...); !Denied(err) {
			t.Errorf("%s: want refusal, got %v", name, err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("want exactly the callTracer request upstream, got %d", hits.Load())
	}
}

func TestGuardBatchWithOneDeniedMethodIsRefusedWhole(t *testing.T) {
	var status, hits atomic.Int32
	srv := upstream(t, &status, &hits)
	c := dial(t, srv.URL, false, 0)
	var a, b any
	err := c.Batch(context.Background(), []BatchElem{
		{Method: "eth_blockNumber", Result: &a},
		{Method: "personal_listWallets", Result: &b},
	})
	if !Denied(err) {
		t.Fatalf("want refusal, got %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("refused batch reached the node")
	}
}

func TestGuardBroadcastOnlyWhenAllowed(t *testing.T) {
	var status, hits atomic.Int32
	srv := upstream(t, &status, &hits)
	var out any
	if err := dial(t, srv.URL, false, 0).Call(context.Background(), &out, "eth_sendRawTransaction", "0x00"); !Denied(err) {
		t.Fatalf("broadcast without the flag: want refusal, got %v", err)
	}
	if err := dial(t, srv.URL, true, 0).Call(context.Background(), &out, "eth_sendRawTransaction", "0x00"); err != nil {
		t.Fatalf("broadcast with the flag: %v", err)
	}
}

func TestGuardRetriesReadsButNotBroadcasts(t *testing.T) {
	var status, hits atomic.Int32
	srv := upstream(t, &status, &hits)
	status.Store(http.StatusBadGateway)
	ctx := context.Background()
	var out any

	c := dial(t, srv.URL, true, 2)
	_ = c.Call(ctx, &out, "eth_blockNumber")
	if got := hits.Load(); got != 3 {
		t.Fatalf("read: want 3 attempts, got %d", got)
	}
	hits.Store(0)
	_ = c.Call(ctx, &out, "eth_sendRawTransaction", "0x00")
	if got := hits.Load(); got != 1 {
		t.Fatalf("broadcast: want 1 attempt, got %d", got)
	}
}

func TestGuardRetryRecovers(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			http.Error(w, "busy", http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":"0x2a"}`)
	}))
	defer srv.Close()
	var out string
	if err := dial(t, srv.URL, false, 2).Call(context.Background(), &out, "eth_blockNumber"); err != nil {
		t.Fatal(err)
	}
	if out != "0x2a" || hits.Load() != 2 {
		t.Fatalf("got %q after %d attempts", out, hits.Load())
	}
}

func TestMethodNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"the method txpool_status does not exist/is not available"}}`)
	}))
	defer srv.Close()
	var out any
	err := dial(t, srv.URL, false, 0).Call(context.Background(), &out, "txpool_status")
	if !MethodNotFound(err) {
		t.Fatalf("want method-not-found, got %v", err)
	}
	var e *Error
	if !errors.As(err, &e) || e.Code != -32601 {
		t.Fatalf("want code -32601, got %#v", err)
	}
}

func TestDialRejectsNonHTTP(t *testing.T) {
	for _, u := range []string{"ws://localhost:8546", "/tmp/parallax.ipc", "localhost:8545"} {
		if _, err := Dial(Options{URL: u}); err == nil {
			t.Errorf("%s: want error", u)
		}
	}
}
