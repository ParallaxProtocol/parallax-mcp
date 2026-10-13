// Package mining is the mining tool group: difficulty, hashrate, block
// times, rewards and supply, derived from block headers and the protocol's
// emission schedule, cross-checked against the status page.
package mining

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/ParallaxProtocol/parallax/v2/util/hexutil"

	"github.com/ParallaxProtocol/parallax-mcp/internal/config"
	"github.com/ParallaxProtocol/parallax-mcp/internal/evm"
	"github.com/ParallaxProtocol/parallax-mcp/internal/lax"
	"github.com/ParallaxProtocol/parallax-mcp/internal/server"
	"github.com/ParallaxProtocol/parallax-mcp/internal/untrusted"
)

const group = config.GroupMining

const (
	defaultWindow = 144  // one day of blocks at the target
	maxWindow     = 2016 // two weeks
	batchSize     = 250
	maxRewardTxs  = 500
)

// Register adds the mining tools.
func Register(r *server.Registrar) {
	t := &tools{r: r}
	server.Add(r, server.Spec{Group: group, Name: "mining_stats", Title: "Mining statistics",
		Description: `Mining figures derived from the headers of the last "blocks" blocks (default 144, about a day; at most 2016), with how each is worked out:
- seconds_since_last_block: now − the head block's timestamp. Blocks arrive as a Poisson process, so the time already waited says nothing about the wait to come.
- block_time: statistics of the intervals between consecutive timestamps in the window (mean, median, p90, min, max, in seconds) against the 600 second target. Miners set timestamps within protocol limits, so single intervals can be zero or negative; the mean over the window is reliable.
- hashrate_estimate: the sum of the window's block difficulties ÷ the seconds they took (last timestamp − the timestamp of the block before the window). A block takes "difficulty" hashes on average, so this is hashes per second. It swings with luck: over 144 blocks expect roughly ±8% (one standard deviation is 1/√blocks).
- difficulty: the head block's, the window's mean, the change across the window in percent, and pool units (÷ 2^32, as mining pools display it). Difficulty adjusts every block by ASERT, aiming at 600 second blocks.
- expected_block_seconds: head difficulty ÷ hashrate_estimate, the mean wait for the next block at the current hashrate; p_block_within_10_minutes = 1 − e^(−600 ÷ that).
- miners: blocks found per coinbase address in the window with their share, named when the status page knows the pool (names are untrusted).
- status_page: the status page's own figures for comparison: its 7-day block time, its hashrate (over the last 24 hours) and the exact next-block difficulty from its port of the ASERT rule.`,
	}, t.stats)

	server.Add(r, server.Spec{Group: group, Name: "supply_info", Title: "Supply and emission",
		Description: `LAX issuance against the 21,000,000 cap. Parallax follows Bitcoin's schedule: 50 LAX per block from block 1 (genesis pays nothing), halving every 210,000 blocks; there is no premine, burn or protocol lock, so circulating supply equals total issued (block rewards count from the block that pays them, although they are spendable only 100 blocks later). Returns the head height, issued supply in wei and LAX from the node (eth_getTotalSupply) and as recomputed here from the schedule (they must agree; "mismatch" is set if not), its share of the cap, what is left to mine, the current reward and era, daily issuance at the 600 second target (144 blocks), and the next halving: height, blocks to go, the reward after it, and the expected date at the target block time and at the last day's observed pace.`,
	}, t.supply)

	server.Add(r, server.Spec{Group: group, Name: "block_reward", Title: "Block reward",
		Description: `What the miner of a block earned: the subsidy from the emission schedule (50 LAX halved every 210,000 blocks) plus the transaction fees (the sum over its transactions of gas used × effective gas price; Parallax burns nothing). Amounts in wei and LAX. Also the coinbase address, the block's height and era, and whether the reward has matured: it can be spent from 100 blocks after the block (coinbase maturity). Blocks with more than 500 transactions report fees only for the first 500.`,
	}, t.blockReward)
}

type tools struct{ r *server.Registrar }

// header is the part of a header the derivations need.
type header struct {
	Number     uint64
	Timestamp  int64
	Difficulty *big.Int
	Miner      string
}

