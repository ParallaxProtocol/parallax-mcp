package chain

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"

	"github.com/ParallaxProtocol/parallax/v2/script/abi"
	"github.com/ParallaxProtocol/parallax/v2/util"
	"github.com/ParallaxProtocol/parallax/v2/util/hexutil"

	"github.com/ParallaxProtocol/parallax-mcp/pkg/evm"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/lax"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/server"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/untrusted"
)

const (
	approvalMaxLogs     = 20_000
	approvalMaxChunks   = 100
	approvalMaxSpenders = 25
	callBatch           = 100
)

var (
	topicApproval       = util.BytesToHash(keccak([]byte("Approval(address,address,uint256)")))
	topicApprovalForAll = util.BytesToHash(keccak([]byte("ApprovalForAll(address,address,bool)")))
	// unlimitedFloor: allowances at or above 2^128 are "unlimited" in
	// practice; wallets approve 2^256-1.
	unlimitedFloor = new(big.Int).Lsh(big.NewInt(1), 128)

	mAllowance   = mustSig("allowance(address,address) returns (uint256)")
	mBalanceOf   = mustSig("balanceOf(address) returns (uint256)")
	mIsApproved  = mustSig("isApprovedForAll(address,address) returns (bool)")
	mGetApproved = mustSig("getApproved(uint256) returns (address)")
	mOwnerOf     = mustSig("ownerOf(uint256) returns (address)")
	mSymbol      = mustSig("symbol() returns (string)")
	mDecimals    = mustSig("decimals() returns (uint8)")
)

func mustSig(s string) abi.Method {
	m, err := evm.ParseSignature(s)
	if err != nil {
		panic(err)
	}
	return m
}

const approvalsDescription = `Audits the token approvals an address has granted: who may move its tokens.
How: scans the chain for the owner's Approval and ApprovalForAll events (from "from_block", default the genesis block, in ranges of the server's log range limit), keeps the latest per token and spender, and checks each against the contract's current state with eth_call (allowance, isApprovedForAll, or getApproved and ownerOf for single NFTs), since allowances shrink as they are spent. Approvals now zero or revoked are counted, not listed, unless include_inactive=true.
Each active approval has: kind (erc20_allowance, nft_operator_for_all, nft_single_token), token (address, symbol, decimals), spender (address, contract or plain account, and the explorer's name and verification for up to 25 spenders), the allowance raw and in whole tokens with "unlimited" (≥ 2^128), the owner's current balance of the token and the exposure (what the spender could take now: the smaller of allowance and balance), the approving transaction, risk flags and a severity, and the exact call to revoke it (pass it to build_unsigned_tx: approve(spender, 0), setApprovalForAll(operator, false) or approve(0x0…0, tokenId)).
Risk flags: unlimited, covers_balance, operator_for_all, spender_not_a_contract (approvals to plain accounts are a common phishing pattern unless it is the owner's own wallet), spender_unverified, flagged_scam. Severity is high when broad access (unlimited, covers_balance, operator_for_all) meets a doubtful spender (not a contract, unverified, flagged), medium for broad access alone, low otherwise. Token symbols and spender names are untrusted.`

type approvalsIn struct {
	Owner           string `json:"owner" jsonschema:"the address whose approvals to audit"`
	FromBlock       uint64 `json:"from_block,omitempty" jsonschema:"first block to scan (default 0, the whole chain)"`
	IncludeInactive bool   `json:"include_inactive,omitempty" jsonschema:"also list approvals that are now zero or revoked"`
}

type approvalKey struct {
	token util.Address
	kind  string
	id    string // spender, operator, or token ID
}

type approvalEvent struct {
	approvalKey
	spender util.Address
	amount  *big.Int // ERC-20 amount
	tokenID *big.Int
	allowed bool // ApprovalForAll flag
	block   uint64
	tx      util.Hash
}

type tokenMeta struct {
	Address  string          `json:"address"`
	Symbol   *untrusted.Text `json:"symbol,omitempty"`
	Decimals *int            `json:"decimals,omitempty"`
}

type spenderInfo struct {
	Address    string          `json:"address"`
	IsContract bool            `json:"is_contract"`
	Name       *untrusted.Text `json:"name,omitempty"`
	Verified   *bool           `json:"verified,omitempty"`
	Scam       bool            `json:"flagged_scam,omitempty"`
}

