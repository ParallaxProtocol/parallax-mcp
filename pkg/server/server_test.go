package server_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ParallaxProtocol/parallax-mcp/pkg/config"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/server"
)

// instructions returns what a client is told at initialization.
func instructions(t *testing.T, deps server.Deps) string {
	t.Helper()
	ctx := context.Background()
	reg := server.New(ctx, deps)
	ct, st := mcp.NewInMemoryTransports()
	ss, err := reg.MCP().Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	return cs.InitializeResult().Instructions
}

// A program embedding the server tells clients about itself, keeping the
// conventions the tools' results follow.
func TestInstructions(t *testing.T) {
	deps := server.Deps{Cfg: config.Default(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if got := instructions(t, deps); got != server.Instructions || !strings.HasPrefix(got, "Read-only access to the Parallax network (chain ID 2110): a proof-of-work") {
		t.Fatalf("default instructions: %q", got)
	}
	deps.Instructions = "Parallax Client's node and wallet: " + server.Chain + "\n\n" + server.Conventions
	if got := instructions(t, deps); got != deps.Instructions {
		t.Fatalf("own instructions: %q", got)
	}
}
