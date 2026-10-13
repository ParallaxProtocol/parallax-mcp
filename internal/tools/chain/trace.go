package chain

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ParallaxProtocol/parallax/v2/crypto"
	"github.com/ParallaxProtocol/parallax/v2/rpc"
	"github.com/ParallaxProtocol/parallax/v2/script/abi"
	"github.com/ParallaxProtocol/parallax/v2/util"
	"github.com/ParallaxProtocol/parallax/v2/util/hexutil"

	"github.com/ParallaxProtocol/parallax-mcp/internal/evm"
	"github.com/ParallaxProtocol/parallax-mcp/internal/lax"
	"github.com/ParallaxProtocol/parallax-mcp/internal/server"
	"github.com/ParallaxProtocol/parallax-mcp/internal/untrusted"
)

type traceTxIn struct {
	Hash   string `json:"hash" jsonschema:"transaction hash"`
	Tracer string `json:"tracer,omitempty" jsonschema:"\"call\" (default), \"prestate\" or \"4byte\""`
}

// rawCall is the callTracer's frame.
type rawCall struct {
	Type    string         `json:"type"`
	From    util.Address   `json:"from"`
	To      *util.Address  `json:"to"`
	Value   *hexutil.Big   `json:"value"`
	Gas     hexutil.Uint64 `json:"gas"`
	GasUsed hexutil.Uint64 `json:"gasUsed"`
	Input   hexutil.Bytes  `json:"input"`
	Output  hexutil.Bytes  `json:"output"`
	Error   string         `json:"error"`
	Revert  string         `json:"revertReason"`
	Calls   []rawCall      `json:"calls"`
}

// callNode is a call frame in tool output.
type callNode struct {
	Depth    int             `json:"depth"`
	Type     string          `json:"type"`
	From     string          `json:"from"`
	To       string          `json:"to,omitempty"`
	Value    *lax.Amount     `json:"value,omitempty"`
	Gas      uint64          `json:"gas"`
	GasUsed  uint64          `json:"gas_used"`
	Selector string          `json:"selector,omitempty"`
	InputLen int             `json:"input_bytes"`
	Decoded  *evm.Call       `json:"decoded_input,omitempty"`
	Output   string          `json:"output,omitempty"`
	Error    *untrusted.Text `json:"error,omitempty"`
	Revert   *untrusted.Text `json:"revert_reason,omitempty"`
}

type traceOut struct {
	server.Partial
	Tracer    string     `json:"tracer"`
	Calls     []callNode `json:"calls,omitempty"`
	CallCount int        `json:"call_count,omitempty"`
	Truncated bool       `json:"truncated,omitempty"`
	Result    any        `json:"result,omitempty"`
}

var tracerNames = map[string]string{
	"":         "callTracer",
	"call":     "callTracer",
	"prestate": "prestateTracer",
	"4byte":    "4byteTracer",
}

func (t *tools) traceTransaction(ctx context.Context, in traceTxIn) (any, error) {
	h, err := evm.ParseHash(in.Hash)
	if err != nil {
		return nil, server.Inputf("%v", err)
	}
	tracer, ok := tracerNames[in.Tracer]
	if !ok {
		return nil, server.Inputf(`tracer must be "call", "prestate" or "4byte"`)
	}
	cfg := map[string]any{"tracer": tracer, "timeout": "20s"}
	raw, err := t.r.Node.Raw(ctx, "debug_traceTransaction", h.Hex(), cfg)
	if err != nil {
		return nil, stateErr(err)
	}
	return t.formatTrace(ctx, tracer, raw)
}

func (t *tools) traceCall(ctx context.Context, in callIn) (any, error) {
	var p server.Partial
	pc, err := t.prepareCall(ctx, in, &p)
	if err != nil {
		return nil, err
	}
	cfg := map[string]any{"tracer": "callTracer", "timeout": "20s"}
	raw, err := t.r.Node.Raw(ctx, "debug_traceCall", pc.msg, pc.ref.Param(), cfg)
	if err != nil {
		return nil, stateErr(err)
	}
	out, err := t.formatTrace(ctx, "callTracer", raw)
	if err != nil {
		return nil, err
	}
	for k, v := range p.Errors {
		out.Fail(k, errors.New(v))
	}
	return out, nil
}