type approvalOut struct {
	Kind       string       `json:"kind"`
	Token      *tokenMeta   `json:"token"`
	Spender    *spenderInfo `json:"spender"`
	TokenID    string       `json:"token_id,omitempty"`
	Allowance  *amountOut   `json:"allowance,omitempty"`
	Unlimited  bool         `json:"unlimited,omitempty"`
	Balance    *amountOut   `json:"owner_balance,omitempty"`
	Exposure   *amountOut   `json:"exposure,omitempty"`
	Active     bool         `json:"active"`
	StateCheck string       `json:"state_check"`
	ApprovedAt struct {
		Block uint64 `json:"block"`
		Tx    string `json:"transaction"`
	} `json:"last_approval"`
	Risks    []string `json:"risks,omitempty"`
	Severity string   `json:"severity"`
	Revoke   *revoke  `json:"revoke,omitempty"`
}

type amountOut struct {
	Raw    string `json:"raw"`
	Tokens string `json:"tokens,omitempty"`
}

type revoke struct {
	To       string `json:"to"`
	Function string `json:"function"`
	Args     []any  `json:"args"`
}

func (t *tools) approvals(ctx context.Context, in approvalsIn) (any, error) {
	owner, err := evm.ParseAddress(in.Owner)
	if err != nil {
		return nil, server.Inputf("owner: %v", err)
	}
	var p server.Partial
	var headQ hexutil.Uint64
	if err := t.r.Node.Call(ctx, &headQ, "eth_blockNumber"); err != nil {
		return nil, err
	}
	head := uint64(headQ)
	if in.FromBlock > head {
		return nil, server.Inputf("from_block %d is after the head %d", in.FromBlock, head)
	}

	events, scanned, err := t.scanApprovals(ctx, owner, in.FromBlock, head)
	if err != nil {
		return nil, err
	}

	// The latest event per key wins; logs arrive in chain order.
	latest := map[approvalKey]*approvalEvent{}
	var order []approvalKey
	for i := range events {
		e := &events[i]
		if _, seen := latest[e.approvalKey]; !seen {
			order = append(order, e.approvalKey)
		}
		latest[e.approvalKey] = e
	}

	tokens := map[util.Address]bool{}
	spenders := map[util.Address]bool{}
	for _, k := range order {
		e := latest[k]
		tokens[e.token] = true
		if e.spender != (util.Address{}) {
			spenders[e.spender] = true
		}
	}

	state, err := t.approvalState(ctx, owner, order, latest, tokens)
	if err != nil {
		return nil, err
	}
	meta := t.tokenMetas(ctx, tokens)
	info := t.spenderInfos(ctx, spenders, &p)

	out := map[string]any{
		"owner":   owner.Hex(),
		"scanned": map[string]any{"from_block": in.FromBlock, "to_block": head, "approval_events": scanned},
	}
	var list []approvalOut
	inactive := 0
	counts := map[string]int{}
	for _, k := range order {
		e := latest[k]
		a := buildApproval(owner, e, state[k], meta[e.token], info[e.spender], state[approvalKey{token: e.token, kind: "balance"}])
		if !a.Active {
			inactive++
			if !in.IncludeInactive {
				continue
			}
		} else {
			counts[a.Severity]++
		}
		list = append(list, a)
	}
	rank := map[string]int{"high": 0, "medium": 1, "low": 2, "none": 3}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Active != list[j].Active {
			return list[i].Active
		}
		return rank[list[i].Severity] < rank[list[j].Severity]
	})
	if list == nil {
		list = []approvalOut{}
	}
	out["approvals"] = list
	out["active_by_severity"] = counts
	out["inactive"] = inactive
	if len(spenders) > approvalMaxSpenders {
		out["note"] = fmt.Sprintf("explorer names and verification were looked up for %d of %d spenders", approvalMaxSpenders, len(spenders))
	}
	if p.Errors != nil {
		out["errors"] = p.Errors
	}
	return out, nil
}

