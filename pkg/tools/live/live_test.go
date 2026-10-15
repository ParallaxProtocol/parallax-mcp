package live_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ParallaxProtocol/parallax/v2/util/hexutil"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ParallaxProtocol/parallax-mcp/internal/testutil"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/config"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/server"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/tools/live"
)

// chain is a fake node whose head the test advances.
func chain(head *atomic.Uint64) func(*testutil.Env) {
	return func(e *testutil.Env) {
		e.Node.Handle("eth_getBlockByNumber", func(p []json.RawMessage) (any, *testutil.RPCError) {
			n := head.Load()
			return map[string]any{
				"number": hexutil.EncodeUint64(n), "hash": fmt.Sprintf("0x%064x", n), "parentHash": fmt.Sprintf("0x%064x", n-1),
				"timestamp": hexutil.EncodeUint64(1_700_000_000 + 600*n), "miner": "0x38432a328b3c6f15582292f7235f17b08a98c6ab",
				"difficulty": "0x1", "gasUsed": "0x0", "transactions": []string{},
			}, nil
		})
	}
}

func newEnv(t *testing.T, head *atomic.Uint64) *testutil.Env {
	return testutil.NewEnv(t, []func(*server.Registrar){live.Register}, chain(head), func(c *config.Config) {
		c.HeadPollInterval = 50 * time.Millisecond
	})
}

func TestSubscribersAreNotifiedOfNewBlocks(t *testing.T) {
	var head atomic.Uint64
	head.Store(100)
	e := newEnv(t, &head)

	updates := make(chan string, 8)
	client := mcp.NewClient(&mcp.Implementation{Name: "sub", Version: "0"}, &mcp.ClientOptions{
		ResourceUpdatedHandler: func(_ context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			updates <- req.Params.URI
		},
	})
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := e.Reg.MCP().Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	if err := cs.Subscribe(ctx, &mcp.SubscribeParams{URI: "parallax://chain/head"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // the watcher sees block 100 first
	head.Store(101)
	select {
	case uri := <-updates:
		if uri != "parallax://chain/head" {
			t.Fatalf("notified about %s", uri)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no notification for the new block")
	}
	res, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "parallax://chain/head"})
	if err != nil {
		t.Fatal(err)
	}
	var h struct{ Number uint64 }
	_ = json.Unmarshal([]byte(res.Contents[0].Text), &h)
	if h.Number != 101 {
		t.Fatalf("head resource says %d", h.Number)
	}
}

func TestWaitForNewBlock(t *testing.T) {
	var head atomic.Uint64
	head.Store(500)
	e := newEnv(t, &head)

	go func() {
		time.Sleep(300 * time.Millisecond)
		head.Store(501)
	}()
	out := e.MustCall(t, "wait_for_new_block", map[string]any{"timeout_seconds": 10})
	if out["new_block"] != true || testutil.Get(out, "block", "number") != float64(501) {
		t.Fatalf("got %v", out)
	}

	// Already past: answers at once.
	out = e.MustCall(t, "wait_for_new_block", map[string]any{"after_block": 400})
	if out["new_block"] != true || out["waited_seconds"] != float64(0) {
		t.Fatalf("got %v", out)
	}

	// Nothing comes: times out with the current head.
	out = e.MustCall(t, "wait_for_new_block", map[string]any{"timeout_seconds": 1})
	if out["new_block"] != false || testutil.Get(out, "current_head", "number") != float64(501) {
		t.Fatalf("got %v", out)
	}
	if _, errText := e.Call(t, "wait_for_new_block", map[string]any{"timeout_seconds": 3600}); errText == "" {
		t.Fatal("a one hour wait was accepted")
	}
}
