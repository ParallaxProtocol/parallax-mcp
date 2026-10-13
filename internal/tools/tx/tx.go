// Package tx is the transaction tool group, disabled by default: it builds
// unsigned transactions for an external signer, decodes raw transactions,
// and, only with --allow-broadcast, submits signed ones.
//
// The server never holds, accepts or asks for a private key. Signing happens
// outside it, in a wallet, hardware device or the MPC signer.
package tx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"

	"github.com/ParallaxProtocol/parallax/v2/crypto"
	"github.com/ParallaxProtocol/parallax/v2/primitives/rlp"
	"github.com/ParallaxProtocol/parallax/v2/primitives/types"
	"github.com/ParallaxProtocol/parallax/v2/script/abi"
	"github.com/ParallaxProtocol/parallax/v2/util"
	"github.com/ParallaxProtocol/parallax/v2/util/hexutil"

	"github.com/ParallaxProtocol/parallax-mcp/internal/config"
	"github.com/ParallaxProtocol/parallax-mcp/internal/evm"
	"github.com/ParallaxProtocol/parallax-mcp/internal/lax"
	"github.com/ParallaxProtocol/parallax-mcp/internal/server"
)

const group = config.GroupTx

const keyWarning = "This input is 32 bytes, the size of a private key. This server never accepts private keys: do not share it with anyone or any tool, and if it is a key that was pasted somewhere, move the funds it controls to a new key."

// Register adds the tx tools. broadcast_signed_tx is skipped by the
// registrar unless broadcasting was explicitly allowed.
func Register(r *server.Registrar) {
	t := &tools{r: r}
	server.Add(r, server.Spec{Group: group, Name: "build_unsigned_tx", Title: "Build unsigned transaction",
		Description: `Builds an unsigned Parallax transaction for an external signer (wallet, hardware device, MPC signer). This server never signs and never accepts private keys.
Fills in what is not given: nonce (the sender's pending nonce), gas limit (eth_estimateGas; exactly 21,000 for a plain transfer, otherwise the estimate plus 10% headroom, since state can change before inclusion), gas price (Parallax's fee estimator for fee_target_blocks, default 6 ≈ 1 hour, falling back to eth_gasPrice) and chain ID 2110.
Value is given in LAX (value_lax, a decimal string such as "1.5") or wei (value_wei). Calldata is raw "data" or "function" + "args" (+ "abi"), as in call_contract. "type" is "legacy" (default; EIP-155 replay protected) or "access_list" (EIP-2930). Parallax has no EIP-1559, so there is no type 2.
Returns the transaction fields, "unsigned_tx" (the RLP-encoded transaction with an empty signature, as wallets import it), "signing_payload" (the RLP preimage the signature covers) and "signing_hash" (its keccak256, what the key signs), the maximum cost (value + gas × gas price) in wei and LAX, and the sender's balance with a warning when it does not cover the cost. Nothing is sent.`,
	}, t.build)

	server.Add(r, server.Spec{Group: group, Name: "decode_tx", Title: "Decode raw transaction",
		Description: `Decodes a raw transaction given as 0x-hex: a signed transaction (as broadcast), an unsigned one, or an EIP-155 signing payload. Returns its type, chain ID (with a warning if it is not 2110), nonce, gas limit, gas price (wei and gwei), recipient, value (wei and LAX), calldata with its decoding (verified ABI from the explorer or the standard token ABI), maximum cost, whether it is signed and replay protected, and for signed transactions the sender recovered from the signature and the transaction hash. Decoded strings are untrusted. Use it to check what a transaction does before signing or broadcasting it.`,
	}, t.decode)

	server.Add(r, server.Spec{Group: group, Name: "broadcast_signed_tx", Title: "Broadcast signed transaction", Writes: true,
		Description: `Submits a signed raw transaction (0x-hex, as decode_tx accepts) to the network with eth_sendRawTransaction. THIS SPENDS FUNDS AND CANNOT BE UNDONE: confirm with the user, after showing them the decode_tx output, before calling it.
Checks first and refuses: unsigned transactions, transactions for another chain ID, and transactions without EIP-155 replay protection. Reports the sender, the hash, and warnings when the nonce is not the sender's next one or the balance does not cover the maximum cost. Returns the transaction hash; use get_transaction and get_receipt to follow it. Only available when the server was started with --allow-broadcast.`,
	}, t.broadcast)
}

