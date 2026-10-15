package chain_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ParallaxProtocol/parallax-mcp/internal/testutil"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/server"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/tools/chain"
)

const (
	owner    = "0x00000000000000000000000000000000000000aa"
	erc20    = "0x0000000000000000000000000000000000000020"
	nft      = "0x0000000000000000000000000000000000000721"
	router   = "0x0000000000000000000000000000000000000001" // verified contract, unlimited
	revoked  = "0x0000000000000000000000000000000000000002" // approved then revoked
	phisher  = "0x0000000000000000000000000000000000000003" // plain account, limited
	operator = "0x0000000000000000000000000000000000000004" // plain account, NFT operator
	consumed = "0x0000000000000000000000000000000000000005" // allowance fully spent since
)

func word(hex string) string { return fmt.Sprintf("%064s", strings.TrimPrefix(hex, "0x")) }

func topicOf(addr string) string { return "0x" + word(addr) }

func approvalLog(block int, token, spender, data string, forAll bool) map[string]any {
	t0 := "0x8c5be1e5ebec7d5bd14f71427d1e84f3dd0314c0f7b2291e5b200ac8c7c3b925"
	if forAll {
		t0 = "0x17307eab39ab6107e8899845ad3d59bd9653f200f220920489ca2b5937696c31"
	}
	return map[string]any{"address": token, "topics": []string{t0, topicOf(owner), topicOf(spender)}, "data": "0x" + word(data),
		"blockNumber": fmt.Sprintf("0x%x", block), "transactionHash": fmt.Sprintf("0x%064x", block), "logIndex": "0x0", "transactionIndex": "0x0"}
}

func approvalsEnv(t *testing.T, opts ...testutil.Options) *testutil.Env {
	max := strings.Repeat("f", 64)
	logs := []any{
		approvalLog(10, erc20, router, max, false),
		approvalLog(11, erc20, revoked, "64", false),
		approvalLog(12, erc20, revoked, "0", false),
		approvalLog(13, erc20, phisher, "64", false),
		approvalLog(14, nft, operator, "1", true),
		approvalLog(15, erc20, consumed, "64", false),
	}
	return testutil.NewEnv(t, []func(*server.Registrar){chain.Register}, func(e *testutil.Env) {
		e.Node.Result("eth_blockNumber", "0x100")
		e.Node.Result("eth_getLogs", logs)
		e.Node.Handle("eth_getCode", func(p []json.RawMessage) (any, *testutil.RPCError) {
			var a string
			_ = json.Unmarshal(p[0], &a)
			if strings.EqualFold(a, router) {
				return "0x6080", nil
			}
			return "0x", nil
		})
		e.Node.Handle("eth_call", func(p []json.RawMessage) (any, *testutil.RPCError) {
			var msg struct{ To, Data string }
			_ = json.Unmarshal(p[0], &msg)
			sel, args := msg.Data[:10], msg.Data[10:]
			switch sel {
			case "0xdd62ed3e": // allowance(owner, spender)
				switch "0x" + args[64+24:128] {
				case router:
					return "0x" + max, nil
				case phisher:
					return "0x" + word("64"), nil // 100
				default:
					return "0x" + word("0"), nil
				}
			case "0x70a08231": // balanceOf(owner)
				return "0x" + word("32"), nil // 50
			case "0xe985e9c5": // isApprovedForAll
				return "0x" + word("1"), nil
			case "0x95d89b41": // symbol()
				if strings.EqualFold(msg.To, erc20) {
					return "0x" + word("20") + word("4") + "54455354" + strings.Repeat("0", 56), nil
				}
				return nil, &testutil.RPCError{Code: -32000, Message: "execution reverted"}
			case "0x313ce567": // decimals()
				if strings.EqualFold(msg.To, erc20) {
					return "0x" + word("0"), nil
				}
				return nil, &testutil.RPCError{Code: -32000, Message: "execution reverted"}
			}
			return nil, &testutil.RPCError{Code: -32000, Message: "execution reverted"}
		})
		e.Explorer.Route("/api/v2/addresses/0x0000000000000000000000000000000000000001", `{"hash":"`+router+`","name":"Router","is_contract":true,"is_verified":true}`)
	}, opts...)
}

func TestAuditTokenApprovals(t *testing.T) {
	e := approvalsEnv(t)
	out := e.MustCall(t, "audit_token_approvals", map[string]any{"owner": owner})
	list := out["approvals"].([]any)
	if out["inactive"] != float64(2) || len(list) != 3 {
		t.Fatalf("want 3 active (router, phisher, operator) and 2 inactive (revoked, consumed): %v", out)
	}
	bySpender := map[string]map[string]any{}
	for _, a := range list {
		m := a.(map[string]any)
		bySpender[strings.ToLower(testutil.Get(m, "spender", "address").(string))] = m
	}

	r := bySpender[router]
	if r["unlimited"] != true || r["severity"] != "medium" || testutil.Get(r, "allowance", "tokens") != "unlimited" || testutil.Get(r, "spender", "name", "untrusted") != "Router" {
		t.Errorf("router: %v", r)
	}
	if testutil.Get(r, "token", "symbol", "untrusted") != "TEST" || testutil.Get(r, "exposure", "raw") != "50" {
		t.Errorf("router token/exposure: %v", r)
	}

	ph := bySpender[phisher]
	risks := fmt.Sprint(ph["risks"])
	if ph["severity"] != "high" || !strings.Contains(risks, "covers_balance") || !strings.Contains(risks, "spender_not_a_contract") {
		t.Errorf("phisher: %v", ph)
	}
	if testutil.Get(ph, "revoke", "function") != "approve(address,uint256)" || testutil.Get(ph, "revoke", "args", 1) != "0" {
		t.Errorf("phisher revoke: %v", ph["revoke"])
	}

	op := bySpender[operator]
	if op["kind"] != "nft_operator_for_all" || op["severity"] != "high" || testutil.Get(op, "revoke", "args", 1) != false {
		t.Errorf("operator: %v", op)
	}
	if testutil.Get(out, "active_by_severity", "high") != float64(2) {
		t.Errorf("counts: %v", out["active_by_severity"])
	}

	all := e.MustCall(t, "audit_token_approvals", map[string]any{"owner": owner, "include_inactive": true})
	if len(all["approvals"].([]any)) != 5 {
		t.Fatalf("include_inactive: %v", all["approvals"])
	}
}

func TestAuditTokenApprovalsBounds(t *testing.T) {
	e := approvalsEnv(t)
	if _, errText := e.Call(t, "audit_token_approvals", map[string]any{"owner": owner, "from_block": 99999}); errText == "" {
		t.Fatal("from_block past the head accepted")
	}
}

func TestAuditTokenApprovalsWithoutExplorer(t *testing.T) {
	e := approvalsEnv(t, testutil.NodeOnly)
	out := e.MustCall(t, "audit_token_approvals", map[string]any{"owner": owner})
	if len(out["approvals"].([]any)) != 3 || out["errors"] != nil {
		t.Fatalf("want the 3 active approvals and no errors: %v", out)
	}
	for _, a := range out["approvals"].([]any) {
		if testutil.Get(a, "spender", "name") != nil {
			t.Errorf("spender named without an explorer: %v", a)
		}
	}
}