// headers fetches blocks from..to (inclusive) in batches.
func (t *tools) headers(ctx context.Context, from, to uint64) ([]header, error) {
	out := make([]header, 0, to-from+1)
	for start := from; start <= to; start += batchSize {
		end := min(start+batchSize-1, to)
		raw := make([]evm.RPCHeader, end-start+1)
		batch := make([]server.BatchElem, len(raw))
		for i := range raw {
			batch[i] = server.BatchElem{Method: "eth_getHeaderByNumber", Args: []any{hexutil.EncodeUint64(start + uint64(i))}, Result: &raw[i]}
		}
		if err := t.r.Node.Batch(ctx, batch); err != nil {
			return nil, err
		}
		for i, b := range batch {
			if b.Error != nil {
				return nil, b.Error
			}
			h := raw[i]
			if h.Difficulty == nil {
				return nil, fmt.Errorf("block %d: missing header", start+uint64(i))
			}
			out = append(out, header{Number: uint64(h.Number), Timestamp: int64(h.Timestamp), Difficulty: (*big.Int)(h.Difficulty), Miner: h.Miner.Hex()})
		}
	}
	return out, nil
}

func (t *tools) head(ctx context.Context) (uint64, error) {
	var n hexutil.Uint64
	err := t.r.Node.Call(ctx, &n, "eth_blockNumber")
	return uint64(n), err
}

// ---- mining_stats ----

type statsIn struct {
	Blocks int `json:"blocks,omitempty" jsonschema:"window size in blocks (default 144, at most 2016)"`
}

type blockTimeStats struct {
	Intervals int     `json:"intervals"`
	Mean      float64 `json:"mean"`
	Median    float64 `json:"median"`
	P90       float64 `json:"p90"`
	Min       int64   `json:"min"`
	Max       int64   `json:"max"`
	Target    int     `json:"target"`
}

type minerShare struct {
	Address string          `json:"address"`
	Pool    *untrusted.Text `json:"pool,omitempty"`
	Blocks  int             `json:"blocks"`
	Share   float64         `json:"share"`
}

type statsOut struct {
	server.Partial
	Head                  uint64         `json:"head_block"`
	HeadTimestamp         int64          `json:"head_timestamp"`
	SecondsSinceLastBlock int64          `json:"seconds_since_last_block"`
	Window                string         `json:"window"`
	BlockTime             blockTimeStats `json:"block_time_seconds"`
	Hashrate              hashrate       `json:"hashrate_estimate"`
	Difficulty            map[string]any `json:"difficulty"`
	ExpectedBlockSeconds  float64        `json:"expected_block_seconds"`
	PWithin10Min          float64        `json:"p_block_within_10_minutes"`
	Reward                lax.Amount     `json:"current_block_subsidy"`
	Miners                []minerShare   `json:"miners"`
	StatusPage            map[string]any `json:"status_page,omitempty"`
}

type hashrate struct {
	HashesPerSecond float64 `json:"hashes_per_second"`
	Human           string  `json:"human"`
	SpanSeconds     int64   `json:"span_seconds"`
	Blocks          int     `json:"blocks"`
}