type tools struct{ r *server.Registrar }

func (t *tools) chainID() *big.Int { return new(big.Int).SetUint64(t.r.Cfg.ChainID) }

// ---- build_unsigned_tx ----

type buildIn struct {
	From            string `json:"from" jsonschema:"sender address (needed for nonce, gas estimate and balance check)"`
	To              string `json:"to,omitempty" jsonschema:"recipient address; omit to deploy a contract with data"`
	ValueLAX        string `json:"value_lax,omitempty" jsonschema:"amount in LAX as a decimal string, such as \"1.5\""`
	ValueWei        string `json:"value_wei,omitempty" jsonschema:"amount in wei, instead of value_lax"`
	Data            string `json:"data,omitempty" jsonschema:"raw 0x-hex calldata"`
	Function        string `json:"function,omitempty" jsonschema:"function name or full signature, instead of data"`
	Args            []any  `json:"args,omitempty" jsonschema:"function arguments; integers as decimal strings"`
	ABI             string `json:"abi,omitempty" jsonschema:"contract ABI JSON; default: the explorer's verified ABI"`
	Gas             uint64 `json:"gas,omitempty" jsonschema:"gas limit; estimated when omitted"`
	GasPriceWei     string `json:"gas_price_wei,omitempty" jsonschema:"gas price in wei; estimated when omitted"`
	FeeTargetBlocks int    `json:"fee_target_blocks,omitempty" jsonschema:"confirmation target for the fee estimate, in blocks (default 6)"`
	Nonce           *int64 `json:"nonce,omitempty" jsonschema:"nonce; the sender's pending nonce when omitted"`
	Type            string `json:"type,omitempty" jsonschema:"\"legacy\" (default) or \"access_list\""`
}

type buildOut struct {
	server.Partial
	Type           string         `json:"type"`
	ChainID        uint64         `json:"chain_id"`
	From           string         `json:"from"`
	To             string         `json:"to,omitempty"`
	Nonce          uint64         `json:"nonce"`
	Gas            uint64         `json:"gas_limit"`
	GasEstimate    uint64         `json:"gas_estimate,omitempty"`
	GasPriceWei    string         `json:"gas_price_wei"`
	GasPriceGwei   string         `json:"gas_price_gwei"`
	GasPriceSource string         `json:"gas_price_source"`
	Value          lax.Amount     `json:"value"`
	Data           string         `json:"data"`
	Function       string         `json:"function,omitempty"`
	MaxFee         lax.Amount     `json:"max_fee"`
	MaxCost        lax.Amount     `json:"max_cost"`
	Balance        *lax.Amount    `json:"sender_balance,omitempty"`
	Warnings       []string       `json:"warnings,omitempty"`
	UnsignedTx     string         `json:"unsigned_tx"`
	SigningPayload string         `json:"signing_payload"`
	SigningHash    string         `json:"signing_hash"`
	JSON           map[string]any `json:"tx_json"`
	Note           string         `json:"note"`
}

