// Package docs is the docs tool group: search and read the Parallax
// documentation and PIPs embedded in the server, so assistants look up
// Parallax specifics instead of guessing from Ethereum or Bitcoin.
package docs

import (
	"context"
	"fmt"
	"unicode/utf8"

	"github.com/ParallaxProtocol/parallax-mcp/internal/config"
	"github.com/ParallaxProtocol/parallax-mcp/internal/resources"
	"github.com/ParallaxProtocol/parallax-mcp/internal/server"
)

const (
	defaultResults = 6
	maxResults     = 20
	defaultBytes   = 24_000
	maxBytes       = 120_000
)

// Register adds the docs tools.
func Register(r *server.Registrar) {
	server.Add(r, server.Spec{Group: config.GroupDocs, Name: "search_docs", Title: "Search the Parallax docs",
		Description: `Searches the official Parallax documentation (docs.parallaxprotocol.org: running a node, mining, wallets, JSON-RPC, the PVM, networking, Tor, protocol concepts) and the Parallax Improvement Proposals (PIP-0002 ASERT difficulty, PIP-0003 DNS discovery, PIP-0004 fee estimation, PIP-0005 UPnP, PIP-0006 TCP discovery).
Use it whenever a question is about how Parallax works or how to do something with it: Parallax differs from Ethereum and Bitcoin in many details (no EIP-1559, Berlin EVM, XHash, ASERT, its own client flags), so look it up rather than answer from general knowledge. Returns the best matches, each with title, path, a summary and an excerpt around the matching text; read a page in full with read_doc and cite its url.
"query" is keywords or a question, such as "mining pool setup" or "how does the fee estimator work"; "kind" limits results to "docs" or "pip".`,
	}, func(ctx context.Context, in searchIn) (any, error) {
		limit := in.Limit
		if limit == 0 {
			limit = defaultResults
		}
		if limit < 1 || limit > maxResults {
			return nil, server.Inputf("limit must be between 1 and %d", maxResults)
		}
		if in.Kind != "" && in.Kind != "docs" && in.Kind != "pip" {
			return nil, server.Inputf(`kind must be "docs" or "pip"`)
		}
		if len(in.Query) > 300 {
			return nil, server.Inputf("query is too long")
		}
		hits := resources.Search(in.Query, in.Kind, limit)
		type result struct {
			Path        string  `json:"path"`
			Kind        string  `json:"kind"`
			Title       string  `json:"title"`
			Description string  `json:"description,omitempty"`
			URL         string  `json:"url"`
			Excerpt     string  `json:"excerpt"`
			Score       float64 `json:"score"`
		}
		out := make([]result, 0, len(hits))
		for _, h := range hits {
			out = append(out, result{Path: h.Doc.Path, Kind: h.Doc.Kind, Title: h.Doc.Title, Description: h.Doc.Description,
				URL: h.Doc.URL, Excerpt: h.Excerpt, Score: float64(int(h.Score*100)) / 100})
		}
		res := map[string]any{"query": in.Query, "results": out}
		if len(out) == 0 {
			res["note"] = "nothing matched; try other keywords, or list the pages with the parallax://docs resource"
		}
		return res, nil
	})

	server.Add(r, server.Spec{Group: config.GroupDocs, Name: "read_doc", Title: "Read a Parallax doc",
		Description: `Returns a page of the Parallax documentation or a PIP in full, as Markdown: by "path" from search_docs (such as "guides/mining/pool" or "pip-0004"; "PIP-4", a parallax://docs URI or a docs.parallaxprotocol.org URL also work). Long pages come in parts of "max_bytes" (default 24,000): pass "next_offset" back as "offset" for the rest. Includes the page's published url, to cite.`,
	}, func(ctx context.Context, in readIn) (any, error) {
		d := resources.Lookup(in.Path)
		if d == nil {
			return nil, server.Inputf("no page or PIP %q; find paths with search_docs", in.Path)
		}
		size := in.MaxBytes
		if size == 0 {
			size = defaultBytes
		}
		if size < 1000 || size > maxBytes {
			return nil, server.Inputf("max_bytes must be between 1000 and %d", maxBytes)
		}
		text := d.Text
		if in.Offset < 0 || in.Offset > len(text) {
			return nil, server.Inputf("offset is past the end (the page has %d bytes)", len(text))
		}
		end := min(len(text), in.Offset+size)
		for end < len(text) && !utf8.RuneStart(text[end]) {
			end--
		}
		out := map[string]any{
			"path":        d.Path,
			"kind":        d.Kind,
			"title":       d.Title,
			"url":         d.URL,
			"total_bytes": len(text),
			"text":        text[in.Offset:end],
		}
		if d.Description != "" {
			out["description"] = d.Description
		}
		if end < len(text) {
			out["next_offset"] = end
			out["note"] = fmt.Sprintf("part of the page (bytes %d-%d of %d); call again with offset %d for more", in.Offset, end, len(text), end)
		}
		return out, nil
	})
}

type searchIn struct {
	Query string `json:"query" jsonschema:"keywords or a question"`
	Kind  string `json:"kind,omitempty" jsonschema:"\"docs\" or \"pip\" to search only one kind"`
	Limit int    `json:"limit,omitempty" jsonschema:"number of results, 1-20 (default 6)"`
}

type readIn struct {
	Path     string `json:"path" jsonschema:"page path from search_docs, such as guides/mining/pool or pip-0004"`
	Offset   int    `json:"offset,omitempty" jsonschema:"byte offset to start from, for long pages (next_offset of the previous call)"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"how much to return, 1000-120000 bytes (default 24000)"`
}
