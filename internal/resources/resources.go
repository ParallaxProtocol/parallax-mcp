// Package resources serves the MCP resources: chain parameters, a
// glossary, the Parallax documentation and the PIPs. The documents are
// snapshots embedded at build time by scripts/sync-content.sh, so the
// server needs no network access or credentials to serve them.
package resources

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ParallaxProtocol/parallax-mcp/internal/server"
)

//go:embed content glossary.md
var files embed.FS

const (
	uriParams   = "parallax://chain/parameters"
	uriGlossary = "parallax://glossary"
	uriDocs     = "parallax://docs"
	uriPIPs     = "parallax://pips"
)

// Register adds every resource to the server.
func Register(r *server.Registrar) {
	s := r.MCP()
	src := source()

	s.AddResource(&mcp.Resource{
		URI: uriParams, Name: "chain-parameters", Title: "Parallax chain parameters", MIMEType: "application/json",
		Description: "Consensus and network constants of Parallax mainnet: chain ID, block time, PoW and difficulty algorithm, reward schedule and supply cap, coinbase maturity, EVM fork level and supported transaction types, fee model, ports and public endpoints.",
	}, static(uriParams, "application/json", paramsJSON(r)))

	glossary, _ := files.ReadFile("glossary.md")
	s.AddResource(&mcp.Resource{
		URI: uriGlossary, Name: "glossary", Title: "Parallax glossary", MIMEType: "text/markdown",
		Description: "Definitions of Parallax and proof-of-work terms used by this server's tools: LAX and wei, XHash, ASERT, halvings and eras, coinbase maturity, confirmations, reorgs, verified nodes, hashprice, PIPs and more.",
	}, static(uriGlossary, "text/markdown", string(glossary)))

	docs := docIndex()
	s.AddResource(&mcp.Resource{
		URI: uriDocs, Name: "docs-index", Title: "Parallax documentation index", MIMEType: "text/markdown",
		Description: fmt.Sprintf("Index of the %d pages of the Parallax documentation (docs.parallaxprotocol.org, %s, %s at %s): each page's resource URI, title and summary. Read a page with parallax://docs/{path}.", len(docs), src.Docs.Version, src.Docs.Ref, short(src.Docs.Commit)),
	}, static(uriDocs, "text/markdown", renderIndex("Parallax documentation", src.docsNote(), docs)))
	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "parallax://docs/{+path}", Name: "docs-page", Title: "Parallax documentation page", MIMEType: "text/markdown",
		Description: "One page of the Parallax documentation by its path, such as parallax://docs/guides/mining/pool. See parallax://docs for the list.",
	}, page("content/docs", uriDocs+"/", ".mdx"))

	pips := pipIndex()
	s.AddResource(&mcp.Resource{
		URI: uriPIPs, Name: "pips-index", Title: "Parallax Improvement Proposals", MIMEType: "text/markdown",
		Description: "Index of the Parallax Improvement Proposals (PIPs), with each one's title and resource URI.",
	}, static(uriPIPs, "text/markdown", renderIndex("Parallax Improvement Proposals", src.pipsNote(), pips)))
	for _, p := range pips {
		s.AddResource(&mcp.Resource{URI: p.URI, Name: strings.TrimPrefix(p.URI, uriPIPs+"/"), Title: p.Title, MIMEType: "text/markdown", Description: p.Summary},
			page("content/pips", uriPIPs+"/", ".md"))
	}
}

type sourceInfo struct {
	Docs struct {
		Repository, Ref, Commit, Version string
	}
	PIPs struct {
		Discussions, Fetched string
	} `json:"pips"`
}

func source() sourceInfo {
	var s sourceInfo
	if b, err := files.ReadFile("content/SOURCE.json"); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func (s sourceInfo) docsNote() string {
	return fmt.Sprintf("Snapshot of docs/%s in %s at %s (%s), as published at https://docs.parallaxprotocol.org.", s.Docs.Version, s.Docs.Repository, s.Docs.Ref, short(s.Docs.Commit))
}

func (s sourceInfo) pipsNote() string {
	return fmt.Sprintf("PIP-0002 to PIP-0005 are discussions at %s (snapshot of %s); PIP-0006 is from the protocol documentation.", s.PIPs.Discussions, s.PIPs.Fetched)
}

func short(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

func static(uri, mime, text string) mcp.ResourceHandler {
	return func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: mime, Text: text}}}, nil
	}
}

// page serves files under dir for URIs under prefix, rejecting anything
// that is not a clean relative path to an embedded file.
func page(dir, prefix, ext string) mcp.ResourceHandler {
	return func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		uri := req.Params.URI
		rel := strings.TrimSuffix(strings.TrimPrefix(uri, prefix), ext)
		if rel == "" || rel != path.Clean(rel) || strings.HasPrefix(rel, "/") || strings.Contains(rel, "..") {
			return nil, mcp.ResourceNotFoundError(uri)
		}
		b, err := files.ReadFile(dir + "/" + rel + ext)
		if err != nil {
			return nil, mcp.ResourceNotFoundError(uri)
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: "text/markdown", Text: string(b)}}}, nil
	}
}