func (t *tools) build(ctx context.Context, in buildIn) (any, error) {
	out := buildOut{ChainID: t.r.Cfg.ChainID, Type: "legacy"}
	from, err := evm.ParseAddress(in.From)
	if err != nil {
		return nil, server.Inputf("from: %v", err)
	}
	out.From = from.Hex()
	var to *util.Address
	if in.To != "" {
		a, err := evm.ParseAddress(in.To)
		if err != nil {
			return nil, server.Inputf("to: %v", err)
		}
		to = &a
		out.To = a.Hex()
	}
	switch in.Type {
	case "", "legacy":
	case "access_list":
		out.Type = "access_list"
	default:
		return nil, server.Inputf(`type must be "legacy" or "access_list" (Parallax has no EIP-1559 transactions)`)
	}

	value := new(big.Int)
	switch {
	case in.ValueLAX != "" && in.ValueWei != "":
		return nil, server.Inputf("give value_lax or value_wei, not both")
	case in.ValueLAX != "":
		if value, err = lax.ParseLAX(in.ValueLAX); err != nil {
			return nil, server.Inputf("value_lax: %v", err)
		}
	case in.ValueWei != "":
		if value, err = evm.ParseBig(in.ValueWei); err != nil || value.Sign() < 0 {
			return nil, server.Inputf("value_wei must be a non-negative integer")
		}
	}
	out.Value = lax.NewAmount(value)

	data, fn, err := t.calldata(ctx, to, in, &out.Partial)
	if err != nil {
		return nil, err
	}
	out.Function = fn
	if to == nil && len(data) == 0 {
		return nil, server.Inputf("give to (a recipient) or data (contract creation code)")
	}
	out.Data = hexutil.Encode(data)

	// Nonce, gas price and balance in one round trip.
	var (
		pending  hexutil.Uint64
		price    hexutil.Big
		balance  hexutil.Big
		smartFee struct {
			GasPrice       hexutil.Big `json:"gasPrice"`
			LegacyFallback bool        `json:"legacyFallback"`
		}
		chainID hexutil.Big
	)
	target := in.FeeTargetBlocks
	if target == 0 {
		target = 6
	}
	if target < 1 || target > 1008 {
		return nil, server.Inputf("fee_target_blocks must be between 1 and 1008")
	}
	batch := []server.BatchElem{
		{Method: "eth_getTransactionCount", Args: []any{from.Hex(), "pending"}, Result: &pending},
		{Method: "eth_gasPrice", Result: &price},
		{Method: "eth_getBalance", Args: []any{from.Hex(), "latest"}, Result: &balance},
		{Method: "eth_estimateSmartFee", Args: []any{target}, Result: &smartFee},
		{Method: "eth_chainId", Result: &chainID},
	}
	if err := t.r.Node.Batch(ctx, batch); err != nil {
		return nil, err
	}
	if batch[4].Error == nil && (*big.Int)(&chainID).Cmp(t.chainID()) != 0 {
		return nil, fmt.Errorf("the node is on chain %s, not %d; refusing to build a transaction for it", (*big.Int)(&chainID), t.r.Cfg.ChainID)
	}

	if in.Nonce != nil {
		if *in.Nonce < 0 {
			return nil, server.Inputf("nonce must not be negative")
		}
		out.Nonce = uint64(*in.Nonce)
		if batch[0].Error == nil && out.Nonce != uint64(pending) {
			out.Warnings = append(out.Warnings, fmt.Sprintf("nonce %d is not the sender's next nonce %d: the transaction will wait for the gap to fill, or replace a pending one", out.Nonce, uint64(pending)))
		}
	} else {
		if batch[0].Error != nil {
			return nil, batch[0].Error
		}
		out.Nonce = uint64(pending)
	}

	var gasPrice *big.Int
	switch {
	case in.GasPriceWei != "":
		if gasPrice, err = evm.ParseBig(in.GasPriceWei); err != nil || gasPrice.Sign() <= 0 {
			return nil, server.Inputf("gas_price_wei must be a positive integer")
		}
		out.GasPriceSource = "given"
	case batch[3].Error == nil && !smartFee.LegacyFallback:
		gasPrice = (*big.Int)(&smartFee.GasPrice)
		out.GasPriceSource = fmt.Sprintf("fee estimator, %d block target", target)
	case batch[1].Error == nil:
		gasPrice = (*big.Int)(&price)
		out.GasPriceSource = "eth_gasPrice (the fee estimator had too little data)"
	default:
		return nil, batch[1].Error
	}
	out.GasPriceWei, out.GasPriceGwei = gasPrice.String(), lax.FormatUnits(gasPrice, 9)

	out.Gas = in.Gas
	if out.Gas == 0 {
		msg := map[string]any{"from": from.Hex(), "value": hexutil.EncodeBig(value), "data": hexutil.Encode(data)}
		if to != nil {
			msg["to"] = to.Hex()
		}
		var est hexutil.Uint64
		if err := t.r.Node.Call(ctx, &est, "eth_estimateGas", msg); err != nil {
			return nil, fmt.Errorf("estimating gas: %w (the transaction would likely fail; check the arguments and the sender's balance)", err)
		}
		out.GasEstimate = uint64(est)
		out.Gas = uint64(est)
		if !(len(data) == 0 && uint64(est) == 21000) {
			out.Gas = uint64(est) + (uint64(est)+9)/10 // +10%, rounded up
		}
	}

	maxFee := new(big.Int).Mul(gasPrice, new(big.Int).SetUint64(out.Gas))
	maxCost := new(big.Int).Add(maxFee, value)
	out.MaxFee, out.MaxCost = lax.NewAmount(maxFee), lax.NewAmount(maxCost)
	if batch[2].Error == nil {
		b := lax.NewAmount((*big.Int)(&balance))
		out.Balance = &b
		if (*big.Int)(&balance).Cmp(maxCost) < 0 {
			out.Warnings = append(out.Warnings, "the sender's balance does not cover the maximum cost; the node will reject the transaction")
		}
	} else {
		out.Fail("eth_getBalance", batch[2].Error)
	}

	var tx *types.Transaction
	var signer types.Signer
	zero := new(big.Int)
	if out.Type == "access_list" {
		tx = types.NewTx(&types.AccessListTx{ChainID: t.chainID(), Nonce: out.Nonce, GasPrice: gasPrice, Gas: out.Gas, To: to, Value: value, Data: data, V: zero, R: zero, S: zero})
		signer = types.NewEIP2930Signer(t.chainID())
	} else {
		tx = types.NewTx(&types.LegacyTx{Nonce: out.Nonce, GasPrice: gasPrice, Gas: out.Gas, To: to, Value: value, Data: data, V: zero, R: zero, S: zero})
		signer = types.NewEIP155Signer(t.chainID())
	}
	unsigned, err := tx.MarshalBinary()
	if err != nil {
		return nil, err
	}
	payload, err := signingPayload(tx, t.chainID())
	if err != nil {
		return nil, err
	}
	hash := signer.Hash(tx)
	if !strings.EqualFold(util.BytesToHash(crypto.Keccak256(payload)).Hex(), hash.Hex()) {
		return nil, errors.New("internal error: signing payload does not match the signer's hash")
	}
	out.UnsignedTx, out.SigningPayload, out.SigningHash = hexutil.Encode(unsigned), hexutil.Encode(payload), hash.Hex()
	out.JSON = txJSON(tx, t.r.Cfg.ChainID)
	out.JSON["from"] = from.Hex()
	out.Note = "Sign signing_hash (or let the wallet sign unsigned_tx / tx_json) with the sender's key outside this server, then check the result with decode_tx before broadcasting it."
	return out, nil
}

