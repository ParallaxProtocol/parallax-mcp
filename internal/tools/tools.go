// Package tools lists every tool group's registration function.
package tools

import (
	"github.com/ParallaxProtocol/parallax-mcp/internal/prompts"
	"github.com/ParallaxProtocol/parallax-mcp/internal/resources"
	"github.com/ParallaxProtocol/parallax-mcp/internal/server"
	"github.com/ParallaxProtocol/parallax-mcp/internal/tools/chain"
	"github.com/ParallaxProtocol/parallax-mcp/internal/tools/docs"
	"github.com/ParallaxProtocol/parallax-mcp/internal/tools/explorer"
	"github.com/ParallaxProtocol/parallax-mcp/internal/tools/live"
	"github.com/ParallaxProtocol/parallax-mcp/internal/tools/mempool"
	"github.com/ParallaxProtocol/parallax-mcp/internal/tools/mining"
	"github.com/ParallaxProtocol/parallax-mcp/internal/tools/network"
	"github.com/ParallaxProtocol/parallax-mcp/internal/tools/tx"
)

// All registers every tool group, the resources and the prompts; each group
// skips itself when disabled in config.
var All = []func(*server.Registrar){
	chain.Register,
	live.Register,
	explorer.Register,
	mining.Register,
	network.Register,
	mempool.Register,
	tx.Register,
	docs.Register,
	resources.Register,
	prompts.Register,
}