func (t *tools) stats(ctx context.Context, in statsIn) (any, error) {
	n := in.Blocks
	if n == 0 {
		n = defaultWindow
	}
	if n < 2 || n > maxWindow {
		return nil, server.Inputf("blocks must be between 2 and %d", maxWindow)
	}
	head, err := t.head(ctx)
	if err != nil {
		return nil, err
	}
	if head < uint64(n)+1 {
		n = int(head) - 1
	}
	// One extra header before the window gives the first interval.
	hs, err := t.headers(ctx, head-uint64(n), head)
	if err != nil {
		return nil, err
	}
	window := hs[1:]
	last := hs[len(hs)-1]

	out := statsOut{
		Head:                  last.Number,
		HeadTimestamp:         last.Timestamp,
		SecondsSinceLastBlock: t.r.Now().Unix() - last.Timestamp,
		Window:                fmt.Sprintf("blocks %d-%d", window[0].Number, last.Number),
		BlockTime:             intervalStats(hs),
		Reward:                lax.NewAmount(lax.BlockReward(last.Number + 1)),
	}

	sumDiff := new(big.Int)
	for _, h := range window {
		sumDiff.Add(sumDiff, h.Difficulty)
	}
	span := last.Timestamp - hs[0].Timestamp
	if span > 0 {
		hr, _ := new(big.Float).Quo(new(big.Float).SetInt(sumDiff), big.NewFloat(float64(span))).Float64()
		out.Hashrate = hashrate{HashesPerSecond: math.Round(hr), Human: humanRate(hr), SpanSeconds: span, Blocks: len(window)}
		headDiff, _ := new(big.Float).SetInt(last.Difficulty).Float64()
		out.ExpectedBlockSeconds = round(headDiff/hr, 1)
		out.PWithin10Min = round(1-math.Exp(-600/(headDiff/hr)), 4)
	}

	meanDiff := new(big.Int).Quo(sumDiff, big.NewInt(int64(len(window))))
	first, _ := new(big.Float).SetInt(window[0].Difficulty).Float64()
	lastF, _ := new(big.Float).SetInt(last.Difficulty).Float64()
	poolUnits, _ := new(big.Float).Quo(new(big.Float).SetInt(last.Difficulty), big.NewFloat(math.Pow(2, 32))).Float64()
	out.Difficulty = map[string]any{
		"head":                       last.Difficulty.String(),
		"head_pool_units":            round(poolUnits, 2),
		"window_mean":                meanDiff.String(),
		"change_over_window_percent": round((lastF/first-1)*100, 2),
	}

	counts := map[string]int{}
	for _, h := range window {
		counts[h.Miner]++
	}
	pools := map[string]string{}
	snap, serr := t.statusSnapshot(ctx)
	if serr != nil {
		out.Fail("status_page", serr)
	} else {
		for _, m := range snap.topMiners {
			if m.name != "" {
				pools[strings.ToLower(m.address)] = m.name
			}
		}
		out.StatusPage = snap.figures
	}
	for addr, c := range counts {
		ms := minerShare{Address: addr, Blocks: c, Share: round(float64(c)/float64(len(window)), 4)}
		if name := pools[strings.ToLower(addr)]; name != "" {
			ms.Pool = untrusted.Ptr(name)
		}
		out.Miners = append(out.Miners, ms)
	}
	sort.Slice(out.Miners, func(i, j int) bool {
		if out.Miners[i].Blocks != out.Miners[j].Blocks {
			return out.Miners[i].Blocks > out.Miners[j].Blocks
		}
		return out.Miners[i].Address < out.Miners[j].Address
	})
	if len(out.Miners) > 10 {
		out.Miners = out.Miners[:10]
	}
	return out, nil
}