// Entry is one indexed document.
type Entry struct {
	URI, Title, Summary string
}

func docIndex() []Entry {
	var out []Entry
	_ = fs.WalkDir(files, "content/docs", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".mdx") {
			return nil
		}
		rel := strings.TrimSuffix(strings.TrimPrefix(p, "content/docs/"), ".mdx")
		title, desc := frontMatter(p)
		if title == "" {
			title = rel
		}
		out = append(out, Entry{URI: uriDocs + "/" + rel, Title: title, Summary: desc})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].URI < out[j].URI })
	return out
}

func pipIndex() []Entry {
	entries, _ := files.ReadDir("content/pips")
	var out []Entry
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".md")
		title, desc := frontMatter("content/pips/" + e.Name())
		if title == "" {
			title = strings.ToUpper(name)
		}
		if desc == "" {
			desc = title
		}
		out = append(out, Entry{URI: uriPIPs + "/" + name, Title: title, Summary: desc})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URI < out[j].URI })
	return out
}

// frontMatter reads the title and description of a document's YAML front
// matter (a block between --- lines at the top).
func frontMatter(name string) (title, desc string) {
	f, err := files.Open(name)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return "", ""
	}
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "---" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch strings.TrimSpace(k) {
		case "title":
			title = v
		case "description":
			desc = v
		}
	}
	return title, desc
}

func renderIndex(heading, note string, entries []Entry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n%s\n\n", heading, note)
	section := ""
	for _, e := range entries {
		rest := strings.TrimPrefix(strings.TrimPrefix(e.URI, uriDocs+"/"), uriPIPs+"/")
		if dir := path.Dir(rest); dir != "." && dir != section {
			section = dir
			fmt.Fprintf(&b, "\n## %s\n\n", dir)
		}
		fmt.Fprintf(&b, "- [%s](%s)", e.Title, e.URI)
		if e.Summary != "" && e.Summary != e.Title {
			fmt.Fprintf(&b, ": %s", e.Summary)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// paramsJSON is the chain parameters resource. Values that change over
// time (difficulty, gas limit, supply) point to the tools that read them.
func paramsJSON(r *server.Registrar) string {
	p := map[string]any{
		"network":      "Parallax mainnet",
		"chain_id":     r.Cfg.ChainID,
		"network_id":   2110,
		"genesis_hash": "0x96b239e015d50f5c3f11b80733e5f38c95882c072719507b7be1a59db4975457",
		"currency":     map[string]any{"symbol": "LAX", "decimals": 18, "smallest_unit": "wei", "units": "1 LAX = 10^18 wei; 1 gwei = 10^9 wei"},
		"consensus":    map[string]any{"type": "proof of work", "algorithm": "XHash", "target_block_seconds": 600, "fork_choice": "most cumulative work (total difficulty)"},
		"difficulty_adjustment": map[string]any{
			"algorithm":     "ASERT (absolutely scheduled exponentially rising targets), adjusting every block",
			"active_from":   17561,
			"anchor_height": 17560,
			"proposal":      "PIP-0002 (parallax://pips/pip-0002)",
			"before":        "Bitcoin-style retarget every 2016 blocks",
		},
		"emission": map[string]any{
			"initial_block_reward_lax": 50,
			"halving_interval_blocks":  210000,
			"supply_cap_lax":           21000000,
			"genesis_reward":           0,
			"premine":                  0,
			"coinbase_maturity_blocks": 100,
			"live_figures":             "supply_info and block_reward tools",
		},
		"evm": map[string]any{
			"fork_level":        "Berlin (Homestead through Berlin active from genesis); London and later are not active",
			"base_fee":          "none: no EIP-1559, gasPrice is the whole fee per gas and all of it goes to the miner",
			"transaction_types": []string{"0 legacy (EIP-155 replay protected)", "1 access list (EIP-2930)"},
			"not_supported":     []string{"type 2 (EIP-1559) transactions", "post-Berlin opcodes such as BASEFEE and PUSH0 (invalid opcode): compile Solidity with evmVersion \"berlin\""},
			"block_gas_limit":   "600,000,000 at the time of writing; miners can move it (see get_block)",
		},
		"fees": map[string]any{
			"estimator": "eth_estimateSmartFee(confTarget): Bitcoin Core-style fee estimation (PIP-0004), see the gas_price tool",
		},
		"networking": map[string]any{
			"p2p_port":  32110,
			"discovery": "DNS seeds (PIP-0003) and TCP address gossip (PIP-0006)",
		},
		"endpoints": map[string]any{
			"rpc":      r.Cfg.RPCURL,
			"explorer": r.Cfg.ExplorerURL,
			"status":   r.Cfg.StatusURL,
			"docs":     "https://docs.parallaxprotocol.org",
		},
	}
	b, _ := json.MarshalIndent(p, "", "  ")
	return string(b)
}
