// Package tools lists every tool group's registration function.
package tools

import (
	"github.com/ParallaxProtocol/parallax-mcp/internal/server"
	"github.com/ParallaxProtocol/parallax-mcp/internal/tools/chain"
	"github.com/ParallaxProtocol/parallax-mcp/internal/tools/explorer"
	"github.com/ParallaxProtocol/parallax-mcp/internal/tools/mempool"
	"github.com/ParallaxProtocol/parallax-mcp/internal/tools/mining"
	"github.com/ParallaxProtocol/parallax-mcp/internal/tools/network"
)

// All registers every group; each skips itself when disabled in config.
var All = []func(*server.Registrar){
	chain.Register,
	explorer.Register,
	mining.Register,
	network.Register,
	mempool.Register,
}
