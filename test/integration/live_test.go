// Package integration runs the server against the real Parallax endpoints.
// It is skipped unless PARALLAX_MCP_LIVE=1:
//
//	PARALLAX_MCP_LIVE=1 go test ./test/integration/ -v
//
// PARALLAX_MCP_LIVE_RPC points it at another node, such as a local one with
// the debug, txpool and admin namespaces, whose tools are then tested too.
// Assertions check invariants (chain ID, consistency between sources),
// not values that move with every block.
package integration

import (
	"context"
	"io"
	"log/slog"
	"math"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ParallaxProtocol/parallax-mcp/internal/backend/blockscout"
	"github.com/ParallaxProtocol/parallax-mcp/internal/backend/node"
	"github.com/ParallaxProtocol/parallax-mcp/internal/backend/status"
	"github.com/ParallaxProtocol/parallax-mcp/internal/config"
	"github.com/ParallaxProtocol/parallax-mcp/internal/httpx"
	"github.com/ParallaxProtocol/parallax-mcp/internal/lax"
	"github.com/ParallaxProtocol/parallax-mcp/internal/server"
	"github.com/ParallaxProtocol/parallax-mcp/internal/testutil"
	"github.com/ParallaxProtocol/parallax-mcp/internal/tools"
)

// Known mainnet data.
const (
	stakeTx = "0x3afc1e38981d6246473bb324abfeff063dc5cec044d96d01802db20b184824f0" // block 20,063
	apepe   = "0xd1060183552A88455f037a58B2aC639C7966a214"
	vault   = "0x45a117ABe265623239a3c7A050EA7e8D7b040248"
)

type live struct {
	cs   *mcp.ClientSession
	reg  *server.Registrar
	full bool
}

