package mining_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ParallaxProtocol/parallax/v2/util/hexutil"

	"github.com/ParallaxProtocol/parallax-mcp/internal/testutil"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/server"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/tools/mining"
)

const (
	head    = 1000
	genesis = 1_700_000_000
	minerA  = "0x38432a328b3c6f15582292f7235f17b08a98c6ab"
	minerB  = "0x6c0db3ea9eed7ed145f36da461d84a8d02596b08"
)

// chain serves headers with a fixed 600 second spacing, difficulty 6e12
// (so hashrate is exactly 1e10 H/s), and every fourth block from minerB.
func chain(e *testutil.Env) {
	e.Node.Result("eth_blockNumber", hexutil.EncodeUint64(head))
	e.Node.Handle("eth_getHeaderByNumber", func(p []json.RawMessage) (any, *testutil.RPCError) {
		var tag string
		_ = json.Unmarshal(p[0], &tag)
		n, err := hexutil.DecodeUint64(tag)
		if err != nil || n > head {
			return nil, nil
		}
		miner := minerA
		if n%4 == 0 {
			miner = minerB
		}
		return map[string]any{
			"number": hexutil.EncodeUint64(n), "timestamp": hexutil.EncodeUint64(genesis + 600*n),
			"difficulty": "0x574fbde6000", "miner": miner, // 6e12
			"hash": fmt.Sprintf("0x%064x", n), "parentHash": fmt.Sprintf("0x%064x", n-1),
		}, nil
	})
	e.Status.Route("/api/v2/stats", `{"serverTime":1,"blockTime":{"targetSeconds":600,"week":{"averageSeconds":502.17,"blocks":1206,"spanSeconds":604225,"fullWeek":true}},
	  "hashrate":{"hashesPerSecond":10752345347.6},"difficulty":{"latest":"5015400733942","next":"5013575845864","changeWeek":0.603},
	  "decentralization":{"grade":"critical","majorityHolders":1,"distinctMiners":2,"topMiners":[{"name":"rplant.xyz","address":"`+minerA+`","blocks":1027,"share":0.85}]},
	  "nodes":{"total":5,"active":4,"synced":4},"uptime":{}}`)
}

func newEnv(t *testing.T, setup func(*testutil.Env)) *testutil.Env {
	return testutil.NewEnv(t, []func(*server.Registrar){mining.Register}, setup)
}

func TestMiningStatsDerivations(t *testing.T) {
	e := newEnv(t, func(e *testutil.Env) {
		chain(e)
		e.Now = timeAt(genesis + 600*head + 120)
	})
	out := e.MustCall(t, "mining_stats", map[string]any{"blocks": 100})
	if out["seconds_since_last_block"] != float64(120) || out["window"] != "blocks 901-1000" {
		t.Fatalf("head/window: %v %v", out["seconds_since_last_block"], out["window"])
	}
	bt := out["block_time_seconds"].(map[string]any)
	if bt["mean"] != float64(600) || bt["median"] != float64(600) || bt["intervals"] != float64(100) {
		t.Fatalf("block time: %v", bt)
	}
	// 100 blocks × 6e12 hashes over 100 × 600 s = 1e10 H/s.
	if testutil.Get(out, "hashrate_estimate", "hashes_per_second") != 1e10 || testutil.Get(out, "hashrate_estimate", "human") != "10.00 GH/s" {
		t.Fatalf("hashrate: %v", out["hashrate_estimate"])
	}
	if out["expected_block_seconds"] != float64(600) || out["p_block_within_10_minutes"] != 0.6321 {
		t.Fatalf("expected block: %v %v", out["expected_block_seconds"], out["p_block_within_10_minutes"])
	}
	if testutil.Get(out, "miners", 0, "share") != 0.75 || testutil.Get(out, "miners", 0, "pool", "untrusted") != "rplant.xyz" {
		t.Fatalf("miners: %v", out["miners"])
	}
	if testutil.Get(out, "status_page", "next_block_difficulty") != "5013575845864" {
		t.Fatalf("status figures: %v", out["status_page"])
	}
}

func TestMiningStatsStatusDownIsPartial(t *testing.T) {
	e := newEnv(t, chain)
	e.Status.SetFail(503)
	out := e.MustCall(t, "mining_stats", nil)
	if testutil.Get(out, "errors", "status_page") == nil || testutil.Get(out, "hashrate_estimate", "hashes_per_second") != 1e10 {
		t.Fatalf("want node figures plus a status error: %v", out)
	}
	if testutil.Get(out, "miners", 0, "pool") != nil {
		t.Fatal("pool names need the status page")
	}
}

func TestMiningStatsBounds(t *testing.T) {
	e := newEnv(t, chain)
	if _, errText := e.Call(t, "mining_stats", map[string]any{"blocks": 5000}); !strings.Contains(errText, "between 2 and 2016") {
		t.Fatalf("got %q", errText)
	}
}

func TestSupplyInfo(t *testing.T) {
	e := newEnv(t, func(e *testutil.Env) {
		chain(e)
		e.Node.Result("eth_getTotalSupply", "50000000000000000000000") // 1000 × 50 LAX
	})
	out := e.MustCall(t, "supply_info", nil)
	if testutil.Get(out, "issued_node", "lax") != "50000" || out["mismatch"] != nil || testutil.Get(out, "left_to_mine", "lax") != "20950000" {
		t.Fatalf("supply: %v", out)
	}
	h := out["next_halving"].(map[string]any)
	if h["height"] != float64(210000) || h["blocks_to_go"] != float64(209000) || h["observed_pace_seconds_per_block"] != float64(600) {
		t.Fatalf("halving: %v", h)
	}
}

func TestSupplyMismatchAndFallback(t *testing.T) {
	e := newEnv(t, func(e *testutil.Env) {
		chain(e)
		e.Node.Result("eth_getTotalSupply", "1")
	})
	if out := e.MustCall(t, "supply_info", nil); out["mismatch"] != true {
		t.Fatalf("want mismatch: %v", out)
	}
	e2 := newEnv(t, chain) // no eth_getTotalSupply
	out := e2.MustCall(t, "supply_info", nil)
	if !strings.HasPrefix(out["issued_source"].(string), "computed") || testutil.Get(out, "circulating", "lax") != "50000" {
		t.Fatalf("fallback: %v", out)
	}
}

func TestBlockRewardFees(t *testing.T) {
	e := newEnv(t, func(e *testutil.Env) {
		chain(e)
		e.Node.Result("eth_getBlockByNumber", map[string]any{
			"number": "0x384", "hash": fmt.Sprintf("0x%064x", 900), "miner": minerA, "timestamp": "0x1", "difficulty": "0x1",
			"transactions": []string{fmt.Sprintf("0x%064x", 1), fmt.Sprintf("0x%064x", 2)},
		})
		e.Node.Result("eth_getTransactionReceipt", map[string]any{"gasUsed": "0x5208", "effectiveGasPrice": "0x3b9aca00"})
	})
	out := e.MustCall(t, "block_reward", map[string]any{"block": "900"})
	// 2 × 21000 gas × 1 gwei = 0.000042 LAX.
	if testutil.Get(out, "fees", "lax") != "0.000042" || testutil.Get(out, "total", "lax") != "50.000042" {
		t.Fatalf("reward: %v", out)
	}
	if out["matured"] != true || out["spendable_from_block"] != float64(1000) {
		t.Fatalf("maturity: %v", out)
	}
}

func timeAt(unix int64) time.Time { return time.Unix(unix, 0) }