// calldata resolves data or function+args.
func (t *tools) calldata(ctx context.Context, to *util.Address, in buildIn, p *server.Partial) ([]byte, string, error) {
	switch {
	case in.Data != "" && in.Function != "":
		return nil, "", server.Inputf("give data or function, not both")
	case in.Data != "":
		b, err := evm.ParseHex(in.Data)
		if err != nil {
			return nil, "", server.Inputf("data: %v", err)
		}
		return b, "", nil
	case in.Function == "":
		return nil, "", nil
	}
	var m *abi.Method
	if strings.Contains(in.Function, "(") {
		parsed, err := evm.ParseSignature(in.Function)
		if err != nil {
			return nil, "", server.Inputf("%v", err)
		}
		m = &parsed
	} else {
		if to == nil {
			return nil, "", server.Inputf("function needs to (a contract address)")
		}
		var a *abi.ABI
		var err error
		if in.ABI != "" {
			a, err = evm.ParseABI([]byte(in.ABI))
			if err != nil {
				return nil, "", server.Inputf("%v", err)
			}
		} else if a, err = evm.VerifiedABI(ctx, t.r.Explorer, *to); err != nil {
			return nil, "", fmt.Errorf("fetching the verified ABI: %w; pass a full signature instead", err)
		} else if a == nil {
			return nil, "", server.Inputf("%s has no verified ABI; pass abi or a full signature", to.Hex())
		}
		var matches []abi.Method
		for _, cand := range a.Methods {
			if cand.RawName == in.Function {
				matches = append(matches, cand)
			}
		}
		if len(matches) != 1 {
			return nil, "", server.Inputf("function %q matches %d functions in the ABI; pass the full signature", in.Function, len(matches))
		}
		m = &matches[0]
	}
	raw := make([]json.RawMessage, len(in.Args))
	for i, a := range in.Args {
		if f, ok := a.(float64); ok && (math.Abs(f) > 1<<53 || f != math.Trunc(f)) {
			return nil, "", server.Inputf("argument %d: pass large integers as decimal strings", i)
		}
		raw[i], _ = json.Marshal(a)
	}
	args, err := evm.EncodeArgs(m.Inputs, raw)
	if err != nil {
		return nil, "", server.Inputf("%s: %v", m.Sig, err)
	}
	return append(append([]byte{}, m.ID...), args...), m.Sig, nil
}

