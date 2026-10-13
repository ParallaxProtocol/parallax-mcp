// Package config loads the server configuration from defaults, an optional
// YAML file, PARALLAX_MCP_* environment variables and command-line flags, in
// increasing order of precedence.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Tool groups. Every tool belongs to exactly one.
const (
	GroupChain    = "chain"
	GroupMining   = "mining"
	GroupExplorer = "explorer"
	GroupNetwork  = "network"
	GroupMempool  = "mempool"
	GroupTx       = "tx"
)

// AllGroups lists every group in display order.
var AllGroups = []string{GroupChain, GroupMining, GroupExplorer, GroupNetwork, GroupMempool, GroupTx}

// DefaultGroups is every group except tx, which must be enabled explicitly.
var DefaultGroups = []string{GroupChain, GroupMining, GroupExplorer, GroupNetwork, GroupMempool}

// Transports.
const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"
)

// Config is the full server configuration. YAML keys match the flag names
// with dashes replaced by underscores.
type Config struct {
	Transport string `yaml:"transport"`

	RPCURL      string `yaml:"rpc_url"`
	ExplorerURL string `yaml:"explorer_url"`
	StatusURL   string `yaml:"status_url"`
	ChainID     uint64 `yaml:"chain_id"`

	Groups         []string `yaml:"groups"`
	AllowBroadcast bool     `yaml:"allow_broadcast"`
	// ExposePeerAddresses reveals peer IP addresses in node_peers. Honoured on
	// stdio only: a remote deployment never publishes other operators' IPs.
	ExposePeerAddresses bool `yaml:"expose_peer_addresses"`

	HTTPAddr string `yaml:"http_addr"`
	// HTTPStateless serves every HTTP request without a session, so replicas
	// behind a load balancer need no stickiness, at the cost of resource
	// subscriptions.
	HTTPStateless  bool     `yaml:"http_stateless"`
	HTTPPath       string   `yaml:"http_path"`
	RateLimit      float64  `yaml:"rate_limit"` // requests per second per client
	RateBurst      int      `yaml:"rate_burst"`
	MaxConcurrent  int      `yaml:"max_concurrent"`
	TrustedProxies []string `yaml:"trusted_proxies"`

	MetricsAddr string `yaml:"metrics_addr"`
	LogLevel    string `yaml:"log_level"`
	LogFormat   string `yaml:"log_format"`

	BackendTimeout   time.Duration `yaml:"backend_timeout"`
	ToolTimeout      time.Duration `yaml:"tool_timeout"`
	BackendRetries   int           `yaml:"backend_retries"`
	StatusCacheTTL   time.Duration `yaml:"status_cache_ttl"`
	MaxResponseBytes int           `yaml:"max_response_bytes"`
	MaxLogRange      uint64        `yaml:"max_log_range"`
	HeadPollInterval time.Duration `yaml:"head_poll_interval"`
	ExplorerStatsURL string        `yaml:"explorer_stats_url"`
	MaxLogs          int           `yaml:"max_logs"`
}

// Default returns the built-in defaults: the public Parallax endpoints and a
// read-only tool set.
func Default() Config {
	return Config{
		Transport:        TransportStdio,
		RPCURL:           "https://rpc.parallaxprotocol.org",
		ExplorerURL:      "https://explorer.parallaxprotocol.org",
		StatusURL:        "https://status.parallaxprotocol.org",
		ChainID:          2110,
		Groups:           slices.Clone(DefaultGroups),
		HTTPAddr:         "127.0.0.1:8080",
		HTTPPath:         "/mcp",
		RateLimit:        2,
		RateBurst:        20,
		MaxConcurrent:    64,
		LogLevel:         "info",
		LogFormat:        "json",
		BackendTimeout:   15 * time.Second,
		ToolTimeout:      45 * time.Second,
		BackendRetries:   2,
		StatusCacheTTL:   15 * time.Second,
		MaxResponseBytes: 256 << 10,
		MaxLogRange:      10_000,
		HeadPollInterval: 15 * time.Second,
		MaxLogs:          500,
	}
}