func setup(t *testing.T) *live {
	t.Helper()
	if os.Getenv("PARALLAX_MCP_LIVE") != "1" {
		t.Skip("set PARALLAX_MCP_LIVE=1 to run against the real endpoints")
	}
	cfg := config.Default()
	cfg.Groups = config.AllGroups
	if rpc := os.Getenv("PARALLAX_MCP_LIVE_RPC"); rpc != "" {
		cfg.RPCURL = rpc
	}
	n, err := node.Dial(node.Options{URL: cfg.RPCURL, Timeout: cfg.BackendTimeout, Retries: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	opts := httpx.Options{Timeout: cfg.BackendTimeout, Retries: 2, UserAgent: "parallax-mcp-integration-test"}
	ex, st := opts, opts
	ex.BaseURL, st.BaseURL = cfg.ExplorerURL, cfg.StatusURL
	deps := server.Deps{
		Cfg: cfg, Node: n,
		Explorer: blockscout.New(ex),
		Status:   status.New(st, time.Minute),
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx := context.Background()
	reg := server.New(ctx, deps, tools.All...)
	ct, sst := mcp.NewInMemoryTransports()
	ss, err := reg.MCP().Connect(ctx, sst, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "it", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return &live{cs: cs, reg: reg, full: reg.HasModule("debug")}
}

func (l *live) call(t *testing.T, name string, args map[string]any) map[string]any {
	t.Helper()
	e := &testutil.Env{Client: l.cs}
	return e.MustCall(t, name, args)
}

func TestLiveChain(t *testing.T) {
	l := setup(t)
	info := l.call(t, "chain_info", nil)
	if info["chain_id"] != float64(2110) || info["chain_id_matches"] != true {
		t.Fatalf("chain_info: %v", info)
	}
	head := testutil.Get(info, "head", "number").(float64)
	if head < 59_000 {
		t.Fatalf("head %v is behind mainnet", head)
	}
	if age := testutil.Get(info, "head", "seconds_since_mined").(float64); age > 4*3600 {
		t.Errorf("no block for %v seconds: node stalled or network stalled", age)
	}

	tx := l.call(t, "get_transaction", map[string]any{"hash": stakeTx})
	if testutil.Get(tx, "decoded_input", "signature") != "stake(uint256)" || tx["block_number"] != float64(20063) {
		t.Fatalf("get_transaction: %v", tx)
	}
	rc := l.call(t, "get_receipt", map[string]any{"hash": stakeTx})
	if rc["status"] != "success" || testutil.Get(rc, "fee", "wei") != "10015005000000000" {
		t.Fatalf("get_receipt: %v", rc)
	}
	blk := l.call(t, "get_block", map[string]any{"block": "20063"})
	if blk["hash"] != "0xf7c436ececcc50200c86cba4781a38719631e3603798b3bbb962aaa1d15a867d" {
		t.Fatalf("get_block hash: %v", blk["hash"])
	}
	sym := l.call(t, "call_contract", map[string]any{"to": apepe, "function": "symbol() returns (string)"})
	if testutil.Get(sym, "decoded", 0, "value", "untrusted") != "APEPE" {
		t.Fatalf("call_contract: %v", sym)
	}
	logs := l.call(t, "get_logs", map[string]any{"addresses": []any{apepe}, "event_signature": "Transfer(address,address,uint256)", "from_block": "20063", "to_block": "20063"})
	if logs["count"] != float64(1) {
		t.Fatalf("get_logs: %v", logs)
	}
	gp := l.call(t, "gas_price", nil)
	if gp["gas_price_wei"] == nil {
		t.Fatalf("gas_price: %v", gp)
	}
}

func TestLiveMiningAgreesWithStatusPage(t *testing.T) {
	l := setup(t)
	st := l.call(t, "mining_stats", nil)
	ours := testutil.Get(st, "hashrate_estimate", "hashes_per_second").(float64)
	theirs, ok := testutil.Get(st, "status_page", "hashrate_24h_hashes_per_second").(float64)
	if !ok {
		t.Skipf("status page unavailable: %v", st["errors"])
	}
	// Both estimate the last day's hashrate from similar windows.
	if r := ours / theirs; r < 0.7 || r > 1.3 {
		t.Errorf("hashrate %.3g vs status page %.3g", ours, theirs)
	}

	sup := l.call(t, "supply_info", nil)
	if sup["mismatch"] != nil {
		t.Fatalf("node supply disagrees with the schedule: %v", sup)
	}
	head := uint64(sup["head_block"].(float64))
	issued, _ := new(big.Int).SetString(testutil.Get(sup, "circulating", "wei").(string), 10)
	if lo, hi := lax.IssuedThrough(head-1), lax.IssuedThrough(head+1); issued.Cmp(lo) < 0 || issued.Cmp(hi) > 0 {
		t.Fatalf("issued %s outside the schedule around %d", issued, head)
	}
	rw := l.call(t, "block_reward", map[string]any{"block": "20063"})
	if testutil.Get(rw, "total", "wei") != new(big.Int).Add(lax.BlockReward(20063), big.NewInt(10015005000000000)).String() {
		t.Fatalf("block_reward: %v", rw)
	}
}

func TestLiveExplorer(t *testing.T) {
	l := setup(t)
	tok := l.call(t, "explorer_token", map[string]any{"address": apepe})
	if testutil.Get(tok, "symbol", "untrusted") != "APEPE" || testutil.Get(tok, "decimals") != float64(18) {
		t.Fatalf("explorer_token: %v", tok)
	}
	c := l.call(t, "explorer_contract", map[string]any{"address": vault})
	if c["verified"] != true || len(c["functions"].([]any)) == 0 {
		t.Fatalf("explorer_contract: %v", c)
	}
	page := l.call(t, "explorer_token_holders", map[string]any{"address": apepe})
	if cur, _ := page["next_cursor"].(string); cur != "" {
		next := l.call(t, "explorer_token_holders", map[string]any{"address": apepe, "cursor": cur})
		if len(next["holders"].([]any)) == 0 {
			t.Fatal("second page empty")
		}
	}
	s := l.call(t, "explorer_search", map[string]any{"query": "pepe"})
	if len(s["results"].([]any)) == 0 {
		t.Fatal("search found nothing")
	}
}

func TestLiveNetwork(t *testing.T) {
	l := setup(t)
	h := l.call(t, "network_health", nil)
	if h["errors"] != nil {
		t.Fatalf("network_health errors: %v", h["errors"])
	}
	if behind, ok := testutil.Get(h, "node", "blocks_behind_status_best").(float64); !ok || math.Abs(behind) > 3 {
		t.Errorf("node and status page disagree on the head by %v blocks", behind)
	}
	nodes := l.call(t, "network_nodes", nil)
	if nodes["listed"].(float64) < 1 {
		t.Fatalf("network_nodes: %v", nodes)
	}
	l.call(t, "network_incidents", nil)
	l.call(t, "network_propagation", nil)
	l.call(t, "mempool_status", nil)
}

func TestLiveTx(t *testing.T) {
	l := setup(t)
	b := l.call(t, "build_unsigned_tx", map[string]any{"from": vault, "to": apepe, "function": "totalSupply() returns (uint256)"})
	if b["chain_id"] != float64(2110) || b["signing_hash"] == nil {
		t.Fatalf("build_unsigned_tx: %v", b)
	}
	d := l.call(t, "decode_tx", map[string]any{"raw": b["signing_payload"]})
	if d["format"] != "eip155_signing_payload" {
		t.Fatalf("decode_tx: %v", d)
	}
}

func TestLiveFullNode(t *testing.T) {
	l := setup(t)
	if !l.full {
		t.Skip("the node does not serve debug; set PARALLAX_MCP_LIVE_RPC to a full node")
	}
	tr := l.call(t, "trace_transaction", map[string]any{"hash": stakeTx})
	if tr["call_count"] != float64(2) || testutil.Get(tr, "calls", 1, "decoded_input", "signature") != "transferFrom(address,address,uint256)" {
		t.Fatalf("trace_transaction: %v", tr)
	}
	pre := l.call(t, "trace_transaction", map[string]any{"hash": stakeTx, "tracer": "prestate"})
	if testutil.Get(pre, "result", "touched_accounts_before") == nil {
		t.Fatalf("prestate: %v", pre)
	}
	if l.reg.HasModule("admin") {
		p := l.call(t, "node_peers", nil)
		if _, ok := testutil.Get(p, "peers", 0, "address").(string); ok {
			t.Fatal("peer address exposed without the flag")
		}
	}
	if l.reg.HasModule("txpool") {
		l.call(t, "mempool_transactions", nil)
	}
	old := l.call(t, "get_balance", map[string]any{"address": "0x38432a328b3c6f15582292f7235f17b08a98c6ab", "block": "30000"})
	if testutil.Get(old, "balance", "wei") == nil {
		t.Fatalf("archive read: %v", old)
	}
}
