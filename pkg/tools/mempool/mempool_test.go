package mempool_test

import (
	"testing"

	"github.com/ParallaxProtocol/parallax-mcp/internal/testutil"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/server"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/tools/mempool"
)

const sender = "0x38432a328b3c6f15582292f7235F17b08A98C6AB"

func newEnv(t *testing.T, modules map[string]string, opts ...testutil.Options) *testutil.Env {
	return testutil.NewEnv(t, []func(*server.Registrar){mempool.Register}, func(e *testutil.Env) {
		e.Node.Result("rpc_modules", modules)
		e.Node.Result("eth_gasPrice", "0x3b9aca00")
		e.Node.Result("txpool_status", map[string]string{"pending": "0x2", "queued": "0x1"})
		e.Node.Result("txpool_inspect", map[string]any{
			"pending": map[string]any{sender: map[string]string{
				"7": "0x2CAa9795cd62261f96e40B8448b33636FF5e249a: 987900000000000000 wei + 21000 gas × 1000000000 wei",
				"8": "contract creation: 0 wei + 500000 gas × 129130911424 wei",
			}},
			"queued": map[string]any{sender: map[string]string{"10": "0x2CAa9795cd62261f96e40B8448b33636FF5e249a: 1 wei + 21000 gas × 2000000000 wei"}},
		})
		e.Status.Route("/api/v2/stats", `{"mempool":{"pending":3,"pressure":0.01,"blocksToClear":1},"nodes":{},"uptime":{}}`)
	}, opts...)
}

func TestMempoolStatus(t *testing.T) {
	e := newEnv(t, map[string]string{"eth": "1.0", "txpool": "1.0"})
	out := e.MustCall(t, "mempool_status", nil)
	if testutil.Get(out, "node", "pending") != float64(2) || testutil.Get(out, "network", "pressure_blocks") != 0.01 {
		t.Fatalf("got %v", out)
	}
}

func TestMempoolStatusWithoutTxpool(t *testing.T) {
	e := newEnv(t, map[string]string{"eth": "1.0"})
	e.Status.SetFail(500)
	out := e.MustCall(t, "mempool_status", nil)
	if out["gas_price_wei"] != "1000000000" || testutil.Get(out, "errors", "status_page") == nil {
		t.Fatalf("got %v", out)
	}
}

func TestMempoolTransactionsSortedByPrice(t *testing.T) {
	e := newEnv(t, map[string]string{"eth": "1.0", "txpool": "1.0"})
	out := e.MustCall(t, "mempool_transactions", nil)
	if out["count"] != float64(3) {
		t.Fatalf("count: %v", out)
	}
	first := testutil.Get(out, "transactions", 0)
	if testutil.Get(first, "gas_price_gwei") != "129.130911424" || testutil.Get(first, "to") != nil || testutil.Get(first, "nonce") != float64(8) {
		t.Fatalf("highest price first, contract creation has no to: %v", first)
	}
	if testutil.Get(out, "transactions", 2, "state") != "pending" || testutil.Get(out, "transactions", 1, "state") != "queued" {
		t.Fatalf("states: %v", out["transactions"])
	}
}

func TestMempoolStatusWithoutStatusPage(t *testing.T) {
	e := newEnv(t, map[string]string{"eth": "1.0", "txpool": "1.0"}, testutil.NodeOnly)
	out := e.MustCall(t, "mempool_status", nil)
	if testutil.Get(out, "node", "pending") != float64(2) || out["network"] != nil || testutil.Get(out, "errors", "status_page") != "no status page is configured" {
		t.Fatalf("got %v", out)
	}
}
