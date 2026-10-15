package tx_test

import (
	"context"
	"encoding/json"
	"math/big"
	"slices"
	"strings"
	"testing"

	"github.com/ParallaxProtocol/parallax/v2/crypto"
	"github.com/ParallaxProtocol/parallax/v2/primitives/types"
	"github.com/ParallaxProtocol/parallax/v2/util"
	"github.com/ParallaxProtocol/parallax/v2/util/hexutil"

	"github.com/ParallaxProtocol/parallax-mcp/internal/testutil"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/config"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/server"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/tools/tx"
)

const recipient = "0x2CAa9795cd62261f96e40B8448b33636FF5e249a"

var chainID = big.NewInt(2110)

type env struct {
	*testutil.Env
	sent []string
}

func newEnv(t *testing.T, broadcast bool, opts ...testutil.Options) *env {
	e := &env{}
	e.Env = testutil.NewEnv(t, []func(*server.Registrar){tx.Register}, func(te *testutil.Env) {
		te.Node.Result("eth_getTransactionCount", "0x7")
		te.Node.Result("eth_gasPrice", "0x3b9aca00")
		te.Node.Result("eth_getBalance", "0xde0b6b3a7640000")                                                                         // 1 LAX
		te.Node.Result("eth_estimateSmartFee", map[string]any{"gasPrice": "0x77359400", "successRate": 0.9, "legacyFallback": false}) // 2 gwei
		te.Node.Handle("eth_estimateGas", func(p []json.RawMessage) (any, *testutil.RPCError) {
			var msg struct{ Data string }
			_ = json.Unmarshal(p[0], &msg)
			if msg.Data == "0x" || msg.Data == "" {
				return "0x5208", nil
			}
			return "0xc350", nil // 50,000
		})
		te.Node.Handle("eth_sendRawTransaction", func(p []json.RawMessage) (any, *testutil.RPCError) {
			var raw string
			_ = json.Unmarshal(p[0], &raw)
			e.sent = append(e.sent, raw)
			b, _ := hexutil.Decode(raw)
			var parsed types.Transaction
			_ = parsed.UnmarshalBinary(b)
			return parsed.Hash().Hex(), nil
		})
	}, append([]testutil.Options{func(c *config.Config) {
		c.Groups = []string{config.GroupTx}
		c.AllowBroadcast = broadcast
	}}, opts...)...)
	return e
}

// sign plays the external signer: it signs the server's signing hash and
// attaches the signature to the unsigned transaction.
func sign(t *testing.T, out map[string]any) (string, util.Address) {
	t.Helper()
	key, _ := crypto.GenerateKey()
	sig, err := crypto.Sign(hexutil.MustDecode(out["signing_hash"].(string)), key)
	if err != nil {
		t.Fatal(err)
	}
	var unsigned types.Transaction
	if err := unsigned.UnmarshalBinary(hexutil.MustDecode(out["unsigned_tx"].(string))); err != nil {
		t.Fatal(err)
	}
	signer := types.LatestSignerForChainID(chainID)
	signed, err := unsigned.WithSignature(signer, sig)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := signed.MarshalBinary()
	return hexutil.Encode(raw), crypto.PubkeyToAddress(key.PublicKey)
}

func TestBuildLegacyTransfer(t *testing.T) {
	e := newEnv(t, false)
	out := e.MustCall(t, "build_unsigned_tx", map[string]any{"from": recipient, "to": recipient, "value_lax": "0.25"})
	if out["nonce"] != float64(7) || out["gas_limit"] != float64(21000) || out["gas_price_gwei"] != "2" || out["chain_id"] != float64(2110) {
		t.Fatalf("fields: %v", out)
	}
	if testutil.Get(out, "value", "wei") != "250000000000000000" || testutil.Get(out, "max_cost", "lax") != "0.250042" {
		t.Fatalf("amounts: %v %v", out["value"], out["max_cost"])
	}
	if out["warnings"] != nil {
		t.Fatalf("unexpected warnings: %v", out["warnings"])
	}

	raw, signer := sign(t, out)
	dec := e.MustCall(t, "decode_tx", map[string]any{"raw": raw})
	if dec["from"] != signer.Hex() || dec["signed"] != true || dec["replay_protected"] != true || dec["chain_id"] != float64(2110) {
		t.Fatalf("decoded: %v", dec)
	}
}