// Load builds the configuration from args (without the program name) and
// the environment. getenv is os.Getenv outside tests.
func Load(args []string, getenv func(string) string, stderr io.Writer) (Config, error) {
	cfg := Default()

	fs := flag.NewFlagSet("parallax-mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", getenv("PARALLAX_MCP_CONFIG"), "optional YAML config file (env PARALLAX_MCP_CONFIG)")

	// The flags are declared against a scratch config so that their defaults
	// can show in -help without being applied over the YAML file.
	var f Config = cfg
	groups := strings.Join(cfg.Groups, ",")
	proxies := ""
	fs.StringVar(&f.Transport, "transport", cfg.Transport, `"stdio" or "http"`)
	fs.StringVar(&f.RPCURL, "rpc-url", cfg.RPCURL, "Parallax JSON-RPC endpoint")
	fs.StringVar(&f.ExplorerURL, "explorer-url", cfg.ExplorerURL, "Blockscout explorer base URL")
	fs.StringVar(&f.StatusURL, "status-url", cfg.StatusURL, "network status page base URL")
	fs.Uint64Var(&f.ChainID, "chain-id", cfg.ChainID, "expected chain ID; the server refuses to start against another chain")
	fs.StringVar(&groups, "groups", groups, "comma-separated tool groups: "+strings.Join(AllGroups, ","))
	fs.BoolVar(&f.AllowBroadcast, "allow-broadcast", false, "register broadcast_signed_tx (needs the tx group); the only tool that writes to the network")
	fs.BoolVar(&f.ExposePeerAddresses, "expose-peer-addresses", false, "show peer IP addresses in node_peers (stdio only)")
	fs.StringVar(&f.HTTPAddr, "http-addr", cfg.HTTPAddr, "listen address for the http transport")
	fs.BoolVar(&f.HTTPStateless, "http-stateless", false, "serve http without sessions (for load-balanced replicas; disables resource subscriptions)")
	fs.DurationVar(&f.HeadPollInterval, "head-poll-interval", cfg.HeadPollInterval, "how often the node is polled for new blocks while clients are subscribed or waiting")
	fs.StringVar(&f.ExplorerStatsURL, "explorer-stats-url", "", "Blockscout stats service base URL (default: <explorer-url>/stats-service)")
	fs.StringVar(&f.HTTPPath, "http-path", cfg.HTTPPath, "URL path of the MCP endpoint")
	fs.Float64Var(&f.RateLimit, "rate-limit", cfg.RateLimit, "sustained requests per second per client on the http transport")
	fs.IntVar(&f.RateBurst, "rate-burst", cfg.RateBurst, "burst size per client on the http transport")
	fs.IntVar(&f.MaxConcurrent, "max-concurrent", cfg.MaxConcurrent, "maximum requests in flight on the http transport")
	fs.StringVar(&proxies, "trusted-proxies", "", "comma-separated proxy addresses or CIDRs allowed to set X-Forwarded-For")
	fs.StringVar(&f.MetricsAddr, "metrics-addr", "", "listen address for Prometheus /metrics (disabled when empty)")
	fs.StringVar(&f.LogLevel, "log-level", cfg.LogLevel, "debug, info, warn or error")
	fs.StringVar(&f.LogFormat, "log-format", cfg.LogFormat, `"json" or "text" (always written to stderr)`)
	fs.DurationVar(&f.BackendTimeout, "backend-timeout", cfg.BackendTimeout, "timeout of a single backend request")
	fs.DurationVar(&f.ToolTimeout, "tool-timeout", cfg.ToolTimeout, "timeout of a whole tool call")
	fs.IntVar(&f.BackendRetries, "backend-retries", cfg.BackendRetries, "retries of failed idempotent backend requests")
	fs.DurationVar(&f.StatusCacheTTL, "status-cache-ttl", cfg.StatusCacheTTL, "how long status page responses are reused")
	fs.IntVar(&f.MaxResponseBytes, "max-response-bytes", cfg.MaxResponseBytes, "largest tool result returned, in bytes")
	fs.Uint64Var(&f.MaxLogRange, "max-log-range", cfg.MaxLogRange, "largest block range get_logs accepts")
	fs.IntVar(&f.MaxLogs, "max-logs", cfg.MaxLogs, "most logs get_logs returns")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	if *configPath != "" {
		if err := loadYAML(*configPath, &cfg); err != nil {
			return Config{}, err
		}
	}
	if err := applyEnv(&cfg, getenv); err != nil {
		return Config{}, err
	}

	// Flags set explicitly on the command line win over everything else.
	set := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	apply := func(name string, fn func()) {
		if set[name] {
			fn()
		}
	}
	apply("transport", func() { cfg.Transport = f.Transport })
	apply("rpc-url", func() { cfg.RPCURL = f.RPCURL })
	apply("explorer-url", func() { cfg.ExplorerURL = f.ExplorerURL })
	apply("status-url", func() { cfg.StatusURL = f.StatusURL })
	apply("chain-id", func() { cfg.ChainID = f.ChainID })
	apply("groups", func() { cfg.Groups = splitList(groups) })
	apply("allow-broadcast", func() { cfg.AllowBroadcast = f.AllowBroadcast })
	apply("expose-peer-addresses", func() { cfg.ExposePeerAddresses = f.ExposePeerAddresses })
	apply("http-addr", func() { cfg.HTTPAddr = f.HTTPAddr })
	apply("http-path", func() { cfg.HTTPPath = f.HTTPPath })
	apply("http-stateless", func() { cfg.HTTPStateless = f.HTTPStateless })
	apply("head-poll-interval", func() { cfg.HeadPollInterval = f.HeadPollInterval })
	apply("explorer-stats-url", func() { cfg.ExplorerStatsURL = f.ExplorerStatsURL })
	apply("rate-limit", func() { cfg.RateLimit = f.RateLimit })
	apply("rate-burst", func() { cfg.RateBurst = f.RateBurst })
	apply("max-concurrent", func() { cfg.MaxConcurrent = f.MaxConcurrent })
	apply("trusted-proxies", func() { cfg.TrustedProxies = splitList(proxies) })
	apply("metrics-addr", func() { cfg.MetricsAddr = f.MetricsAddr })
	apply("log-level", func() { cfg.LogLevel = f.LogLevel })
	apply("log-format", func() { cfg.LogFormat = f.LogFormat })
	apply("backend-timeout", func() { cfg.BackendTimeout = f.BackendTimeout })
	apply("tool-timeout", func() { cfg.ToolTimeout = f.ToolTimeout })
	apply("backend-retries", func() { cfg.BackendRetries = f.BackendRetries })
	apply("status-cache-ttl", func() { cfg.StatusCacheTTL = f.StatusCacheTTL })
	apply("max-response-bytes", func() { cfg.MaxResponseBytes = f.MaxResponseBytes })
	apply("max-log-range", func() { cfg.MaxLogRange = f.MaxLogRange })
	apply("max-logs", func() { cfg.MaxLogs = f.MaxLogs })

	return cfg, cfg.Validate()
}

