// Package tools lists every tool group's registration function.
package tools

import (
	"github.com/ParallaxProtocol/parallax-mcp/pkg/prompts"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/resources"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/server"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/tools/chain"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/tools/docs"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/tools/explorer"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/tools/live"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/tools/mempool"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/tools/mining"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/tools/network"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/tools/tx"
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