// scanApprovals collects the owner's Approval and ApprovalForAll events.
func (t *tools) scanApprovals(ctx context.Context, owner util.Address, from, to uint64) ([]approvalEvent, int, error) {
	chunk := t.r.Cfg.MaxLogRange
	if (to-from)/chunk+1 > approvalMaxChunks {
		return nil, 0, server.Inputf("scanning %d blocks needs more than %d log queries; pass a later from_block", to-from+1, approvalMaxChunks)
	}
	ownerTopic := util.BytesToHash(owner.Bytes()).Hex()
	topics := []any{[]string{topicApproval.Hex(), topicApprovalForAll.Hex()}, ownerTopic}

	var ranges [][2]uint64
	for start := from; start <= to; start += chunk {
		ranges = append(ranges, [2]uint64{start, min(start+chunk-1, to)})
	}
	var all []evm.RPCLog
	for i := 0; i < len(ranges); i += 10 {
		group := ranges[i:min(i+10, len(ranges))]
		results := make([][]evm.RPCLog, len(group))
		batch := make([]server.BatchElem, len(group))
		for j, rg := range group {
			batch[j] = server.BatchElem{Method: "eth_getLogs", Args: []any{map[string]any{
				"fromBlock": hexutil.EncodeUint64(rg[0]), "toBlock": hexutil.EncodeUint64(rg[1]), "topics": topics,
			}}, Result: &results[j]}
		}
		if err := t.r.Node.Batch(ctx, batch); err != nil {
			return nil, 0, err
		}
		for j, b := range batch {
			if b.Error != nil {
				return nil, 0, b.Error
			}
			all = append(all, results[j]...)
		}
		if len(all) > approvalMaxLogs {
			return nil, 0, server.Inputf("more than %d approval events; pass a later from_block", approvalMaxLogs)
		}
	}

	var events []approvalEvent
	for _, l := range all {
		if l.Removed || len(l.Topics) < 3 {
			continue
		}
		e := approvalEvent{block: uint64(l.BlockNumber), tx: l.TxHash}
		e.token = l.Address
		spender := util.BytesToAddress(l.Topics[2].Bytes())
		switch {
		case l.Topics[0] == topicApprovalForAll && len(l.Topics) == 3 && len(l.Data) >= 32:
			e.kind, e.id, e.spender = "nft_operator_for_all", spender.Hex(), spender
			e.allowed = new(big.Int).SetBytes(l.Data[:32]).Sign() != 0
		case l.Topics[0] == topicApproval && len(l.Topics) == 3 && len(l.Data) >= 32:
			e.kind, e.id, e.spender = "erc20_allowance", spender.Hex(), spender
			e.amount = new(big.Int).SetBytes(l.Data[:32])
		case l.Topics[0] == topicApproval && len(l.Topics) == 4:
			e.tokenID = l.Topics[3].Big()
			e.kind, e.id, e.spender = "nft_single_token", e.tokenID.String(), spender
		default:
			continue
		}
		events = append(events, e)
	}
	return events, len(all), nil
}

// callState is the current on-chain state behind an approval.
type callState struct {
	value   *big.Int     // allowance or balance
	flag    *bool        // isApprovedForAll
	address util.Address // getApproved
	owner   util.Address // ownerOf
	ok      bool
}

// approvalState reads each approval's current state, and the owner's
// balance of every token, with batched eth_calls.
func (t *tools) approvalState(ctx context.Context, owner util.Address, order []approvalKey, latest map[approvalKey]*approvalEvent, tokens map[util.Address]bool) (map[approvalKey]*callState, error) {
	type call struct {
		key    approvalKey
		method abi.Method
		to     util.Address
		data   []byte
		apply  func(*callState, []any)
	}
	var calls []call
	pack := func(m abi.Method, args ...any) []byte {
		b, _ := m.Inputs.Pack(args...)
		return append(append([]byte{}, m.ID...), b...)
	}
	for _, k := range order {
		e := latest[k]
		switch k.kind {
		case "erc20_allowance":
			calls = append(calls, call{key: k, method: mAllowance, to: k.token, data: pack(mAllowance, owner, e.spender),
				apply: func(s *callState, v []any) { s.value, s.ok = v[0].(*big.Int), true }})
		case "nft_operator_for_all":
			calls = append(calls, call{key: k, method: mIsApproved, to: k.token, data: pack(mIsApproved, owner, e.spender),
				apply: func(s *callState, v []any) { b := v[0].(bool); s.flag, s.ok = &b, true }})
		case "nft_single_token":
			calls = append(calls,
				call{key: k, method: mGetApproved, to: k.token, data: pack(mGetApproved, e.tokenID),
					apply: func(s *callState, v []any) { s.address, s.ok = v[0].(util.Address), true }},
				call{key: k, method: mOwnerOf, to: k.token, data: pack(mOwnerOf, e.tokenID),
					apply: func(s *callState, v []any) { s.owner = v[0].(util.Address) }})
		}
	}
	for tok := range tokens {
		calls = append(calls, call{key: approvalKey{token: tok, kind: "balance"}, method: mBalanceOf, to: tok, data: pack(mBalanceOf, owner),
			apply: func(s *callState, v []any) { s.value, s.ok = v[0].(*big.Int), true }})
	}

	state := map[approvalKey]*callState{}
	for i := 0; i < len(calls); i += callBatch {
		group := calls[i:min(i+callBatch, len(calls))]
		results := make([]hexutil.Bytes, len(group))
		batch := make([]server.BatchElem, len(group))
		for j, c := range group {
			batch[j] = server.BatchElem{Method: "eth_call", Args: []any{map[string]any{"to": c.to.Hex(), "data": hexutil.Encode(c.data)}, "latest"}, Result: &results[j]}
		}
		if err := t.r.Node.Batch(ctx, batch); err != nil {
			return nil, err
		}
		for j, c := range group {
			s := state[c.key]
			if s == nil {
				s = &callState{}
				state[c.key] = s
			}
			if batch[j].Error != nil || len(results[j]) == 0 {
				continue
			}
			if v, err := c.method.Outputs.Unpack(results[j]); err == nil && len(v) == 1 {
				c.apply(s, v)
			}
		}
	}
	return state, nil
}