func loadYAML(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config file: %w", err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("parsing config file %s: %w", path, err)
	}
	return nil
}

// applyEnv reads PARALLAX_MCP_<NAME> for every setting, NAME being the flag
// name upper-cased with dashes as underscores.
func applyEnv(cfg *Config, getenv func(string) string) error {
	var errs []error
	str := func(name string, dst *string) {
		if v := getenv("PARALLAX_MCP_" + name); v != "" {
			*dst = v
		}
	}
	list := func(name string, dst *[]string) {
		if v := getenv("PARALLAX_MCP_" + name); v != "" {
			*dst = splitList(v)
		}
	}
	parse := func(name string, fn func(string) error) {
		if v := getenv("PARALLAX_MCP_" + name); v != "" {
			if err := fn(v); err != nil {
				errs = append(errs, fmt.Errorf("PARALLAX_MCP_%s: %w", name, err))
			}
		}
	}
	boolean := func(name string, dst *bool) {
		parse(name, func(v string) (err error) { *dst, err = strconv.ParseBool(v); return })
	}
	integer := func(name string, dst *int) {
		parse(name, func(v string) (err error) { *dst, err = strconv.Atoi(v); return })
	}
	duration := func(name string, dst *time.Duration) {
		parse(name, func(v string) (err error) { *dst, err = time.ParseDuration(v); return })
	}

	str("TRANSPORT", &cfg.Transport)
	str("RPC_URL", &cfg.RPCURL)
	str("EXPLORER_URL", &cfg.ExplorerURL)
	str("STATUS_URL", &cfg.StatusURL)
	parse("CHAIN_ID", func(v string) (err error) { cfg.ChainID, err = strconv.ParseUint(v, 10, 64); return })
	list("GROUPS", &cfg.Groups)
	boolean("ALLOW_BROADCAST", &cfg.AllowBroadcast)
	boolean("EXPOSE_PEER_ADDRESSES", &cfg.ExposePeerAddresses)
	str("HTTP_ADDR", &cfg.HTTPAddr)
	str("HTTP_PATH", &cfg.HTTPPath)
	boolean("HTTP_STATELESS", &cfg.HTTPStateless)
	duration("HEAD_POLL_INTERVAL", &cfg.HeadPollInterval)
	str("EXPLORER_STATS_URL", &cfg.ExplorerStatsURL)
	parse("RATE_LIMIT", func(v string) (err error) { cfg.RateLimit, err = strconv.ParseFloat(v, 64); return })
	integer("RATE_BURST", &cfg.RateBurst)
	integer("MAX_CONCURRENT", &cfg.MaxConcurrent)
	list("TRUSTED_PROXIES", &cfg.TrustedProxies)
	str("METRICS_ADDR", &cfg.MetricsAddr)
	str("LOG_LEVEL", &cfg.LogLevel)
	str("LOG_FORMAT", &cfg.LogFormat)
	duration("BACKEND_TIMEOUT", &cfg.BackendTimeout)
	duration("TOOL_TIMEOUT", &cfg.ToolTimeout)
	integer("BACKEND_RETRIES", &cfg.BackendRetries)
	duration("STATUS_CACHE_TTL", &cfg.StatusCacheTTL)
	integer("MAX_RESPONSE_BYTES", &cfg.MaxResponseBytes)
	parse("MAX_LOG_RANGE", func(v string) (err error) { cfg.MaxLogRange, err = strconv.ParseUint(v, 10, 64); return })
	integer("MAX_LOGS", &cfg.MaxLogs)
	return errors.Join(errs...)
}

