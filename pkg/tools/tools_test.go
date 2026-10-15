package tools_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ParallaxProtocol/parallax-mcp/internal/testutil"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/config"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/server"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/tools"
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
	// chain 16, explorer 11, mining 3, network 6, mempool 2, docs 2, tx 3.
	if len(ts) != 43 {
		t.Fatalf("%d tools registered, want 43", len(ts))
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

// A program embedding the server registers tools of its own that change
// its state: they need no --allow-broadcast and are annotated as changes.
func TestEmbedderToolsThatChangeState(t *testing.T) {
	register := func(r *server.Registrar) {
		server.Add(r, server.Spec{Group: "app", Name: "start_node", Title: "Start node", Changes: true,
			Description: "Starts the node of the app serving these tools, which it runs in process; it does nothing when the node already runs."},
			func(context.Context, struct{}) (any, error) { return map[string]any{"started": true}, nil })
		server.Add(r, server.Spec{Group: "app", Name: "remove_account", Title: "Remove account", Changes: true, Destructive: true,
			Description: "Removes an account from the wallet of the app serving these tools; the account is gone unless it was backed up first."},
			func(context.Context, struct{}) (any, error) { return map[string]any{}, nil })
	}
	e := testutil.NewEnv(t, []func(*server.Registrar){register}, nil, func(c *config.Config) { c.Groups = []string{"app"} })
	byName := map[string]*mcp.Tool{}
	for _, tool := range listTools(t, e) {
		byName[tool.Name] = tool
	}
	start, remove := byName["start_node"], byName["remove_account"]
	if start == nil || remove == nil {
		t.Fatalf("registered: %v", byName)
	}
	if a := start.Annotations; a.ReadOnlyHint || *a.DestructiveHint || *a.OpenWorldHint {
		t.Errorf("start_node annotations: %+v", a)
	}
	if a := remove.Annotations; a.ReadOnlyHint || !*a.DestructiveHint {
		t.Errorf("remove_account annotations: %+v", a)
	}
	if out := e.MustCall(t, "start_node", nil); out["started"] != true {
		t.Fatalf("got %v", out)
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