func intervalStats(hs []header) blockTimeStats {
	iv := make([]int64, 0, len(hs)-1)
	for i := 1; i < len(hs); i++ {
		iv = append(iv, hs[i].Timestamp-hs[i-1].Timestamp)
	}
	s := blockTimeStats{Intervals: len(iv), Target: lax.TargetBlockSeconds}
	if len(iv) == 0 {
		return s
	}
	var sum int64
	for _, v := range iv {
		sum += v
	}
	sorted := append([]int64(nil), iv...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	s.Mean = round(float64(sum)/float64(len(iv)), 1)
	s.Median = percentile(sorted, 0.5)
	s.P90 = percentile(sorted, 0.9)
	s.Min, s.Max = sorted[0], sorted[len(sorted)-1]
	return s
}

// percentile interpolates linearly between closest ranks.
func percentile(sorted []int64, p float64) float64 {
	if len(sorted) == 1 {
		return float64(sorted[0])
	}
	pos := p * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := min(lo+1, len(sorted)-1)
	frac := pos - float64(lo)
	return round(float64(sorted[lo])+(float64(sorted[hi])-float64(sorted[lo]))*frac, 1)
}

func humanRate(hps float64) string {
	units := []string{"H/s", "kH/s", "MH/s", "GH/s", "TH/s", "PH/s", "EH/s"}
	i := 0
	for hps >= 1000 && i < len(units)-1 {
		hps /= 1000
		i++
	}
	return fmt.Sprintf("%.2f %s", hps, units[i])
}

type statusFigures struct {
	figures   map[string]any
	topMiners []struct{ address, name string }
	weekAvg   float64
}

// statusSnapshot extracts the status page figures the mining tools compare
// against.
func (t *tools) statusSnapshot(ctx context.Context) (*statusFigures, error) {
	if t.r.Status == nil {
		return nil, fmt.Errorf("no status page is configured")
	}
	snap, err := t.r.Status.Stats(ctx)
	if err != nil {
		return nil, err
	}
	s := snap.Stats
	f := &statusFigures{figures: map[string]any{"fetched_at": snap.FetchedAt.UTC().Format(time.RFC3339)}}
	if snap.Stale {
		f.figures["stale"] = true
	}
	if w := s.BlockTime.Week; w != nil {
		f.figures["block_time_7d_mean_seconds"] = round(w.AverageSeconds, 1)
		f.figures["block_time_7d_blocks"] = w.Blocks
		f.weekAvg = w.AverageSeconds
	}
	if s.Hashrate.HashesPerSecond != nil {
		f.figures["hashrate_24h"] = humanRate(*s.Hashrate.HashesPerSecond)
		f.figures["hashrate_24h_hashes_per_second"] = math.Round(*s.Hashrate.HashesPerSecond)
	}
	if s.Difficulty.Next != nil {
		f.figures["next_block_difficulty"] = *s.Difficulty.Next
	}
	if s.Difficulty.ChangeWeek != nil {
		f.figures["difficulty_change_7d_percent"] = round(*s.Difficulty.ChangeWeek*100, 2)
	}
	if d := s.Decentralization; d != nil {
		f.figures["decentralization_grade_7d"] = d.Grade
		for _, m := range d.TopMiners {
			name := ""
			if m.Name != nil {
				name = *m.Name
			}
			f.topMiners = append(f.topMiners, struct{ address, name string }{m.Address, name})
		}
	}
	return f, nil
}

// ---- supply_info ----

type noIn struct{}

func (t *tools) supply(ctx context.Context, _ noIn) (any, error) {
	var head hexutil.Uint64
	var nodeSupply string
	batch := []server.BatchElem{
		{Method: "eth_blockNumber", Result: &head},
		{Method: "eth_getTotalSupply", Result: &nodeSupply},
	}
	if err := t.r.Node.Batch(ctx, batch); err != nil {
		return nil, err
	}
	if batch[0].Error != nil {
		return nil, batch[0].Error
	}
	var p server.Partial
	h := uint64(head)
	computed := lax.IssuedThrough(h)
	out := map[string]any{
		"head_block":      h,
		"issued_computed": lax.NewAmount(computed),
		"cap":             lax.NewAmount(lax.SupplyCap),
	}
	issued := computed
	if batch[1].Error != nil {
		p.Fail("eth_getTotalSupply", batch[1].Error)
		out["issued_source"] = "computed from the schedule (the node does not serve eth_getTotalSupply)"
	} else if v, ok := new(big.Int).SetString(nodeSupply, 10); ok {
		out["issued_node"] = lax.NewAmount(v)
		out["issued_source"] = "node (eth_getTotalSupply)"
		if v.Cmp(computed) != 0 {
			// The node's head can move between the two calls of the batch.
			if v.Cmp(lax.IssuedThrough(h+1)) != 0 {
				out["mismatch"] = true
			}
		}
		issued = v
	}
	left := new(big.Int).Sub(lax.SupplyCap, issued)
	share, _ := new(big.Rat).SetFrac(issued, lax.SupplyCap).Float64()
	next := lax.NextHalving(h)
	blocksToGo := next - h
	reward := lax.BlockReward(h + 1)
	out["issued_share_of_cap"] = round(share, 6)
	out["left_to_mine"] = lax.NewAmount(left)
	out["circulating"] = lax.NewAmount(issued)
	out["era"] = lax.Era(h + 1)
	out["block_subsidy"] = lax.NewAmount(reward)
	out["daily_issuance_at_target"] = lax.NewAmount(new(big.Int).Mul(reward, big.NewInt(144)))
	now := t.r.Now()
	halving := map[string]any{
		"height":                  next,
		"blocks_to_go":            blocksToGo,
		"subsidy_after":           lax.NewAmount(lax.BlockReward(next)),
		"expected_at_target_pace": now.Add(time.Duration(blocksToGo) * 10 * time.Minute).UTC().Format(time.RFC3339),
	}
	if pace, err := t.recentPace(ctx, h); err == nil && pace > 0 {
		halving["observed_pace_seconds_per_block"] = round(pace, 1)
		halving["expected_at_observed_pace"] = now.Add(time.Duration(float64(blocksToGo)*pace) * time.Second).UTC().Format(time.RFC3339)
	} else if err != nil {
		p.Fail("pace", err)
	}
	out["next_halving"] = halving
	if p.Errors != nil {
		out["errors"] = p.Errors
	}
	return out, nil
}

// recentPace is the mean block time over the last 144 blocks, from two
// headers.
func (t *tools) recentPace(ctx context.Context, head uint64) (float64, error) {
	if head < defaultWindow {
		return 0, nil
	}
	var a, b evm.RPCHeader
	batch := []server.BatchElem{
		{Method: "eth_getHeaderByNumber", Args: []any{hexutil.EncodeUint64(head - defaultWindow)}, Result: &a},
		{Method: "eth_getHeaderByNumber", Args: []any{hexutil.EncodeUint64(head)}, Result: &b},
	}
	if err := t.r.Node.Batch(ctx, batch); err != nil {
		return 0, err
	}
	for _, e := range batch {
		if e.Error != nil {
			return 0, e.Error
		}
	}
	return float64(int64(b.Timestamp)-int64(a.Timestamp)) / defaultWindow, nil
}

// ---- block_reward ----

type rewardIn struct {
	Block string `json:"block,omitempty" jsonschema:"block number, hash or \"latest\" (default)"`
}

func (t *tools) blockReward(ctx context.Context, in rewardIn) (any, error) {
	ref, err := evm.ParseBlockRef(in.Block)
	if err != nil || ref.Tag == "pending" {
		return nil, server.Inputf("block: use a number, a hash or \"latest\"")
	}
	var raw json.RawMessage
	if ref.Hash != nil {
		raw, err = t.r.Node.Raw(ctx, "eth_getBlockByHash", ref.Hash.Hex(), false)
	} else {
		raw, err = t.r.Node.Raw(ctx, "eth_getBlockByNumber", ref.Tag, false)
	}
	if err != nil {
		return nil, err
	}
	if string(raw) == "null" || len(raw) == 0 {
		return nil, server.Inputf("block not found")
	}
	var b evm.RPCBlock
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	head, err := t.head(ctx)
	if err != nil {
		return nil, err
	}

	hashes := b.Transactions
	truncated := len(hashes) > maxRewardTxs
	if truncated {
		hashes = hashes[:maxRewardTxs]
	}
	type receipt struct {
		GasUsed           hexutil.Uint64 `json:"gasUsed"`
		EffectiveGasPrice *hexutil.Big   `json:"effectiveGasPrice"`
	}
	rcpts := make([]receipt, len(hashes))
	var batch []server.BatchElem
	for i, h := range hashes {
		var hash string
		_ = json.Unmarshal(h, &hash)
		batch = append(batch, server.BatchElem{Method: "eth_getTransactionReceipt", Args: []any{hash}, Result: &rcpts[i]})
	}
	for start := 0; start < len(batch); start += batchSize {
		if err := t.r.Node.Batch(ctx, batch[start:min(start+batchSize, len(batch))]); err != nil {
			return nil, err
		}
	}
	fees := new(big.Int)
	for i, e := range batch {
		if e.Error != nil {
			return nil, e.Error
		}
		if rcpts[i].EffectiveGasPrice != nil {
			fees.Add(fees, new(big.Int).Mul((*big.Int)(rcpts[i].EffectiveGasPrice), new(big.Int).SetUint64(uint64(rcpts[i].GasUsed))))
		}
	}
	num := uint64(b.Number)
	subsidy := lax.BlockReward(num)
	out := map[string]any{
		"block":                num,
		"hash":                 b.Hash.Hex(),
		"miner":                b.Miner.Hex(),
		"era":                  lax.Era(num),
		"subsidy":              lax.NewAmount(subsidy),
		"fees":                 lax.NewAmount(fees),
		"total":                lax.NewAmount(new(big.Int).Add(subsidy, fees)),
		"transaction_count":    len(b.Transactions),
		"spendable_from_block": num + lax.CoinbaseMaturity,
		"matured":              head >= num+lax.CoinbaseMaturity,
	}
	if truncated {
		out["fees_cover_first_transactions"] = maxRewardTxs
	}
	return out, nil
}

func round(f float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(f*p) / p
}
