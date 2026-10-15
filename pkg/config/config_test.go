package config

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaults(t *testing.T) {
	cfg, err := Load(nil, env(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Transport != TransportStdio || cfg.ChainID != 2110 || cfg.GroupEnabled(GroupTx) || cfg.BroadcastEnabled() {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestPrecedenceFlagsOverEnvOverYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	yaml := "rpc_url: http://yaml:8545\nexplorer_url: http://yaml-explorer\nstatus_cache_ttl: 30s\ngroups: [chain, tx]\nmax_logs: 7\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	e := env(map[string]string{
		"PARALLAX_MCP_CONFIG":    path,
		"PARALLAX_MCP_RPC_URL":   "http://env:8545",
		"PARALLAX_MCP_MAX_LOGS":  "9",
		"PARALLAX_MCP_LOG_LEVEL": "debug",
	})
	cfg, err := Load([]string{"-rpc-url", "http://flag:8545"}, e, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RPCURL != "http://flag:8545" {
		t.Errorf("rpc_url: flag should win, got %s", cfg.RPCURL)
	}
	if cfg.MaxLogs != 9 || cfg.LogLevel != "debug" {
		t.Errorf("env should win over yaml: max_logs %d, log_level %s", cfg.MaxLogs, cfg.LogLevel)
	}
	if cfg.ExplorerURL != "http://yaml-explorer" || cfg.StatusCacheTTL != 30*time.Second {
		t.Errorf("yaml values lost: %+v", cfg)
	}
	if !slices.Equal(cfg.Groups, []string{"chain", "tx"}) {
		t.Errorf("groups: %v", cfg.Groups)
	}
}

func TestUnsetFlagsDoNotOverrideYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	_ = os.WriteFile(path, []byte("transport: http\n"), 0o600)
	cfg, err := Load([]string{"-config", path, "-max-logs", "3"}, env(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Transport != TransportHTTP || cfg.MaxLogs != 3 {
		t.Fatalf("got transport %s, max_logs %d", cfg.Transport, cfg.MaxLogs)
	}
}

func TestYAMLUnknownKeyRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	_ = os.WriteFile(path, []byte("rpc_ulr: typo\n"), 0o600)
	if _, err := Load([]string{"-config", path}, env(nil), io.Discard); err == nil {
		t.Fatal("want an error for an unknown key")
	}
}

func TestValidation(t *testing.T) {
	for name, args := range map[string][]string{
		"transport":            {"-transport", "grpc"},
		"group":                {"-groups", "chain,wallet"},
		"broadcast without tx": {"-allow-broadcast"},
		"proxy":                {"-trusted-proxies", "not-an-ip"},
		"rate":                 {"-rate-limit", "0"},
		"path":                 {"-http-path", "mcp"},
		"response cap":         {"-max-response-bytes", "10"},
	} {
		if _, err := Load(args, env(nil), io.Discard); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if _, err := Load([]string{"-groups", "chain,tx", "-allow-broadcast"}, env(nil), io.Discard); err != nil {
		t.Fatalf("broadcast with tx: %v", err)
	}
}

func TestBadEnv(t *testing.T) {
	_, err := Load(nil, env(map[string]string{"PARALLAX_MCP_TOOL_TIMEOUT": "soon"}), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "PARALLAX_MCP_TOOL_TIMEOUT") {
		t.Fatalf("want an error naming the variable, got %v", err)
	}
}

func TestPeerAddressesNeverOnHTTP(t *testing.T) {
	cfg, _ := Load([]string{"-transport", "http", "-expose-peer-addresses"}, env(nil), io.Discard)
	if cfg.PeerAddressesExposed() {
		t.Fatal("peer addresses must not be exposed over http")
	}
	cfg, _ = Load([]string{"-expose-peer-addresses"}, env(nil), io.Discard)
	if !cfg.PeerAddressesExposed() {
		t.Fatal("stdio should honour the flag")
	}
}