// tokenMetas reads symbol and decimals of each token over RPC.
func (t *tools) tokenMetas(ctx context.Context, tokens map[util.Address]bool) map[util.Address]*tokenMeta {
	list := make([]util.Address, 0, len(tokens))
	for tok := range tokens {
		list = append(list, tok)
	}
	out := map[util.Address]*tokenMeta{}
	for i := 0; i < len(list); i += callBatch / 2 {
		group := list[i:min(i+callBatch/2, len(list))]
		syms := make([]hexutil.Bytes, len(group))
		decs := make([]hexutil.Bytes, len(group))
		var batch []server.BatchElem
		for j, tok := range group {
			batch = append(batch,
				server.BatchElem{Method: "eth_call", Args: []any{map[string]any{"to": tok.Hex(), "data": hexutil.Encode(mSymbol.ID)}, "latest"}, Result: &syms[j]},
				server.BatchElem{Method: "eth_call", Args: []any{map[string]any{"to": tok.Hex(), "data": hexutil.Encode(mDecimals.ID)}, "latest"}, Result: &decs[j]})
		}
		_ = t.r.Node.Batch(ctx, batch)
		for j, tok := range group {
			m := &tokenMeta{Address: tok.Hex()}
			if batch[2*j].Error == nil {
				if s := decodeSymbol(syms[j]); s != "" {
					m.Symbol = untrusted.Ptr(s)
				}
			}
			if batch[2*j+1].Error == nil && len(decs[j]) == 32 {
				if d := new(big.Int).SetBytes(decs[j]); d.IsInt64() && d.Int64() <= 77 {
					n := int(d.Int64())
					m.Decimals = &n
				}
			}
			out[tok] = m
		}
	}
	return out
}

// decodeSymbol reads a string return value, or a bytes32 one as some old
// tokens return.
func decodeSymbol(b []byte) string {
	if v, err := mSymbol.Outputs.Unpack(b); err == nil && len(v) == 1 {
		if s, ok := v[0].(string); ok {
			return s
		}
	}
	if len(b) == 32 {
		return string(bytes.TrimRight(b, "\x00"))
	}
	return ""
}

// spenderInfos learns whether each spender is a contract (RPC) and, for up
// to approvalMaxSpenders, its explorer name, verification and scam flag.
func (t *tools) spenderInfos(ctx context.Context, spenders map[util.Address]bool, p *server.Partial) map[util.Address]*spenderInfo {
	list := make([]util.Address, 0, len(spenders))
	for s := range spenders {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Hex() < list[j].Hex() })
	out := map[util.Address]*spenderInfo{}
	codes := make([]hexutil.Bytes, len(list))
	batch := make([]server.BatchElem, len(list))
	for i, s := range list {
		out[s] = &spenderInfo{Address: s.Hex()}
		batch[i] = server.BatchElem{Method: "eth_getCode", Args: []any{s.Hex(), "latest"}, Result: &codes[i]}
	}
	if err := t.r.Node.Batch(ctx, batch); err == nil {
		for i, s := range list {
			out[s].IsContract = batch[i].Error == nil && len(codes[i]) > 0
		}
	}
	if t.r.Explorer == nil {
		return out
	}
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		failed error
		sem    = make(chan struct{}, 4)
	)
	for _, s := range list[:min(len(list), approvalMaxSpenders)] {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			a, err := t.r.Explorer.Address(ctx, s.Hex())
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if !strings.Contains(err.Error(), "404") {
					failed = err
				}
				return
			}
			si := out[s]
			si.Name = untrusted.Ptr(a.Name)
			if si.IsContract {
				v := a.IsVerified
				si.Verified = &v
			}
			si.Scam = a.IsScam
		}()
	}
	wg.Wait()
	p.Fail("explorer", failed)
	return out
}