// signingPayload is the byte string whose keccak256 the sender signs: for a
// legacy transaction the EIP-155 list [nonce, gasPrice, gas, to, value,
// data, chainId, 0, 0]; for an access list transaction 0x01 followed by
// [chainId, nonce, gasPrice, gas, to, value, data, accessList].
func signingPayload(tx *types.Transaction, chainID *big.Int) ([]byte, error) {
	if tx.Type() == types.AccessListTxType {
		body, err := rlp.EncodeToBytes([]any{chainID, tx.Nonce(), tx.GasPrice(), tx.Gas(), tx.To(), tx.Value(), tx.Data(), tx.AccessList()})
		if err != nil {
			return nil, err
		}
		return append([]byte{types.AccessListTxType}, body...), nil
	}
	return rlp.EncodeToBytes([]any{tx.Nonce(), tx.GasPrice(), tx.Gas(), tx.To(), tx.Value(), tx.Data(), chainID, uint(0), uint(0)})
}

func txJSON(tx *types.Transaction, chainID uint64) map[string]any {
	m := map[string]any{
		"type":     hexutil.EncodeUint64(uint64(tx.Type())),
		"chainId":  hexutil.EncodeUint64(chainID),
		"nonce":    hexutil.EncodeUint64(tx.Nonce()),
		"gas":      hexutil.EncodeUint64(tx.Gas()),
		"gasPrice": hexutil.EncodeBig(tx.GasPrice()),
		"value":    hexutil.EncodeBig(tx.Value()),
		"input":    hexutil.Encode(tx.Data()),
	}
	if tx.To() != nil {
		m["to"] = tx.To().Hex()
	}
	if tx.Type() == types.AccessListTxType {
		m["accessList"] = tx.AccessList()
	}
	return m
}

// ---- decode_tx ----

type rawIn struct {
	Raw string `json:"raw" jsonschema:"raw transaction as 0x-hex"`
}

