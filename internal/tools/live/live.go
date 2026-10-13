// Package live registers what follows the chain as it moves: the
// subscribable parallax://chain/head and parallax://network/alerts
// resources, and the wait_for_new_block tool for clients without resource
// subscriptions.
package live

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ParallaxProtocol/parallax-mcp/internal/config"
	"github.com/ParallaxProtocol/parallax-mcp/internal/server"
	"github.com/ParallaxProtocol/parallax-mcp/internal/watch"
)

const (
	defaultWait = 120 * time.Second
	maxWait     = 600 * time.Second
	// statelessMaxWait bounds waits on --http-stateless, whose plain JSON
	// responses carry no keep-alives: proxies close silent responses after
	// 60 to 100 seconds.
	statelessMaxWait = 90 * time.Second
	maxWaiters       = 32
)

// Register adds the live resources and, with the chain group, the
// wait_for_new_block tool.
func Register(r *server.Registrar) {
	w := watch.New(r.Node, r.Status, r.MCP(), r.Cfg.HeadPollInterval, r.Log)
	s := r.MCP()

	s.AddResource(&mcp.Resource{
		URI: watch.HeadURI, Name: "chain-head", Title: "Chain head", MIMEType: "application/json",
		Description: "The latest block: number, hash, parent, timestamp, miner, difficulty, gas used, transaction count and subsidy, plus a reorg marker when it replaced a block the server had seen. Subscribe to it to be notified of every new block (about one every 10 minutes).",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		h := w.Latest()
		if h == nil || time.Since(h.SeenAt) > r.Cfg.HeadPollInterval {
			fresh, err := watch.FetchHead(ctx, r.Node, "latest")
			if err != nil {
				return nil, err
			}
			if h == nil || fresh.Hash != h.Hash {
				h = fresh
			}
		}
		return jsonResource(watch.HeadURI, h)
	})
	r.Subscribable(watch.HeadURI, w.Interest)

	if r.Status != nil {
		s.AddResource(&mcp.Resource{
			URI: watch.AlertsURI, Name: "network-alerts", Title: "Network alerts", MIMEType: "application/json",
			Description: "The status page's active alerts (chain split, stall, deep reorg, mining majority, slow propagation, outdated clients) with level and details (strings in details are untrusted). Subscribe to it to be notified when an alert appears, clears or changes level.",
		}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			doc, _, err := watch.FetchAlerts(ctx, r.Status)
			if err != nil {
				return nil, err
			}
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: watch.AlertsURI, MIMEType: "application/json", Text: string(doc)}}}, nil
		})
		r.Subscribable(watch.AlertsURI, w.Interest)
	}

	waitLimit, waitDefault := maxWait, defaultWait
	if r.Cfg.Transport == config.TransportHTTP && r.Cfg.HTTPStateless {
		waitLimit, waitDefault = statelessMaxWait, statelessMaxWait
	}
	waiters := make(chan struct{}, maxWaiters)
	server.Add(r, server.Spec{Group: config.GroupChain, Name: "wait_for_new_block", Title: "Wait for a new block",
		Timeout: waitLimit + 30*time.Second,
		Description: `Waits until the chain has a block above "after_block" (default: the current head) and returns it: number, hash, parent hash, timestamp, miner, difficulty, gas used, transaction count and subsidy, with a reorg marker if the tip was replaced instead. Blocks come every 10 minutes on average, but intervals vary widely, so a wait can time out: then it returns "new_block": false with the current head, and you can call it again.
"timeout_seconds" defaults to 120, at most 600 (90 on servers running without HTTP sessions). For clients that support resource subscriptions, subscribing to parallax://chain/head is the better way to follow the chain. Use this to wait for a transaction's next confirmation, then check it with get_receipt.`,
	}, func(ctx context.Context, in waitIn) (any, error) {
		timeout := waitDefault
		if in.TimeoutSeconds != 0 {
			if in.TimeoutSeconds < 1 || in.TimeoutSeconds > int(waitLimit/time.Second) {
				return nil, server.Inputf("timeout_seconds must be between 1 and %d on this server", int(waitLimit/time.Second))
			}
			timeout = time.Duration(in.TimeoutSeconds) * time.Second
		}
		select {
		case waiters <- struct{}{}:
			defer func() { <-waiters }()
		default:
			return nil, fmt.Errorf("too many clients are waiting for blocks; retry shortly or subscribe to %s", watch.HeadURI)
		}

		after := in.AfterBlock
		current, err := watch.FetchHead(ctx, r.Node, "latest")
		if err != nil {
			return nil, err
		}
		if after == nil {
			n := current.Number
			after = &n
		} else if current.Number > *after {
			return waitOut{NewBlock: true, Block: current, WaitedSeconds: 0}, nil
		}

		req := server.Request(ctx)
		start := time.Now()
		h, err := w.WaitAfter(ctx, *after, timeout, func(waited time.Duration) {
			// Progress notifications keep proxies from closing an idle request.
			if req != nil && req.Params.GetProgressToken() != nil {
				_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
					ProgressToken: req.Params.GetProgressToken(),
					Progress:      waited.Seconds(),
					Total:         timeout.Seconds(),
					Message:       fmt.Sprintf("waiting for a block above %d", *after),
				})
			}
		})
		if err != nil {
			return nil, err
		}
		out := waitOut{WaitedSeconds: int64(time.Since(start).Seconds())}
		if h == nil {
			out.Head = w.Latest()
			if out.Head == nil {
				out.Head = current
			}
			out.Note = "no new block yet; blocks average 10 minutes but often take longer. Call again to keep waiting."
			return out, nil
		}
		out.NewBlock, out.Block = true, h
		return out, nil
	})
}

type waitIn struct {
	AfterBlock     *uint64 `json:"after_block,omitempty" jsonschema:"return the first block above this height (default: the current head)"`
	TimeoutSeconds int     `json:"timeout_seconds,omitempty" jsonschema:"how long to wait, 1-600 seconds (default 120)"`
}

type waitOut struct {
	NewBlock      bool        `json:"new_block"`
	Block         *watch.Head `json:"block,omitempty"`
	Head          *watch.Head `json:"current_head,omitempty"`
	WaitedSeconds int64       `json:"waited_seconds"`
	Note          string      `json:"note,omitempty"`
}

func jsonResource(uri string, v any) (*mcp.ReadResourceResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: "application/json", Text: string(b)}}}, nil
}
