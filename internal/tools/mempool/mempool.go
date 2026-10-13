// Package mempool is the mempool tool group: transactions waiting to be
// mined, from the node's txpool namespace and the status page.
package mempool

import (
	"context"
	"errors"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"github.com/ParallaxProtocol/parallax/v2/util/hexutil"

	"github.com/ParallaxProtocol/parallax-mcp/internal/config"
	"github.com/ParallaxProtocol/parallax-mcp/internal/evm"
	"github.com/ParallaxProtocol/parallax-mcp/internal/lax"
	"github.com/ParallaxProtocol/parallax-mcp/internal/server"
)

const group = config.GroupMempool

const maxListed = 200

// Register adds the mempool tools.
func Register(r *server.Registrar) {
	t := &tools{r: r}
	server.Add(r, server.Spec{Group: group, Name: "mempool_status", Title: "Mempool status",
		Description: `Transactions waiting to be mined. From the configured node (txpool namespace): "pending" (executable now) and "queued" (waiting for an earlier nonce) counts. From the status page: the median pending count across synced reporting nodes, "pressure" (the block space the waiting transactions need, as a share of one block: under 0.5 is light, over 2 is a backlog) and blocks to clear; plus the node's current gas price. With 10 minute blocks and large gas limits, Parallax's mempool is usually near empty. Either source may be missing, with its failure in "errors".`,
	}, t.status)

	server.Add(r, server.Spec{Group: group, Name: "mempool_transactions", Title: "Mempool transactions", Requires: "txpool",
		Description: `Transactions in the configured node's mempool (needs the txpool namespace), at most 200, highest gas price first, each with sender, nonce, recipient, value (wei and LAX), gas limit, gas price (wei and gwei) and whether it is pending (executable) or queued (blocked by a nonce gap). Hashes are included only with "address", which lists that sender's transactions (txpool_contentFrom).`,
	}, t.transactions)
}

type tools struct{ r *server.Registrar }

type noIn struct{}

func (t *tools) status(ctx context.Context, _ noIn) (any, error) {
	var p server.Partial
	out := map[string]any{}

	var st struct {
		Pending hexutil.Uint64 `json:"pending"`
		Queued  hexutil.Uint64 `json:"queued"`
	}
	var price hexutil.Big
	if t.r.HasModule("txpool") {
		batch := []server.BatchElem{
			{Method: "txpool_status", Result: &st},
			{Method: "eth_gasPrice", Result: &price},
		}
		if err := t.r.Node.Batch(ctx, batch); err != nil {
			p.Fail("node", err)
		} else {
			if batch[0].Error != nil {
				p.Fail("txpool_status", batch[0].Error)
			} else {
				out["node"] = map[string]any{"pending": uint64(st.Pending), "queued": uint64(st.Queued)}
			}
			if batch[1].Error == nil {
				out["gas_price_wei"] = (*big.Int)(&price).String()
			}
		}
	} else {
		out["node"] = "the configured node does not serve the txpool namespace"
		if err := t.r.Node.Call(ctx, &price, "eth_gasPrice"); err == nil {
			out["gas_price_wei"] = (*big.Int)(&price).String()
		}
	}

	if t.r.Status == nil {
		p.Fail("status_page", errNoStatus)
	} else if snap, err := t.r.Status.Stats(ctx); err != nil {
		p.Fail("status_page", err)
	} else if m := snap.Mempool; m != nil {
		out["network"] = map[string]any{"pending_median": m.Pending, "pressure_blocks": m.Pressure, "blocks_to_clear": m.BlocksToClear}
	}
	if p.Errors != nil {
		out["errors"] = p.Errors
	}
	return out, nil
}

var errNoStatus = errors.New("no status page is configured")

type txsIn struct {
	Address string `json:"address,omitempty" jsonschema:"only this sender's transactions"`
}

type poolTx struct {
	From         string     `json:"from"`
	Nonce        uint64     `json:"nonce"`
	Hash         string     `json:"hash,omitempty"`
	To           string     `json:"to,omitempty"`
	Value        lax.Amount `json:"value"`
	Gas          uint64     `json:"gas_limit"`
	GasPriceWei  string     `json:"gas_price_wei"`
	GasPriceGwei string     `json:"gas_price_gwei"`
	State        string     `json:"state"`
	price        *big.Int
}

func (t *tools) transactions(ctx context.Context, in txsIn) (any, error) {
	if in.Address != "" {
		addr, err := evm.ParseAddress(in.Address)
		if err != nil {
			return nil, server.Inputf("%v", err)
		}
		var content map[string]map[string]evm.RPCTx
		if err := t.r.Node.Call(ctx, &content, "txpool_contentFrom", addr.Hex()); err != nil {
			return nil, err
		}
		list := []poolTx{}
		for state, byNonce := range content {
			for _, tx := range byNonce {
				pt := poolTx{From: tx.From.Hex(), Nonce: uint64(tx.Nonce), Hash: tx.Hash.Hex(), Value: lax.NewAmount((*big.Int)(tx.Value)),
					Gas: uint64(tx.Gas), State: state, price: (*big.Int)(tx.GasPrice)}
				if tx.To != nil {
					pt.To = tx.To.Hex()
				}
				list = append(list, finish(pt))
			}
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Nonce < list[j].Nonce })
		return map[string]any{"address": addr.Hex(), "count": len(list), "transactions": list}, nil
	}

	// txpool_inspect summarizes every transaction as text:
	// "0xTo: 1000 wei + 21000 gas × 1000000000 wei".
	var inspect map[string]map[string]map[string]string
	if err := t.r.Node.Call(ctx, &inspect, "txpool_inspect"); err != nil {
		return nil, err
	}
	list := []poolTx{}
	total := 0
	for state, bySender := range inspect {
		for from, byNonce := range bySender {
			for nonceStr, summary := range byNonce {
				total++
				nonce, _ := strconv.ParseUint(nonceStr, 10, 64)
				pt, ok := parseInspect(summary)
				if !ok {
					continue
				}
				pt.From, pt.Nonce, pt.State = from, nonce, state
				list = append(list, finish(pt))
			}
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if c := list[i].price.Cmp(list[j].price); c != 0 {
			return c > 0
		}
		return list[i].Nonce < list[j].Nonce
	})
	out := map[string]any{"count": total}
	if len(list) > maxListed {
		list = list[:maxListed]
		out["truncated"] = true
	}
	out["transactions"] = list
	return out, nil
}

func finish(pt poolTx) poolTx {
	if pt.price == nil {
		pt.price = new(big.Int)
	}
	pt.GasPriceWei = pt.price.String()
	pt.GasPriceGwei = lax.FormatUnits(pt.price, 9)
	return pt
}

// parseInspect reads "0xTo: 1000 wei + 21000 gas × 1000000000 wei" (or
// "contract creation: …").
func parseInspect(s string) (poolTx, bool) {
	to, rest, ok := strings.Cut(s, ": ")
	if !ok {
		return poolTx{}, false
	}
	f := strings.Fields(rest)
	// value "wei" "+" gas "gas" "×" price "wei"
	if len(f) < 7 {
		return poolTx{}, false
	}
	value, ok1 := new(big.Int).SetString(f[0], 10)
	gas, err := strconv.ParseUint(f[3], 10, 64)
	price, ok2 := new(big.Int).SetString(f[6], 10)
	if !ok1 || !ok2 || err != nil {
		return poolTx{}, false
	}
	pt := poolTx{Value: lax.NewAmount(value), Gas: gas, price: price}
	if strings.HasPrefix(to, "0x") {
		pt.To = to
	}
	return pt, true
}