type decodedOut struct {
	server.Partial
	Format       string     `json:"format"`
	Type         string     `json:"type"`
	ChainID      *uint64    `json:"chain_id,omitempty"`
	Protected    *bool      `json:"replay_protected,omitempty"`
	Signed       bool       `json:"signed"`
	From         string     `json:"from,omitempty"`
	Hash         string     `json:"hash,omitempty"`
	Nonce        uint64     `json:"nonce"`
	Gas          uint64     `json:"gas_limit"`
	GasPriceWei  string     `json:"gas_price_wei"`
	GasPriceGwei string     `json:"gas_price_gwei"`
	To           string     `json:"to,omitempty"`
	Creates      bool       `json:"creates_contract,omitempty"`
	Value        lax.Amount `json:"value"`
	Data         string     `json:"data"`
	DataCut      bool       `json:"data_truncated,omitempty"`
	DataBytes    int        `json:"data_bytes"`
	Decoded      *evm.Call  `json:"decoded_input,omitempty"`
	MaxCost      lax.Amount `json:"max_cost"`
	Warnings     []string   `json:"warnings,omitempty"`
}

// parseRaw decodes a typed or legacy transaction, or an EIP-155 signing
// payload, refusing 32-byte inputs that look like private keys.
func (t *tools) parseRaw(s string) (*types.Transaction, string, error) {
	b, err := evm.ParseHex(strings.TrimSpace(s))
	if err != nil {
		return nil, "", server.Inputf("raw: %v", err)
	}
	if len(b) == 32 {
		return nil, "", server.Inputf("%s", keyWarning)
	}
	if len(b) == 0 || len(b) > 128<<10 {
		return nil, "", server.Inputf("raw must be a non-empty transaction of at most 128 KiB")
	}
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(b); err == nil {
		// An EIP-155 signing payload [nonce, gasPrice, gas, to, value, data,
		// chainId, 0, 0] parses as a legacy transaction whose V is the chain
		// ID and whose R and S are zero.
		v, r, s := tx.RawSignatureValues()
		if tx.Type() == types.LegacyTxType && r.Sign() == 0 && s.Sign() == 0 && v.Sign() != 0 {
			return payloadTx(tx, v), "eip155_signing_payload", nil
		}
		return tx, "transaction", nil
	}
	return nil, "", server.Inputf("raw is not a transaction or EIP-155 signing payload this server can decode")
}

// payloadTx represents a signing payload as an unsigned transaction whose
// V carries the chain ID the way an EIP-155 signature would (2·id + 35).
func payloadTx(tx *types.Transaction, chainID *big.Int) *types.Transaction {
	zero := new(big.Int)
	v := new(big.Int).Add(new(big.Int).Mul(chainID, big.NewInt(2)), big.NewInt(35))
	return types.NewTx(&types.LegacyTx{Nonce: tx.Nonce(), GasPrice: tx.GasPrice(), Gas: tx.Gas(), To: tx.To(), Value: tx.Value(), Data: tx.Data(), V: v, R: zero, S: zero})
}

func (t *tools) describe(ctx context.Context, tx *types.Transaction, format string) decodedOut {
	out := decodedOut{Format: format, Nonce: tx.Nonce(), Gas: tx.Gas(), GasPriceWei: tx.GasPrice().String(),
		GasPriceGwei: lax.FormatUnits(tx.GasPrice(), 9), Value: lax.NewAmount(tx.Value()), DataBytes: len(tx.Data()),
		MaxCost: lax.NewAmount(tx.Cost())}
	switch tx.Type() {
	case types.LegacyTxType:
		out.Type = "legacy"
	case types.AccessListTxType:
		out.Type = "access_list"
	default:
		out.Type = fmt.Sprintf("type %d", tx.Type())
		out.Warnings = append(out.Warnings, "Parallax has no EIP-1559 (type 2) transactions; nodes will reject this one")
	}
	if v, _, _ := tx.RawSignatureValues(); tx.Type() == types.LegacyTxType && (v == nil || v.Sign() == 0) {
		// An unsigned legacy transaction has no chain ID until EIP-155
		// signing puts it into V.
		out.Warnings = append(out.Warnings, "unsigned legacy transaction: its chain ID is set when it is signed; sign it for chain 2110 (EIP-155)")
	} else if id := tx.ChainId(); id != nil && id.Sign() > 0 && id.IsUint64() {
		v := id.Uint64()
		out.ChainID = &v
		if v != t.r.Cfg.ChainID {
			out.Warnings = append(out.Warnings, fmt.Sprintf("chain ID %d is not Parallax's %d", v, t.r.Cfg.ChainID))
		}
	}
	_, r, s := tx.RawSignatureValues()
	out.Signed = r != nil && s != nil && r.Sign() != 0 && s.Sign() != 0
	if out.Signed {
		protected := tx.Protected()
		out.Protected = &protected
		if !protected {
			out.Warnings = append(out.Warnings, "no EIP-155 replay protection: this signed transaction is valid on every chain where the sender has the same nonce")
		}
		out.Hash = tx.Hash().Hex()
		if from, err := types.Sender(types.LatestSignerForChainID(tx.ChainId()), tx); err == nil {
			out.From = from.Hex()
		} else {
			out.Warnings = append(out.Warnings, "the signature does not recover a sender: "+err.Error())
		}
	}
	out.Data, out.DataCut = evm.HexPreview(tx.Data(), 2048)
	if tx.To() != nil {
		out.To = tx.To().Hex()
		if len(tx.Data()) >= 4 {
			dec, err := evm.DecodeCalldata(ctx, t.r.Explorer, *tx.To(), tx.Data())
			out.Decoded = dec
			out.Fail("explorer", err)
		}
	} else {
		out.Creates = true
	}
	return out
}