func (t *tools) formatTrace(ctx context.Context, tracer string, raw json.RawMessage) (*traceOut, error) {
	out := &traceOut{Tracer: tracer}
	switch tracer {
	case "callTracer":
		var root rawCall
		if err := json.Unmarshal(raw, &root); err != nil {
			return nil, fmt.Errorf("decoding trace: %w", err)
		}
		abis := map[util.Address]*abi.ABI{}
		var walk func(c *rawCall, depth int)
		walk = func(c *rawCall, depth int) {
			out.CallCount++
			if len(out.Calls) >= maxTraceCalls {
				out.Truncated = true
				return
			}
			n := callNode{
				Depth: depth, Type: c.Type, From: c.From.Hex(),
				Gas: uint64(c.Gas), GasUsed: uint64(c.GasUsed),
				Selector: evm.Selector(c.Input), InputLen: len(c.Input),
			}
			if c.Value != nil && (*big.Int)(c.Value).Sign() > 0 {
				v := lax.NewAmount((*big.Int)(c.Value))
				n.Value = &v
			}
			if c.To != nil {
				n.To = c.To.Hex()
				if len(c.Input) >= 4 && !strings.HasPrefix(c.Type, "CREATE") {
					a, seen := abis[*c.To]
					if !seen {
						var err error
						a, err = t.verifiedABI(ctx, *c.To)
						out.Fail("explorer", err)
						abis[*c.To] = a
					}
					if a != nil {
						n.Decoded = evm.DecodeCall(a, c.Input, "verified")
					}
					if n.Decoded == nil {
						n.Decoded = evm.DecodeCall(evm.StandardABI, c.Input, "standard")
					}
				}
			}
			if len(c.Output) > 0 && !strings.HasPrefix(c.Type, "CREATE") {
				n.Output, _ = evm.HexPreview(c.Output, 256)
			}
			if c.Error != "" {
				n.Error = untrusted.Ptr(c.Error)
			}
			if c.Revert != "" {
				n.Revert = untrusted.Ptr(c.Revert)
			} else if c.Error != "" && len(c.Output) > 4 {
				if reason, err := abi.UnpackRevert(c.Output); err == nil {
					n.Revert = untrusted.Ptr(reason)
				}
			}
			out.Calls = append(out.Calls, n)
			for i := range c.Calls {
				walk(&c.Calls[i], depth+1)
			}
		}
		walk(&root, 0)
	case "prestateTracer":
		out.Result = summarizePrestate(raw)
	default:
		var counts map[string]int
		if err := json.Unmarshal(raw, &counts); err != nil {
			return nil, fmt.Errorf("decoding trace: %w", err)
		}
		type sel struct {
			Selector  string `json:"selector"`
			ArgsBytes string `json:"calldata_bytes_after_selector"`
			Count     int    `json:"count"`
		}
		var list []sel
		for k, v := range counts {
			s, size, _ := strings.Cut(k, "-")
			list = append(list, sel{s, size, v})
		}
		out.Result = list
	}
	return out, nil
}

// summarizePrestate reshapes a prestate trace: every account the
// transaction touched, with its balance, nonce, code size and the storage
// slots it read or wrote, as they were before the transaction. Parallax's
// tracer has no diff mode; should a node return the {pre, post} diff form,
// both sides are shown.
func summarizePrestate(raw json.RawMessage) any {
	type account struct {
		Balance *hexutil.Big      `json:"balance"`
		Nonce   *uint64           `json:"nonce"`
		Code    hexutil.Bytes     `json:"code"`
		Storage map[string]string `json:"storage"`
	}
	side := func(a account) map[string]any {
		m := map[string]any{}
		if a.Balance != nil {
			m["balance"] = lax.NewAmount((*big.Int)(a.Balance))
		}
		if a.Nonce != nil {
			m["nonce"] = *a.Nonce
		}
		if len(a.Code) > 0 {
			m["code_bytes"] = len(a.Code)
		}
		if len(a.Storage) > 0 {
			st, i := map[string]string{}, 0
			for k, v := range a.Storage {
				if i == 50 {
					m["storage_truncated"] = true
					break
				}
				st[k] = v
				i++
			}
			m["storage"] = st
		}
		return m
	}

	var diff struct {
		Pre  map[string]account `json:"pre"`
		Post map[string]account `json:"post"`
	}
	if json.Unmarshal(raw, &diff) == nil && diff.Pre != nil && diff.Post != nil {
		out := map[string]any{}
		for a, pre := range diff.Pre {
			out[util.HexToAddress(a).Hex()] = map[string]any{"before": side(pre)}
		}
		for a, post := range diff.Post {
			key := util.HexToAddress(a).Hex()
			entry, _ := out[key].(map[string]any)
			if entry == nil {
				entry = map[string]any{}
			}
			entry["after"] = side(post)
			out[key] = entry
		}
		return map[string]any{"changed_accounts": out}
	}
	var pre map[string]account
	if err := json.Unmarshal(raw, &pre); err != nil {
		return json.RawMessage(raw)
	}
	out := map[string]any{}
	for a, acc := range pre {
		out[util.HexToAddress(a).Hex()] = side(acc)
	}
	return map[string]any{"touched_accounts_before": out}
}

// revertInfo extracts a revert from an eth_call or eth_estimateGas error:
// the decoded Error(string) reason when there is one, else the raw data.
func revertInfo(err error) (*untrusted.Text, string, bool) {
	var de rpc.DataError
	if !errors.As(err, &de) {
		if strings.Contains(err.Error(), "execution reverted") {
			return untrusted.Ptr(err.Error()), "0x", true
		}
		return nil, "", false
	}
	s, _ := de.ErrorData().(string)
	data, derr := hexutil.Decode(s)
	if derr != nil {
		return untrusted.Ptr(err.Error()), "0x", true
	}
	if reason, uerr := abi.UnpackRevert(data); uerr == nil {
		return untrusted.Ptr(reason), hexutil.Encode(data), true
	}
	msg := "reverted without a reason string"
	if len(data) >= 4 {
		msg = "reverted with custom error selector 0x" + hex.EncodeToString(data[:4])
	}
	t := untrusted.New(msg)
	return &t, hexutil.Encode(data), true
}

func keccak(b []byte) []byte { return crypto.Keccak256(b) }
