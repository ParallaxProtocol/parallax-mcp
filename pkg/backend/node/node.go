// Package node is the Parallax JSON-RPC backend. It speaks raw JSON-RPC
// through Parallax's rpc.Client, so blocks keep fields upstream go-ethereum
// types would drop (epochStartTime), and every request passes through an
// allowlist (guard.go) before it leaves the process.
package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ParallaxProtocol/parallax/v2/rpc"
)

// Options configures a Client.
type Options struct {
	URL            string
	Timeout        time.Duration
	Retries        int
	AllowBroadcast bool
	// Transport overrides the HTTP transport, for tests.
	Transport http.RoundTripper
}

// Client is a connection to one node.
type Client struct {
	rpc *rpc.Client

	mu      sync.Mutex
	modules map[string]bool
	probed  time.Time
}

// Dial connects to an http(s) JSON-RPC endpoint. No request is made until
// the first call.
func Dial(opts Options) (*Client, error) {
	u, err := url.Parse(opts.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("rpc url must be http:// or https://, got %q", opts.URL)
	}
	next := opts.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	g := &guard{
		next:           next,
		allowBroadcast: opts.AllowBroadcast,
		timeout:        opts.Timeout,
		retries:        opts.Retries,
		baseDelay:      250 * time.Millisecond,
	}
	c, err := rpc.DialHTTPWithClient(opts.URL, &http.Client{Transport: g})
	if err != nil {
		return nil, err
	}
	return &Client{rpc: c}, nil
}

// Close releases the connection.
func (c *Client) Close() { c.rpc.Close() }

// Call invokes method and decodes the result into out.
func (c *Client) Call(ctx context.Context, out any, method string, args ...any) error {
	if err := c.rpc.CallContext(ctx, out, method, args...); err != nil {
		return wrap(method, err)
	}
	return nil
}

// Raw invokes method and returns the undecoded result, which is JSON null
// for a missing block or transaction.
func (c *Client) Raw(ctx context.Context, method string, args ...any) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.Call(ctx, &out, method, args...)
	return out, err
}

// BatchElem is one call of a batch.
type BatchElem = rpc.BatchElem

// Batch sends several calls in one request. Per-call failures are in each
// element's Error; the returned error is a transport failure.
func (c *Client) Batch(ctx context.Context, elems []BatchElem) error {
	if len(elems) == 0 {
		return nil
	}
	if err := c.rpc.BatchCallContext(ctx, elems); err != nil {
		return wrap("batch", err)
	}
	return nil
}

// Modules returns the namespaces the node serves, from rpc_modules. The
// answer is kept for 10 minutes; a node is not reconfigured while running.
func (c *Client) Modules(ctx context.Context) (map[string]bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.modules != nil && time.Since(c.probed) < 10*time.Minute {
		return c.modules, nil
	}
	var res map[string]string
	if err := c.Call(ctx, &res, "rpc_modules"); err != nil {
		return nil, err
	}
	mods := make(map[string]bool, len(res))
	for ns := range res {
		mods[ns] = true
	}
	c.modules, c.probed = mods, time.Now()
	return mods, nil
}

// Has reports whether the node serves namespace ns. An unreachable node
// reports false.
func (c *Client) Has(ctx context.Context, ns string) bool {
	mods, err := c.Modules(ctx)
	return err == nil && mods[ns]
}

// ModuleList returns the served namespaces sorted, or nil.
func (c *Client) ModuleList(ctx context.Context) []string {
	mods, err := c.Modules(ctx)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(mods))
	for ns := range mods {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// Error is a failed RPC call.
type Error struct {
	Method string
	Code   int // JSON-RPC error code, 0 for transport errors
	Msg    string
	err    error
}

func (e *Error) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("rpc %s: %s (code %d)", e.Method, e.Msg, e.Code)
	}
	return fmt.Sprintf("rpc %s: %s", e.Method, e.Msg)
}

func (e *Error) Unwrap() error { return e.err }

// MethodNotFound reports whether err says the node does not serve the method.
func MethodNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && (e.Code == -32601 || strings.Contains(e.Msg, "does not exist/is not available"))
}

// Denied reports whether err is an allowlist refusal.
func Denied(err error) bool { return errors.Is(err, ErrDenied) }

func wrap(method string, err error) error {
	e := &Error{Method: method, Msg: err.Error(), err: err}
	var rerr rpc.Error
	if errors.As(err, &rerr) {
		e.Code = rerr.ErrorCode()
	}
	var herr rpc.HTTPError
	if errors.As(err, &herr) {
		e.Msg = fmt.Sprintf("HTTP %d", herr.StatusCode)
	}
	return e
}