func (t *tools) decode(ctx context.Context, in rawIn) (any, error) {
	tx, format, err := t.parseRaw(in.Raw)
	if err != nil {
		return nil, err
	}
	return t.describe(ctx, tx, format), nil
}

// ---- broadcast_signed_tx ----

func (t *tools) broadcast(ctx context.Context, in rawIn) (any, error) {
	tx, format, err := t.parseRaw(in.Raw)
	if err != nil {
		return nil, err
	}
	d := t.describe(ctx, tx, format)
	switch {
	case format != "transaction" || !d.Signed:
		return nil, server.Inputf("the transaction is not signed; sign it outside this server first")
	case d.Protected == nil || !*d.Protected:
		return nil, server.Inputf("refusing a transaction without EIP-155 replay protection")
	case d.ChainID == nil || *d.ChainID != t.r.Cfg.ChainID:
		return nil, server.Inputf("refusing a transaction for chain %v; Parallax is %d", d.ChainID, t.r.Cfg.ChainID)
	case d.From == "":
		return nil, server.Inputf("the signature does not recover a sender")
	}

	var pending hexutil.Uint64
	var balance hexutil.Big
	batch := []server.BatchElem{
		{Method: "eth_getTransactionCount", Args: []any{d.From, "pending"}, Result: &pending},
		{Method: "eth_getBalance", Args: []any{d.From, "latest"}, Result: &balance},
	}
	if err := t.r.Node.Batch(ctx, batch); err == nil {
		if batch[0].Error == nil && tx.Nonce() != uint64(pending) {
			d.Warnings = append(d.Warnings, fmt.Sprintf("nonce %d is not the sender's next nonce %d", tx.Nonce(), uint64(pending)))
		}
		if batch[1].Error == nil && (*big.Int)(&balance).Cmp(tx.Cost()) < 0 {
			d.Warnings = append(d.Warnings, "the sender's balance does not cover the maximum cost")
		}
	}

	var hash util.Hash
	if err := t.r.Node.Call(ctx, &hash, "eth_sendRawTransaction", strings.TrimSpace(in.Raw)); err != nil {
		return nil, fmt.Errorf("the node rejected the transaction: %w", err)
	}
	t.r.Log.Warn("broadcast transaction", "hash", hash.Hex(), "from", d.From, "to", d.To, "value_wei", d.Value.Wei)
	return map[string]any{
		"broadcast": true,
		"hash":      hash.Hex(),
		"from":      d.From,
		"to":        d.To,
		"value":     d.Value,
		"nonce":     d.Nonce,
		"warnings":  d.Warnings,
		"next":      "follow it with get_transaction (pending until mined) and get_receipt; blocks come every 10 minutes on average",
	}, nil
}
