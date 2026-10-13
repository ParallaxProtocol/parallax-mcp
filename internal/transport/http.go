// Package transport serves the MCP server over stdio or Streamable HTTP.
package transport

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/time/rate"

	"github.com/ParallaxProtocol/parallax-mcp/internal/config"
	"github.com/ParallaxProtocol/parallax-mcp/internal/metrics"
)

// Stdio serves one client over standard input and output until it
// disconnects or ctx ends. Logs must go to stderr: stdout is the protocol.
func Stdio(ctx context.Context, srv *mcp.Server) error {
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// Handler returns the HTTP handler: the MCP endpoint at cfg.HTTPPath
// behind per-client rate limiting and a concurrency cap, and /healthz.
func Handler(cfg config.Config, srv *mcp.Server, log *slog.Logger) http.Handler {
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		// Every tool is a stateless read, so no session needs to outlive a
		// request; this lets the deployment scale out behind a load balancer.
		Stateless:           true,
		JSONResponse:        true,
		Logger:              log,
		MaxRequestBodyBytes: 1 << 20,
		// DNS-rebinding protection refuses loopback connections whose Host
		// is not loopback, which is what a reverse proxy on the same host
		// sends. Configuring trusted proxies declares such a deployment.
		DisableLocalhostProtection: len(cfg.TrustedProxies) > 0,
	})

	var proxies []netip.Prefix
	for _, p := range cfg.TrustedProxies {
		if pr, err := config.ParsePrefix(p); err == nil {
			proxies = append(proxies, pr)
		}
	}
	lim := newLimiter(rate.Limit(cfg.RateLimit), cfg.RateBurst)
	sem := make(chan struct{}, cfg.MaxConcurrent)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle(cfg.HTTPPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := ClientIP(r, proxies)
		if !lim.allow(ip) {
			metrics.RateLimited()
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		default:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "server busy", http.StatusServiceUnavailable)
			return
		}
		mcpHandler.ServeHTTP(w, r)
	}))
	return mux
}

// Serve listens on cfg.HTTPAddr until ctx ends, then shuts down gracefully.
func Serve(ctx context.Context, cfg config.Config, h http.Handler, log *slog.Logger) error {
	hs := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      cfg.ToolTimeout + 15*time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	log.Info("listening", "transport", "http", "addr", cfg.HTTPAddr, "path", cfg.HTTPPath)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := hs.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ClientIP returns the address to rate-limit by: the socket peer, or, when
// the peer is a trusted proxy, the right-most X-Forwarded-For entry that is
// not itself a trusted proxy.
func ClientIP(r *http.Request, trusted []netip.Prefix) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	peer = peer.Unmap()
	if !inPrefixes(peer, trusted) {
		return peer
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		a = a.Unmap()
		if !inPrefixes(a, trusted) {
			return a
		}
	}
	return peer
}

func inPrefixes(a netip.Addr, ps []netip.Prefix) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// limiter is a token bucket per client address, forgotten after 10 idle
// minutes.
type limiter struct {
	mu      sync.Mutex
	r       rate.Limit
	burst   int
	clients map[netip.Addr]*client
	sweep   time.Time
}

type client struct {
	lim  *rate.Limiter
	seen time.Time
}

func newLimiter(r rate.Limit, burst int) *limiter {
	return &limiter{r: r, burst: burst, clients: map[netip.Addr]*client{}, sweep: time.Now()}
}

func (l *limiter) allow(ip netip.Addr) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.sweep) > time.Minute {
		for k, c := range l.clients {
			if now.Sub(c.seen) > 10*time.Minute {
				delete(l.clients, k)
			}
		}
		l.sweep = now
	}
	c, ok := l.clients[ip]
	if !ok {
		c = &client{lim: rate.NewLimiter(l.r, l.burst)}
		l.clients[ip] = c
	}
	c.seen = now
	return c.lim.AllowN(now, 1)
}
