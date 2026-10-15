// Package watch follows the chain head and the status page's alerts, for
// clients that subscribe to parallax://chain/head and
// parallax://network/alerts and for the wait_for_new_block tool.
//
// Polling starts with the first subscription or waiting call and stops
// after ten minutes without either, so an idle server makes no requests.
package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ParallaxProtocol/parallax/v2/util/hexutil"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ParallaxProtocol/parallax-mcp/pkg/backend/node"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/backend/status"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/evm"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/lax"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/untrusted"
)

// Resource URIs clients can subscribe to.
const (
	HeadURI   = "parallax://chain/head"
	AlertsURI = "parallax://network/alerts"
)

// idleStop is how long polling continues after the last interest.
const idleStop = 10 * time.Minute

// Head is the latest block as the watcher saw it.
type Head struct {
	Number       uint64     `json:"number"`
	Hash         string     `json:"hash"`
	ParentHash   string     `json:"parent_hash"`
	Timestamp    int64      `json:"timestamp"`
	TimestampUTC string     `json:"timestamp_utc"`
	Miner        string     `json:"miner"`
	Difficulty   string     `json:"difficulty"`
	GasUsed      uint64     `json:"gas_used"`
	TxCount      int        `json:"transaction_count"`
	Subsidy      lax.Amount `json:"block_subsidy"`
	// Reorg is set when this head replaced a block at the same or a higher
	// height that the watcher had seen.
	Reorg *Reorg `json:"reorg,omitempty"`
	// SeenAt is when the watcher first saw this block.
	SeenAt time.Time `json:"seen_at,omitzero"`
}

// Reorg describes a replaced head.
type Reorg struct {
	ReplacedNumber uint64 `json:"replaced_number"`
	ReplacedHash   string `json:"replaced_hash"`
}

// Watcher polls the node and the status page.
type Watcher struct {
	node     *node.Client
	status   *status.Client
	srv      *mcp.Server
	interval time.Duration
	log      *slog.Logger
	now      func() time.Time

	mu        sync.Mutex
	running   bool
	interest  time.Time
	head      *Head
	changed   chan struct{} // closed and replaced on every new head
	alerts    string        // fingerprint of the last alert set
	alertsDoc json.RawMessage
}

// New returns a Watcher; nothing is polled until Interest is called.
func New(n *node.Client, st *status.Client, srv *mcp.Server, interval time.Duration, log *slog.Logger) *Watcher {
	return &Watcher{node: n, status: st, srv: srv, interval: interval, log: log, now: time.Now, changed: make(chan struct{})}
}

// Interest starts polling, or keeps it running.
func (w *Watcher) Interest() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.interest = w.now()
	if w.running {
		return
	}
	w.running = true
	go w.loop()
}

func (w *Watcher) loop() {
	w.log.Info("watching the chain head", "interval", w.interval)
	tick := time.NewTicker(w.interval)
	defer tick.Stop()
	alertTick := time.NewTicker(max(w.interval*4, time.Minute))
	defer alertTick.Stop()
	w.pollHead()
	w.pollAlerts()
	for {
		select {
		case <-tick.C:
			w.pollHead()
		case <-alertTick.C:
			w.pollAlerts()
		}
		w.mu.Lock()
		if w.now().Sub(w.interest) > idleStop {
			w.running = false
			w.mu.Unlock()
			w.log.Info("stopped watching the chain head: no subscribers or waiters")
			return
		}
		w.mu.Unlock()
	}
}

func (w *Watcher) pollHead() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	h, err := FetchHead(ctx, w.node, "latest")
	if err != nil {
		w.log.Warn("polling the head", "err", err)
		return
	}
	w.mu.Lock()
	prev := w.head
	if prev != nil && prev.Hash == h.Hash {
		w.mu.Unlock()
		return
	}
	if prev != nil && h.Number <= prev.Number {
		h.Reorg = &Reorg{ReplacedNumber: prev.Number, ReplacedHash: prev.Hash}
	}
	h.SeenAt = w.now()
	w.head = h
	close(w.changed)
	w.changed = make(chan struct{})
	w.mu.Unlock()

	if prev != nil {
		w.log.Info("new head", "number", h.Number, "hash", h.Hash, "reorg", h.Reorg != nil)
		_ = w.srv.ResourceUpdated(ctx, &mcp.ResourceUpdatedNotificationParams{URI: HeadURI})
	}
}

