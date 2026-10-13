// Command parallax-mcp is a Model Context Protocol server for the Parallax
// network: chain data from a node, the Blockscout explorer and the network
// status page, served to AI assistants over stdio or Streamable HTTP.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ParallaxProtocol/parallax/v2/util/hexutil"

	"github.com/ParallaxProtocol/parallax-mcp/internal/backend/blockscout"
	"github.com/ParallaxProtocol/parallax-mcp/internal/backend/node"
	"github.com/ParallaxProtocol/parallax-mcp/internal/backend/status"
	"github.com/ParallaxProtocol/parallax-mcp/internal/config"
	"github.com/ParallaxProtocol/parallax-mcp/internal/httpx"
	"github.com/ParallaxProtocol/parallax-mcp/internal/metrics"
	"github.com/ParallaxProtocol/parallax-mcp/internal/server"
	"github.com/ParallaxProtocol/parallax-mcp/internal/tools"
	"github.com/ParallaxProtocol/parallax-mcp/internal/transport"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(os.Args[2:]))
	}
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println("parallax-mcp", server.Version)
		return
	}
	if err := run(); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "parallax-mcp:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Args[1:], os.Getenv, os.Stderr)
	if err != nil {
		return err
	}
	log := newLogger(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	deps, err := backends(cfg, log)
	if err != nil {
		return err
	}
	defer deps.Node.Close()

	if err := checkChain(ctx, deps.Node, cfg.ChainID, log); err != nil {
		return err
	}

	reg := server.New(ctx, deps, tools.All...)

	if cfg.MetricsAddr != "" {
		go serveMetrics(ctx, cfg.MetricsAddr, log)
	}

	switch cfg.Transport {
	case config.TransportHTTP:
		if cfg.BroadcastEnabled() {
			log.Warn("broadcast_signed_tx is enabled on the http transport: remote clients can submit signed transactions")
		}
		return transport.Serve(ctx, cfg, transport.Handler(cfg, reg.MCP(), log), log)
	default:
		log.Info("serving", "transport", "stdio")
		err := transport.Stdio(ctx, reg.MCP())
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
}

func backends(cfg config.Config, log *slog.Logger) (server.Deps, error) {
	ua := "parallax-mcp/" + server.Version
	n, err := node.Dial(node.Options{
		URL:            cfg.RPCURL,
		Timeout:        cfg.BackendTimeout,
		Retries:        cfg.BackendRetries,
		AllowBroadcast: cfg.BroadcastEnabled(),
	})
	if err != nil {
		return server.Deps{}, err
	}
	deps := server.Deps{Cfg: cfg, Node: n, Log: log}
	if cfg.ExplorerURL != "" {
		deps.Explorer = blockscout.New(httpx.Options{BaseURL: cfg.ExplorerURL, Timeout: cfg.BackendTimeout, Retries: cfg.BackendRetries, UserAgent: ua})
	}
	if u := cfg.StatsURL(); u != "" {
		deps.ExplorerStats = blockscout.NewStats(httpx.Options{BaseURL: u, Timeout: cfg.BackendTimeout, Retries: cfg.BackendRetries, UserAgent: ua})
	}
	if cfg.StatusURL != "" {
		deps.Status = status.New(httpx.Options{BaseURL: cfg.StatusURL, Timeout: cfg.BackendTimeout, Retries: cfg.BackendRetries, UserAgent: ua}, cfg.StatusCacheTTL)
	}
	return deps, nil
}

// checkChain refuses to serve a node on another chain, which would make
// every answer, and any transaction built from them, wrong. An unreachable
// node is only logged: tools report its errors per call.
func checkChain(ctx context.Context, n *node.Client, want uint64, log *slog.Logger) error {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var id hexutil.Big
	if err := n.Call(cctx, &id, "eth_chainId"); err != nil {
		log.Warn("node unreachable at startup; continuing", "err", err)
		return nil
	}
	if got := (*big.Int)(&id); !got.IsUint64() || got.Uint64() != want {
		return fmt.Errorf("the node is on chain %s, not %d; check --rpc-url or set --chain-id", got, want)
	}
	log.Info("node on expected chain", "chain_id", want)
	return nil
}

func serveMetrics(ctx context.Context, addr string, log *slog.Logger) {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics.Handler())
	hs := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = hs.Close()
	}()
	log.Info("metrics listening", "addr", addr)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("metrics server", "err", err)
	}
}

// healthcheck is for container health checks, which cannot run curl in a
// distroless image: it exits 0 when GET <url> answers 200.
func healthcheck(args []string) int {
	url := "http://127.0.0.1:8080/healthz"
	if len(args) > 0 {
		url = args[0]
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "status", resp.StatusCode)
		return 1
	}
	return 0
}

func newLogger(cfg config.Config) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.LogLevel))
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}