func TestBuildContractCallAndCreation(t *testing.T) {
	e := newEnv(t, false)
	out := e.MustCall(t, "build_unsigned_tx", map[string]any{
		"from": recipient, "to": recipient, "function": "transfer(address,uint256)", "args": []any{recipient, "1000"}, "type": "access_list",
	})
	// 50,000 estimated + 10% headroom.
	if out["gas_limit"] != float64(55000) || out["function"] != "transfer(address,uint256)" || !strings.HasPrefix(out["data"].(string), "0xa9059cbb") {
		t.Fatalf("call: %v", out)
	}
	if !strings.HasPrefix(out["unsigned_tx"].(string), "0x01") || !strings.HasPrefix(out["signing_payload"].(string), "0x01") {
		t.Fatalf("type 1 encoding: %v", out)
	}
	raw, signer := sign(t, out)
	dec := e.MustCall(t, "decode_tx", map[string]any{"raw": raw})
	if dec["from"] != signer.Hex() || dec["type"] != "access_list" || testutil.Get(dec, "decoded_input", "signature") != "transfer(address,uint256)" {
		t.Fatalf("decoded: %v", dec)
	}

	// Contract creation: no recipient, nil "to" in the signing payload.
	out = e.MustCall(t, "build_unsigned_tx", map[string]any{"from": recipient, "data": "0x6080604052"})
	raw, signer = sign(t, out)
	dec = e.MustCall(t, "decode_tx", map[string]any{"raw": raw})
	if dec["creates_contract"] != true || dec["from"] != signer.Hex() {
		t.Fatalf("creation: %v", dec)
	}
}

func TestBuildWarnsOnBalanceAndNonce(t *testing.T) {
	e := newEnv(t, false)
	out := e.MustCall(t, "build_unsigned_tx", map[string]any{"from": recipient, "to": recipient, "value_lax": "5", "nonce": 3})
	w := strings.Join(toStrings(out["warnings"]), " | ")
	if !strings.Contains(w, "balance does not cover") || !strings.Contains(w, "not the sender's next nonce 7") {
		t.Fatalf("warnings: %s", w)
	}
}