// Validate checks the configuration for values the server cannot run with.
func (c *Config) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.Transport != TransportStdio && c.Transport != TransportHTTP {
		bad("transport must be %q or %q, not %q", TransportStdio, TransportHTTP, c.Transport)
	}
	if c.RPCURL == "" {
		bad("rpc_url is required")
	}
	if c.ChainID == 0 {
		bad("chain_id is required")
	}
	for _, g := range c.Groups {
		if !slices.Contains(AllGroups, g) {
			bad("unknown tool group %q (known: %s)", g, strings.Join(AllGroups, ", "))
		}
	}
	if c.AllowBroadcast && !c.GroupEnabled(GroupTx) {
		bad("allow_broadcast needs the tx group enabled")
	}
	if !strings.HasPrefix(c.HTTPPath, "/") {
		bad("http_path must start with /")
	}
	if c.RateLimit <= 0 || c.RateBurst < 1 {
		bad("rate_limit and rate_burst must be positive")
	}
	if c.MaxConcurrent < 1 {
		bad("max_concurrent must be positive")
	}
	for _, p := range c.TrustedProxies {
		if _, err := ParsePrefix(p); err != nil {
			bad("trusted proxy %q: %v", p, err)
		}
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		bad("log_level must be debug, info, warn or error")
	}
	if c.LogFormat != "json" && c.LogFormat != "text" {
		bad(`log_format must be "json" or "text"`)
	}
	if c.BackendTimeout <= 0 || c.ToolTimeout <= 0 {
		bad("timeouts must be positive")
	}
	if c.BackendRetries < 0 || c.BackendRetries > 10 {
		bad("backend_retries must be between 0 and 10")
	}
	if c.MaxResponseBytes < 4096 {
		bad("max_response_bytes must be at least 4096")
	}
	if c.HeadPollInterval < time.Second {
		bad("head_poll_interval must be at least 1s")
	}
	if c.MaxLogRange == 0 || c.MaxLogs < 1 {
		bad("max_log_range and max_logs must be positive")
	}
	return errors.Join(errs...)
}

// StatsURL is the Blockscout stats service base URL.
func (c *Config) StatsURL() string {
	if c.ExplorerStatsURL != "" {
		return strings.TrimRight(c.ExplorerStatsURL, "/")
	}
	if c.ExplorerURL == "" {
		return ""
	}
	return strings.TrimRight(c.ExplorerURL, "/") + "/stats-service"
}

// GroupEnabled reports whether the named tool group is on.
func (c *Config) GroupEnabled(g string) bool { return slices.Contains(c.Groups, g) }

// BroadcastEnabled reports whether broadcast_signed_tx is registered. The
// flag is the only switch, on either transport: config can never make the
// HTTP transport writable without it.
func (c *Config) BroadcastEnabled() bool {
	return c.AllowBroadcast && c.GroupEnabled(GroupTx)
}

// PeerAddressesExposed reports whether peer IPs may be shown, which is never
// the case on the http transport.
func (c *Config) PeerAddressesExposed() bool {
	return c.ExposePeerAddresses && c.Transport == TransportStdio
}

// ParsePrefix accepts an address or a CIDR range.
func ParsePrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		return netip.ParsePrefix(s)
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
