package tools_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ParallaxProtocol/parallax-mcp/internal/config"
	"github.com/ParallaxProtocol/parallax-mcp/internal/testutil"
	"github.com/ParallaxProtocol/parallax-mcp/internal/tools"
)

func fullNode(e *testutil.Env) {
	e.Node.Result("rpc_modules", map[string]string{"eth": "1.0", "net": "1.0", "web3": "1.0", "debug": "1.0", "txpool": "1.0", "admin": "1.0"})
}

func listTools(t *testing.T, e *testutil.Env) []*mcp.Tool {
	res, err := e.Client.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return res.Tools
}

func TestDefaultConfigIsReadOnly(t *testing.T) {
	e := testutil.NewEnv(t, tools.All, fullNode, func(c *config.Config) { c.Groups = config.DefaultGroups })
	for _, tool := range listTools(t, e) {
		if !tool.Annotations.ReadOnlyHint || *tool.Annotations.DestructiveHint {
			t.Errorf("%s is not annotated read-only", tool.Name)
		}
		if strings.HasSuffix(tool.Name, "_tx") {
			t.Errorf("%s registered although the tx group is off by default", tool.Name)
		}
	}
}

func TestEveryToolIsDescribedAndStrict(t *testing.T) {
	e := testutil.NewEnv(t, tools.All, fullNode, func(c *config.Config) { c.AllowBroadcast = true })
	ts := listTools(t, e)
	// chain 16, explorer 11, mining 3, network 6, mempool 2, tx 3.
	if len(ts) != 41 {
		t.Fatalf("%d tools registered, want 41", len(ts))
	}
	for _, tool := range ts {
		if len(tool.Description) < 80 {
			t.Errorf("%s: description too short to be useful", tool.Name)
		}
		var schema struct {
			Type                 string `json:"type"`
			AdditionalProperties *bool  `json:"additionalProperties"`
		}
		b, _ := json.Marshal(tool.InputSchema)
		_ = json.Unmarshal(b, &schema)
		if schema.Type != "object" || schema.AdditionalProperties == nil || *schema.AdditionalProperties {
			t.Errorf("%s: input schema should be a closed object, got %s", tool.Name, b)
		}
	}
	names := make([]string, len(ts))
	for i, tool := range ts {
		names[i] = tool.Name
	}
	if !slices.Contains(names, "broadcast_signed_tx") {
		t.Fatal("broadcast_signed_tx missing with the flag")
	}
	for _, tool := range ts {
		if tool.Name == "broadcast_signed_tx" && (tool.Annotations.ReadOnlyHint || !*tool.Annotations.DestructiveHint) {
			t.Fatal("broadcast_signed_tx must be annotated destructive")
		}
	}
}

func TestUnknownArgumentsRejected(t *testing.T) {
	e := testutil.NewEnv(t, tools.All, nil)
	_, errText := e.Call(t, "get_balance", map[string]any{"address": "0x45a117abe265623239a3c7a050ea7e8d7b040248", "private_key": "0x01"})
	if errText == "" {
		t.Fatal("an unexpected argument was accepted")
	}
}

func TestGroupsCanBeDisabled(t *testing.T) {
	e := testutil.NewEnv(t, tools.All, nil, func(c *config.Config) { c.Groups = []string{config.GroupMining} })
	for _, tool := range listTools(t, e) {
		if !slices.Contains([]string{"mining_stats", "supply_info", "block_reward"}, tool.Name) {
			t.Errorf("%s registered with only the mining group on", tool.Name)
		}
	}
}