func buildApproval(owner util.Address, e *approvalEvent, s *callState, meta *tokenMeta, spender *spenderInfo, bal *callState) approvalOut {
	a := approvalOut{Kind: e.kind, Token: meta, Spender: spender, StateCheck: "current state read from the contract"}
	if spender == nil {
		a.Spender = &spenderInfo{Address: e.spender.Hex()}
	}
	a.ApprovedAt.Block, a.ApprovedAt.Tx = e.block, e.tx.Hex()
	var dec *int
	if meta != nil {
		dec = meta.Decimals
	}
	fmtAmount := func(v *big.Int) *amountOut {
		o := &amountOut{Raw: v.String()}
		if dec != nil {
			o.Tokens = lax.FormatUnits(v, *dec)
		}
		return o
	}
	verified := s != nil && s.ok
	if !verified {
		a.StateCheck = "the contract did not answer the standard call; showing the last event"
	}

	switch e.kind {
	case "erc20_allowance":
		amount := e.amount
		if verified {
			amount = s.value
		}
		a.Allowance = fmtAmount(amount)
		a.Active = amount.Sign() > 0
		a.Unlimited = amount.Cmp(unlimitedFloor) >= 0
		if a.Unlimited {
			a.Allowance.Tokens = "unlimited"
		}
		if bal != nil && bal.ok {
			a.Balance = fmtAmount(bal.value)
			a.Exposure = fmtAmount(minBig(amount, bal.value))
		}
		a.Revoke = &revoke{To: e.token.Hex(), Function: "approve(address,uint256)", Args: []any{e.spender.Hex(), "0"}}
	case "nft_operator_for_all":
		a.Active = e.allowed
		if verified {
			a.Active = *s.flag
		}
		if bal != nil && bal.ok {
			a.Balance = &amountOut{Raw: bal.value.String()}
		}
		a.Revoke = &revoke{To: e.token.Hex(), Function: "setApprovalForAll(address,bool)", Args: []any{e.spender.Hex(), false}}
	case "nft_single_token":
		a.TokenID = e.tokenID.String()
		a.Active = e.spender != (util.Address{})
		if verified {
			// An approval on a token the owner no longer holds is moot.
			a.Active = s.address == e.spender && s.address != (util.Address{}) && s.owner == owner
		}
		a.Revoke = &revoke{To: e.token.Hex(), Function: "approve(address,uint256)", Args: []any{"0x0000000000000000000000000000000000000000", e.tokenID.String()}}
	}
	if !a.Active {
		a.Severity, a.Revoke = "none", nil
		return a
	}

	broad := false
	if a.Unlimited {
		a.Risks, broad = append(a.Risks, "unlimited"), true
	}
	if e.kind == "erc20_allowance" && !a.Unlimited && a.Balance != nil && a.Exposure != nil && a.Balance.Raw != "0" && a.Exposure.Raw == a.Balance.Raw {
		a.Risks, broad = append(a.Risks, "covers_balance"), true
	}
	if e.kind == "nft_operator_for_all" {
		a.Risks, broad = append(a.Risks, "operator_for_all"), true
	}
	doubtful := false
	sp := a.Spender
	if !sp.IsContract {
		a.Risks, doubtful = append(a.Risks, "spender_not_a_contract"), true
	} else if sp.Verified != nil && !*sp.Verified {
		a.Risks, doubtful = append(a.Risks, "spender_unverified"), true
	}
	if sp.Scam {
		a.Risks, doubtful = append(a.Risks, "flagged_scam"), true
	}
	switch {
	case broad && doubtful:
		a.Severity = "high"
	case broad || doubtful:
		a.Severity = "medium"
	default:
		a.Severity = "low"
	}
	return a
}

func minBig(a, b *big.Int) *big.Int {
	if a.Cmp(b) < 0 {
		return a
	}
	return b
}
