// Package testutil provides fake backends and an in-process MCP client for
// tests: a JSON-RPC node, an explorer and a status page as httptest servers.
package testutil

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ParallaxProtocol/parallax-mcp/internal/backend/blockscout"
	"github.com/ParallaxProtocol/parallax-mcp/internal/backend/node"
	"github.com/ParallaxProtocol/parallax-mcp/internal/backend/status"
	"github.com/ParallaxProtocol/parallax-mcp/internal/config"
	"github.com/ParallaxProtocol/parallax-mcp/internal/httpx"
	"github.com/ParallaxProtocol/parallax-mcp/internal/server"
)

// RPCHandler answers one JSON-RPC call: a result to encode, or an error.
type RPCHandler func(params []json.RawMessage) (any, *RPCError)

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// FakeNode is a JSON-RPC server with a handler per method.
type FakeNode struct {
	*httptest.Server
	mu       sync.Mutex
	handlers map[string]RPCHandler
	calls    []string
	// Fail, when set, makes every HTTP request answer with that status.
	Fail int
}

// NewFakeNode starts a fake node serving chain 2110 basics; add or replace
// methods with Handle.
func NewFakeNode(t testing.TB) *FakeNode {
	f := &FakeNode{handlers: map[string]RPCHandler{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	f.Result("eth_chainId", "0x83e")
	f.Result("rpc_modules", map[string]string{"eth": "1.0", "net": "1.0", "web3": "1.0"})
	return f
}

// Handle sets the handler of a method.
func (f *FakeNode) Handle(method string, h RPCHandler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[method] = h
}

// Result makes a method always return v.
func (f *FakeNode) Result(method string, v any) {
	f.Handle(method, func([]json.RawMessage) (any, *RPCError) { return v, nil })
}

// Calls returns the methods called so far.
func (f *FakeNode) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

type rpcReq struct {
	ID     json.RawMessage   `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

func (f *FakeNode) serve(w http.ResponseWriter, r *http.Request) {
	if f.Fail != 0 {
		http.Error(w, "down", f.Fail)
		return
	}
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		var reqs []rpcReq
		_ = json.Unmarshal(body, &reqs)
		resps := make([]rpcResp, len(reqs))
		for i, q := range reqs {
			resps[i] = f.answer(q)
		}
		_ = json.NewEncoder(w).Encode(resps)
		return
	}
	var q rpcReq
	_ = json.Unmarshal(body, &q)
	_ = json.NewEncoder(w).Encode(f.answer(q))
}

func (f *FakeNode) answer(q rpcReq) rpcResp {
	f.mu.Lock()
	h := f.handlers[q.Method]
	f.calls = append(f.calls, q.Method)
	f.mu.Unlock()
	resp := rpcResp{JSONRPC: "2.0", ID: q.ID}
	if h == nil {
		resp.Error = &RPCError{Code: -32601, Message: "the method " + q.Method + " does not exist/is not available"}
		return resp
	}
	res, err := h(q.Params)
	if err != nil {
		resp.Error = err
		return resp
	}
	if res == nil {
		resp.Result = json.RawMessage("null")
	} else {
		resp.Result = res
	}
	return resp
}

// FakeHTTP serves fixed JSON bodies by path (with the query string when a
// key contains "?") and counts requests.
type FakeHTTP struct {
	*httptest.Server
	mu     sync.Mutex
	routes map[string]string
	hits   map[string]int
	// Fail, when set, makes every request answer with that status.
	Fail int
}

// NewFakeHTTP starts an empty fake HTTP backend.
func NewFakeHTTP(t testing.TB) *FakeHTTP {
	f := &FakeHTTP{routes: map[string]string{}, hits: map[string]int{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		fail := f.Fail
		key := r.URL.Path
		body, ok := f.routes[r.URL.Path+"?"+r.URL.RawQuery]
		if ok {
			key = r.URL.Path + "?" + r.URL.RawQuery
		} else {
			body, ok = f.routes[r.URL.Path]
		}
		f.hits[key]++
		f.mu.Unlock()
		if fail != 0 {
			http.Error(w, "down", fail)
			return
		}
		if !ok {
			http.Error(w, `{"message":"Not found"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(f.Close)
	return f
}

// Route sets the body served at path.
func (f *FakeHTTP) Route(path, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[path] = body
}

// SetFail sets the failing status (0 to recover).
func (f *FakeHTTP) SetFail(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Fail = code
}

// Hits returns how many requests reached path.
func (f *FakeHTTP) Hits(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

// Env is a server wired to fake backends with an in-memory MCP client.
type Env struct {
	Node     *FakeNode
	Explorer *FakeHTTP
	Status   *FakeHTTP
	Reg      *server.Registrar
	Client   *mcp.ClientSession
	Now      time.Time
}

// Options adjusts the configuration of an Env.
type Options func(*config.Config)

// NewEnv starts fake backends and a server with the given tool groups
// registered, connected to an in-memory client.
func NewEnv(t testing.TB, register []func(*server.Registrar), setup func(*Env), opts ...Options) *Env {
	t.Helper()
	e := &Env{Node: NewFakeNode(t), Explorer: NewFakeHTTP(t), Status: NewFakeHTTP(t), Now: time.Unix(1_791_870_000, 0)}
	if setup != nil {
		setup(e)
	}
	cfg := config.Default()
	cfg.Groups = config.AllGroups
	cfg.RPCURL, cfg.ExplorerURL, cfg.StatusURL = e.Node.URL, e.Explorer.URL, e.Status.URL
	cfg.BackendRetries = 0
	cfg.BackendTimeout = 5 * time.Second
	for _, o := range opts {
		o(&cfg)
	}
	n, err := node.Dial(node.Options{URL: cfg.RPCURL, Timeout: cfg.BackendTimeout, AllowBroadcast: cfg.BroadcastEnabled()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	deps := server.Deps{
		Cfg:           cfg,
		Node:          n,
		Explorer:      blockscout.New(httpx.Options{BaseURL: cfg.ExplorerURL, Timeout: cfg.BackendTimeout}),
		ExplorerStats: blockscout.NewStats(httpx.Options{BaseURL: cfg.StatsURL(), Timeout: cfg.BackendTimeout}),
		Status:        status.New(httpx.Options{BaseURL: cfg.StatusURL, Timeout: cfg.BackendTimeout}, time.Minute),
		Log:           log,
		Now:           func() time.Time { return e.Now },
	}
	ctx := context.Background()
	e.Reg = server.New(ctx, deps, register...)

	ct, st := mcp.NewInMemoryTransports()
	ss, err := e.Reg.MCP().Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	e.Client = cs
	return e
}

// Call invokes a tool and returns its JSON result decoded into a map, or
// the error text when the tool failed.
func (e *Env) Call(t testing.TB, name string, args map[string]any) (map[string]any, string) {
	t.Helper()
	res, err := e.Client.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("calling %s: %v", name, err)
	}
	text := ""
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	if res.IsError {
		return nil, text
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%s returned non-JSON %q: %v", name, text, err)
	}
	return out, ""
}

// MustCall is Call that fails the test on a tool error.
func (e *Env) MustCall(t testing.TB, name string, args map[string]any) map[string]any {
	t.Helper()
	out, errText := e.Call(t, name, args)
	if errText != "" {
		t.Fatalf("%s failed: %s", name, errText)
	}
	return out
}

// Tools lists the registered tool names.
func (e *Env) Tools(t testing.TB) []string {
	t.Helper()
	res, err := e.Client.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// Get walks a decoded JSON value by keys and indexes: Get(m, "a", 0, "b").
func Get(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[k]
		case int:
			s, ok := v.([]any)
			if !ok || k >= len(s) {
				return nil
			}
			v = s[k]
		}
	}
	return v
}