func TestBuildRejectsBadInput(t *testing.T) {
	e := newEnv(t, false)
	for name, args := range map[string]map[string]any{
		"type 2":      {"from": recipient, "to": recipient, "type": "eip1559"},
		"both values": {"from": recipient, "to": recipient, "value_lax": "1", "value_wei": "1"},
		"too precise": {"from": recipient, "to": recipient, "value_lax": "0.0000000000000000001"},
		"nothing":     {"from": recipient},
		"bad from":    {"from": "me", "to": recipient},
		"big number":  {"from": recipient, "to": recipient, "function": "f(uint256)", "args": []any{1e30}},
	} {
		if _, errText := e.Call(t, "build_unsigned_tx", args); errText == "" {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestDecodeSigningPayload(t *testing.T) {
	e := newEnv(t, false)
	out := e.MustCall(t, "build_unsigned_tx", map[string]any{"from": recipient, "to": recipient, "value_wei": "1"})
	dec := e.MustCall(t, "decode_tx", map[string]any{"raw": out["signing_payload"]})
	if dec["format"] != "eip155_signing_payload" || dec["chain_id"] != float64(2110) || dec["signed"] != false {
		t.Fatalf("got %v", dec)
	}
}

func TestDecodeUnsignedLegacy(t *testing.T) {
	e := newEnv(t, false)
	out := e.MustCall(t, "build_unsigned_tx", map[string]any{"from": recipient, "to": recipient, "value_wei": "1"})
	dec := e.MustCall(t, "decode_tx", map[string]any{"raw": out["unsigned_tx"]})
	if dec["chain_id"] != nil || dec["signed"] != false || !strings.Contains(strings.Join(toStrings(dec["warnings"]), " "), "chain ID is set when it is signed") {
		t.Fatalf("got %v", dec)
	}
}

func TestDescriptionsNameConfiguredChain(t *testing.T) {
	e := newEnv(t, false, func(c *config.Config) { c.ChainID = 1337 })
	res, err := e.Client.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if strings.Contains(tool.Description, "2110") || !strings.Contains(tool.Description, "1337") {
			t.Errorf("%s: description does not name chain 1337: %q", tool.Name, tool.Description)
		}
	}
}

func TestPrivateKeyShapedInputRefused(t *testing.T) {
	e := newEnv(t, true)
	key := "0x" + strings.Repeat("4c", 32)
	for _, tool := range []string{"decode_tx", "broadcast_signed_tx"} {
		_, errText := e.Call(t, tool, map[string]any{"raw": key})
		if !strings.Contains(errText, "never accepts private keys") {
			t.Errorf("%s: got %q", tool, errText)
		}
		if strings.Contains(errText, strings.Repeat("4c", 32)) {
			t.Errorf("%s echoed the key back", tool)
		}
	}
}

func TestBroadcastOnlyWithFlag(t *testing.T) {
	if slices.Contains(newEnv(t, false).Tools(t), "broadcast_signed_tx") {
		t.Fatal("broadcast_signed_tx registered without --allow-broadcast")
	}
	if !slices.Contains(newEnv(t, true).Tools(t), "broadcast_signed_tx") {
		t.Fatal("broadcast_signed_tx missing with --allow-broadcast")
	}
}

func TestBroadcastChecks(t *testing.T) {
	e := newEnv(t, true)
	out := e.MustCall(t, "build_unsigned_tx", map[string]any{"from": recipient, "to": recipient, "value_wei": "1"})

	if _, errText := e.Call(t, "broadcast_signed_tx", map[string]any{"raw": out["unsigned_tx"]}); !strings.Contains(errText, "not signed") {
		t.Fatalf("unsigned: %q", errText)
	}

	key, _ := crypto.GenerateKey()
	to := util.HexToAddress(recipient)
	legacy := &types.LegacyTx{Nonce: 7, GasPrice: big.NewInt(1e9), Gas: 21000, To: &to, Value: big.NewInt(1)}
	unprotected, _ := types.SignTx(types.NewTx(legacy), types.HomesteadSigner{}, key)
	raw, _ := unprotected.MarshalBinary()
	if _, errText := e.Call(t, "broadcast_signed_tx", map[string]any{"raw": hexutil.Encode(raw)}); !strings.Contains(errText, "replay protection") {
		t.Fatalf("unprotected: %q", errText)
	}
	other, _ := types.SignTx(types.NewTx(legacy), types.NewEIP155Signer(big.NewInt(1)), key)
	raw, _ = other.MarshalBinary()
	if _, errText := e.Call(t, "broadcast_signed_tx", map[string]any{"raw": hexutil.Encode(raw)}); !strings.Contains(errText, "Parallax is 2110") {
		t.Fatalf("other chain: %q", errText)
	}
	if len(e.sent) != 0 {
		t.Fatal("a refused transaction reached the node")
	}

	signed, _ := sign(t, out)
	res := e.MustCall(t, "broadcast_signed_tx", map[string]any{"raw": signed})
	if res["broadcast"] != true || len(e.sent) != 1 || e.sent[0] != signed {
		t.Fatalf("broadcast: %v, sent %v", res, e.sent)
	}
}

func toStrings(v any) []string {
	var out []string
	for _, s := range v.([]any) {
		out = append(out, s.(string))
	}
	return out
}

func TestWithoutExplorer(t *testing.T) {
	e := newEnv(t, false, testutil.NodeOnly)
	_, errText := e.Call(t, "build_unsigned_tx", map[string]any{"from": recipient, "to": recipient, "function": "transfer", "args": []any{recipient, "1"}})
	if !strings.Contains(errText, "no explorer is configured") {
		t.Fatalf("bare function name: %q", errText)
	}
	out := e.MustCall(t, "build_unsigned_tx", map[string]any{"from": recipient, "to": recipient, "function": "transfer(address,uint256)", "args": []any{recipient, "1"}})
	raw, _ := sign(t, out)
	dec := e.MustCall(t, "decode_tx", map[string]any{"raw": raw})
	if testutil.Get(dec, "decoded_input", "source") != "standard" || dec["errors"] != nil {
		t.Fatalf("decoded: %v", dec)
	}
}