func (w *Watcher) pollAlerts() {
	if w.status == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	doc, fp, err := FetchAlerts(ctx, w.status)
	if err != nil {
		w.log.Warn("polling alerts", "err", err)
		return
	}
	w.mu.Lock()
	first := w.alertsDoc == nil
	changed := fp != w.alerts
	w.alerts, w.alertsDoc = fp, doc
	w.mu.Unlock()
	if changed && !first {
		w.log.Info("alerts changed", "alerts", fp)
		_ = w.srv.ResourceUpdated(ctx, &mcp.ResourceUpdatedNotificationParams{URI: AlertsURI})
	}
}

// Latest returns the newest head the watcher has seen, or nil.
func (w *Watcher) Latest() *Head {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.head
}

// WaitAfter blocks until a head above number (or any different head, when a
// reorg replaces the tip) is seen, ctx ends or timeout passes. It returns
// the head, or nil on timeout. tick, when not nil, is called every 15
// seconds while waiting.
func (w *Watcher) WaitAfter(ctx context.Context, number uint64, timeout time.Duration, tick func(waited time.Duration)) (*Head, error) {
	w.Interest()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	progress := time.NewTicker(15 * time.Second)
	defer progress.Stop()
	start := w.now()
	for {
		w.mu.Lock()
		h, ch := w.head, w.changed
		w.interest = w.now()
		w.mu.Unlock()
		if h != nil && (h.Number > number || (h.Reorg != nil && h.SeenAt.After(start))) {
			return h, nil
		}
		select {
		case <-ch:
		case <-progress.C:
			if tick != nil {
				tick(w.now().Sub(start))
			}
		case <-deadline.C:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// FetchHead reads a block header into a Head.
func FetchHead(ctx context.Context, n *node.Client, tag string) (*Head, error) {
	var b struct {
		evm.RPCHeader
		Transactions []json.RawMessage `json:"transactions"`
	}
	raw, err := n.Raw(ctx, "eth_getBlockByNumber", tag, false)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &b); err != nil || b.Difficulty == nil {
		return nil, fmt.Errorf("decoding block %s", tag)
	}
	return &Head{
		Number:       uint64(b.Number),
		Hash:         b.Hash.Hex(),
		ParentHash:   b.ParentHash.Hex(),
		Timestamp:    int64(b.Timestamp),
		TimestampUTC: time.Unix(int64(b.Timestamp), 0).UTC().Format(time.RFC3339),
		Miner:        b.Miner.Hex(),
		Difficulty:   (*hexutil.Big)(b.Difficulty).ToInt().String(),
		GasUsed:      uint64(b.GasUsed),
		TxCount:      len(b.Transactions),
		Subsidy:      lax.NewAmount(lax.BlockReward(uint64(b.Number))),
	}, nil
}

// FetchAlerts returns the status page's active alerts and ongoing
// incidents as a JSON document, with a fingerprint that changes when an
// alert appears, clears or changes level.
func FetchAlerts(ctx context.Context, st *status.Client) (json.RawMessage, string, error) {
	snap, err := st.Stats(ctx)
	if err != nil {
		return nil, "", err
	}
	type alert struct {
		Type    string `json:"type"`
		Level   string `json:"level"`
		Since   *int64 `json:"since,omitempty"`
		Details any    `json:"details,omitempty"`
	}
	var list []alert
	var keys []string
	for _, a := range snap.Alerts {
		list = append(list, alert{a.Type, a.Level, a.Since, untrusted.WrapJSON(a.Details)})
		keys = append(keys, a.Type+":"+a.Level)
	}
	sort.Strings(keys)
	doc, _ := json.Marshal(map[string]any{
		"fetched_at": snap.FetchedAt.UTC().Format(time.RFC3339),
		"stale":      snap.Stale,
		"alerts":     list,
	})
	return doc, strings.Join(keys, ","), nil
}
