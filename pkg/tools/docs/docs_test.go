package docs_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/ParallaxProtocol/parallax-mcp/internal/testutil"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/server"
	"github.com/ParallaxProtocol/parallax-mcp/pkg/tools/docs"
)

func newEnv(t *testing.T) *testutil.Env {
	return testutil.NewEnv(t, []func(*server.Registrar){docs.Register}, nil)
}

// The embedded snapshot is real documentation, so these check that the
// pages a person would want come first.
func TestSearchRanksTheRightPage(t *testing.T) {
	e := newEnv(t)
	for query, want := range map[string]struct {
		path   string
		within int
	}{
		"mining pool setup": {"hashwarp/pool-mining", 1},
		// The exchange guide's withdrawals page explains using the estimator
		// and ranks alongside the PIP that explains how it works
		"how does the fee estimator work": {"pip-0004", 2},
		"run a node over tor":             {"client/fundamentals/tor", 1},
		"difficulty adjustment ASERT":     {"pip-0002", 1},
		"metamask network settings":       {"guides/wallets/metamask", 1},
		"connect claude mcp":              {"guides/ai/parallax-mcp", 1},
		"sign a message in the wallet":    {"parallax-wallet/messages", 1},
		"import contacts csv":             {"parallax-wallet/contacts", 1},
	} {
		out := e.MustCall(t, "search_docs", map[string]any{"query": query, "limit": 3})
		var got []any
		for i := range want.within {
			got = append(got, testutil.Get(out, "results", i, "path"))
		}
		if !slices.Contains(got, any(want.path)) {
			t.Errorf("%q: top %d results %v, want %s", query, want.within, got, want.path)
		}
	}
}

func TestSearchResultsAreUseful(t *testing.T) {
	e := newEnv(t)
	out := e.MustCall(t, "search_docs", map[string]any{"query": "PUSH0"})
	first := testutil.Get(out, "results", 0)
	if !strings.Contains(testutil.Get(first, "excerpt").(string), "PUSH0") {
		t.Errorf("excerpt should show the match: %v", first)
	}
	if !strings.HasPrefix(testutil.Get(first, "url").(string), "https://docs.parallaxprotocol.org/") || testutil.Get(first, "title") == "" {
		t.Errorf("url/title: %v", first)
	}
	out = e.MustCall(t, "search_docs", map[string]any{"query": "difficulty", "kind": "pip"})
	for _, r := range out["results"].([]any) {
		if testutil.Get(r, "kind") != "pip" {
			t.Fatalf("kind filter leaked %v", r)
		}
	}
	out = e.MustCall(t, "search_docs", map[string]any{"query": "zzqqxx"})
	if len(out["results"].([]any)) != 0 || out["note"] == nil {
		t.Fatalf("no-match: %v", out)
	}
	if _, errText := e.Call(t, "search_docs", map[string]any{"query": "x", "kind": "blog"}); errText == "" {
		t.Fatal("bad kind accepted")
	}
}

func TestReadDocLookupAndPaging(t *testing.T) {
	e := newEnv(t)
	for _, ref := range []string{"pip-0004", "PIP-4", "pip4", "parallax://pips/pip-0004"} {
		if out := e.MustCall(t, "read_doc", map[string]any{"path": ref}); out["path"] != "pip-0004" {
			t.Errorf("%s resolved to %v", ref, out["path"])
		}
	}
	for _, ref := range []string{"hashwarp/pool-mining", "parallax://docs/hashwarp/pool-mining", "https://docs.parallaxprotocol.org/hashwarp/pool-mining",
		// the docs' old layout
		"guides/mining/pool", "https://docs.parallaxprotocol.org/v2.1.x/guides/mining/pool"} {
		if out := e.MustCall(t, "read_doc", map[string]any{"path": ref}); out["title"] != "Mining on a Pool" {
			t.Errorf("%s: %v", ref, out["title"])
		}
	}
	// Parallax Wallet's pages, which moved out of the guides into a tab of their own
	for ref, want := range map[string]string{
		"guides/parallax-wallet/settings":                                   "parallax-wallet/settings",
		"https://docs.parallaxprotocol.org/guides/parallax-wallet/accounts": "parallax-wallet/accounts",
		"guides/ai/parallax-wallet":                                         "parallax-wallet/ai-assistants",
		"https://docs.parallaxprotocol.org/v2.1.x/guides/client/desktop":    "parallax-wallet/introduction",
		"guides/wallets/hardware-wallets":                                   "parallax-wallet/hardware-wallets",
	} {
		if out := e.MustCall(t, "read_doc", map[string]any{"path": ref}); out["path"] != want {
			t.Errorf("%s resolved to %v, want %s", ref, out["path"], want)
		}
	}
	for _, ref := range []string{"client/fundamentals/tor", "parallax-client/fundamentals/tor", "https://docs.parallaxprotocol.org/v2.0.x/parallax-client/fundamentals/tor"} {
		if out := e.MustCall(t, "read_doc", map[string]any{"path": ref}); out["path"] != "client/fundamentals/tor" {
			t.Errorf("%s resolved to %v", ref, out["path"])
		}
	}

	first := e.MustCall(t, "read_doc", map[string]any{"path": "pip-0004", "max_bytes": 1000})
	next, ok := first["next_offset"].(float64)
	if !ok || len(first["text"].(string)) > 1000 {
		t.Fatalf("first part: %v", first["next_offset"])
	}
	second := e.MustCall(t, "read_doc", map[string]any{"path": "pip-0004", "max_bytes": 120000, "offset": int(next)})
	if second["next_offset"] != nil || len(first["text"].(string))+len(second["text"].(string)) != int(first["total_bytes"].(float64)) {
		t.Fatalf("parts do not add up: %v + %v of %v", len(first["text"].(string)), len(second["text"].(string)), first["total_bytes"])
	}
	if strings.Contains(first["text"].(string), "\r") {
		t.Fatal("carriage returns in the text")
	}

	for _, bad := range []string{"../../etc/passwd", "nope", ""} {
		if _, errText := e.Call(t, "read_doc", map[string]any{"path": bad}); errText == "" {
			t.Errorf("%q: want an error", bad)
		}
	}
}
