package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/ParallaxProtocol/parallax-mcp/pkg/metrics"
)

// readMethods are the JSON-RPC methods the server may call. Everything else
// is refused before it leaves the process, so pointing the server at a node
// with personal, admin, miner or debug fully exposed cannot unlock accounts,
// sign, change peers, rewind the chain or read the database.
var readMethods = map[string]bool{
	"web3_clientVersion": true,
	"rpc_modules":        true,
	"net_version":        true,
	"net_peerCount":      true,
	"net_listening":      true,
	"net_peers":          true,

	"eth_chainId":                             true,
	"eth_blockNumber":                         true,
	"eth_syncing":                             true,
	"eth_gasPrice":                            true,
	"eth_maxPriorityFeePerGas":                true,
	"eth_feeHistory":                          true,
	"eth_estimateSmartFee":                    true,
	"eth_getBalance":                          true,
	"eth_getCode":                             true,
	"eth_getStorageAt":                        true,
	"eth_getTransactionCount":                 true,
	"eth_getProof":                            true,
	"eth_getBlockByNumber":                    true,
	"eth_getBlockByHash":                      true,
	"eth_getHeaderByNumber":                   true,
	"eth_getHeaderByHash":                     true,
	"eth_getBlockTransactionCountByNumber":    true,
	"eth_getBlockTransactionCountByHash":      true,
	"eth_getTransactionByHash":                true,
	"eth_getTransactionByBlockNumberAndIndex": true,
	"eth_getRawTransactionByHash":             true,
	"eth_getTransactionReceipt":               true,
	"eth_call":                                true,
	"eth_estimateGas":                         true,
	"eth_getLogs":                             true,
	"eth_getTotalSupply":                      true,
	"eth_getCirculatingSupply":                true,

	"xhash_getTotalSupply":       true,
	"xhash_getCirculatingSupply": true,

	"txpool_status":      true,
	"txpool_inspect":     true,
	"txpool_contentFrom": true,

	"debug_traceTransaction": true,
	"debug_traceCall":        true,

	"admin_nodeInfo":       true,
	"admin_peers":          true,
	"admin_uptime":         true,
	"admin_addrbookStatus": true,
	"admin_listbanned":     true,
}

// broadcastMethod is the one write method, allowed only with
// --allow-broadcast and never retried.
const broadcastMethod = "eth_sendRawTransaction"

// AllowedTracers are the built-in tracers trace calls may name. The node
// also accepts JavaScript tracers, which run arbitrary code against its
// state; those are refused.
var AllowedTracers = map[string]bool{
	"callTracer":     true,
	"prestateTracer": true,
	"4byteTracer":    true,
}

// ErrDenied is wrapped by every refusal of the guard.
var ErrDenied = errors.New("refused by parallax-mcp's RPC allowlist")

// maxRPCBody caps a node response.
const maxRPCBody = 16 << 20

// guard is the http.RoundTripper under the RPC client: it enforces the
// allowlist, retries idempotent calls and bounds each attempt.
type guard struct {
	next           http.RoundTripper
	allowBroadcast bool
	timeout        time.Duration
	retries        int
	baseDelay      time.Duration
}

type rpcMessage struct {
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

func (g *guard) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body == nil {
		return nil, fmt.Errorf("%w: empty request", ErrDenied)
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, 4<<20))
	req.Body.Close()
	if err != nil {
		return nil, err
	}
	methods, err := g.check(body)
	if err != nil {
		metrics.Backend("rpc", "denied")
		return nil, err
	}

	retries := g.retries
	for _, m := range methods {
		if m == broadcastMethod {
			retries = 0 // a resent transaction is harmless, but a resent broadcast is not idempotent from the caller's view
		}
	}

	start := time.Now()
	defer func() { metrics.BackendLatency("rpc", time.Since(start)) }()

	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			metrics.Backend("rpc", "retry")
			d := g.baseDelay << (attempt - 1)
			select {
			case <-req.Context().Done():
				return nil, lastErr
			case <-time.After(d/2 + rand.N(d/2+1)):
			}
		}
		resp, retry, err := g.attempt(req, body)
		if err == nil {
			metrics.Backend("rpc", "ok")
			return resp, nil
		}
		lastErr = err
		if !retry || req.Context().Err() != nil {
			break
		}
	}
	metrics.Backend("rpc", "error")
	return nil, lastErr
}

// attempt sends the request once and reads the whole response, so the
// per-attempt timeout can be released before the body is handed back.
func (g *guard) attempt(orig *http.Request, body []byte) (*http.Response, bool, error) {
	ctx, cancel := context.WithTimeout(orig.Context(), g.timeout)
	defer cancel()
	req := orig.Clone(ctx)
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))

	resp, err := g.next.RoundTrip(req)
	if err != nil {
		return nil, orig.Context().Err() == nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRPCBody+1))
	if err != nil {
		return nil, true, err
	}
	if len(data) > maxRPCBody {
		return nil, false, fmt.Errorf("rpc response larger than %d bytes", maxRPCBody)
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return nil, true, fmt.Errorf("rpc: HTTP %d", resp.StatusCode)
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	resp.ContentLength = int64(len(data))
	return resp, false, nil
}

// check parses a single or batch JSON-RPC request and returns its methods,
// or an error naming the first one the allowlist refuses.
func (g *guard) check(body []byte) ([]string, error) {
	var msgs []rpcMessage
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &msgs); err != nil {
			return nil, fmt.Errorf("%w: malformed batch", ErrDenied)
		}
	} else {
		var m rpcMessage
		if err := json.Unmarshal(trimmed, &m); err != nil {
			return nil, fmt.Errorf("%w: malformed request", ErrDenied)
		}
		msgs = []rpcMessage{m}
	}
	methods := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if err := g.allowed(m); err != nil {
			return nil, err
		}
		methods = append(methods, m.Method)
	}
	return methods, nil
}

func (g *guard) allowed(m rpcMessage) error {
	switch {
	case m.Method == broadcastMethod:
		if !g.allowBroadcast {
			return fmt.Errorf("%w: %s needs --allow-broadcast", ErrDenied, m.Method)
		}
		return nil
	case !readMethods[m.Method]:
		return fmt.Errorf("%w: method %s", ErrDenied, m.Method)
	case strings.HasPrefix(m.Method, "debug_trace"):
		return checkTracer(m)
	}
	return nil
}

// checkTracer requires a trace call's config, its last parameter, to name an
// allowed built-in tracer. Without one the node runs the opcode logger, whose
// output for a single transaction can run to hundreds of megabytes.
func checkTracer(m rpcMessage) error {
	if len(m.Params) < 2 {
		return fmt.Errorf("%w: %s without a tracer config", ErrDenied, m.Method)
	}
	var cfg struct {
		Tracer *string `json:"tracer"`
	}
	if err := json.Unmarshal(m.Params[len(m.Params)-1], &cfg); err != nil || cfg.Tracer == nil {
		return fmt.Errorf("%w: %s without a named tracer", ErrDenied, m.Method)
	}
	if !AllowedTracers[*cfg.Tracer] {
		return fmt.Errorf("%w: tracer %q is not one of callTracer, prestateTracer, 4byteTracer", ErrDenied, truncate(*cfg.Tracer, 40))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
