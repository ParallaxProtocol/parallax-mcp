package resources_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ParallaxProtocol/parallax-mcp/internal/prompts"
	"github.com/ParallaxProtocol/parallax-mcp/internal/resources"
	"github.com/ParallaxProtocol/parallax-mcp/internal/server"
	"github.com/ParallaxProtocol/parallax-mcp/internal/testutil"
)

func read(t *testing.T, e *testutil.Env, uri string) (string, error) {
	res, err := e.Client.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		return "", err
	}
	return res.Contents[0].Text, nil
}

func newEnv(t *testing.T) *testutil.Env {
	return testutil.NewEnv(t, []func(*server.Registrar){resources.Register, prompts.Register}, nil)
}

func TestChainParameters(t *testing.T) {
	text, err := read(t, newEnv(t), "parallax://chain/parameters")
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(text), &p); err != nil || p["chain_id"] != float64(2110) {
		t.Fatalf("parameters: %v %v", err, p["chain_id"])
	}
}

func TestDocsIndexAndPages(t *testing.T) {
	e := newEnv(t)
	index, err := read(t, e, "parallax://docs")
	if err != nil || !strings.Contains(index, "parallax://docs/guides/mining/pool") {
		t.Fatalf("index: %v", err)
	}
	page, err := read(t, e, "parallax://docs/guides/mining/pool")
	if err != nil || !strings.Contains(page, "Mining on a Pool") {
		t.Fatalf("page: %v", err)
	}
	for _, uri := range []string{"parallax://docs/../resources.go", "parallax://docs/guides/../../glossary", "parallax://docs//etc/passwd", "parallax://docs/nope"} {
		if _, err := read(t, e, uri); err == nil {
			t.Errorf("%s: want not found", uri)
		}
	}
}

func TestPIPs(t *testing.T) {
	e := newEnv(t)
	index, err := read(t, e, "parallax://pips")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"0002", "0003", "0004", "0005", "0006"} {
		if !strings.Contains(index, "parallax://pips/pip-"+n) {
			t.Errorf("PIP-%s missing from the index", n)
		}
	}
	if text, err := read(t, e, "parallax://pips/pip-0002"); err != nil || !strings.Contains(text, "ASERT") {
		t.Fatalf("pip-0002: %v", err)
	}
}

func TestPrompts(t *testing.T) {
	e := newEnv(t)
	res, err := e.Client.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "explain_transaction",
		Arguments: map[string]string{"hash": "0x3afc1e38981d6246473bb324abfeff063dc5cec044d96d01802db20b184824f0"}})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Messages[0].Content.(*mcp.TextContent).Text
	if !strings.Contains(text, "get_receipt") || !strings.Contains(text, "untrusted") {
		t.Fatalf("prompt: %s", text)
	}
	if _, err := e.Client.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "audit_address", Arguments: map[string]string{"address": "not an address"}}); err == nil {
		t.Fatal("bad address accepted")
	}
}
