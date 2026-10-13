// Package network is the network tool group: health, nodes, incidents and
// block propagation from the status page, combined with what the configured
// node sees of the network.
package network

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ParallaxProtocol/parallax/v2/util/hexutil"

	"github.com/ParallaxProtocol/parallax-mcp/internal/backend/status"
	"github.com/ParallaxProtocol/parallax-mcp/internal/config"
	"github.com/ParallaxProtocol/parallax-mcp/internal/evm"
	"github.com/ParallaxProtocol/parallax-mcp/internal/server"
	"github.com/ParallaxProtocol/parallax-mcp/internal/untrusted"
)

const group = config.GroupNetwork

const coverageNote = ` The status page covers the nodes that report to it (run with --prlstats), not every node on the network; unverified nodes' rows are self-reported.`

// Register adds the network tools.
func Register(r *server.Registrar) {
	t := &tools{r: r}
	server.Add(r, server.Spec{Group: group, Name: "network_health", Title: "Network health",
		Description: `One-call health report of the Parallax network, combining the status page (status.parallaxprotocol.org) with the configured node.
From the status page: the best block and seconds since it was found, reporting nodes (total, active, synced), active alerts with level (critical, warn, info) and details (types: split = verified nodes on different chains; stall = no block for 2 hours; reorg = a reorganization 3+ blocks deep; majority = one miner found most of the last 7 days' blocks; propagation / slow-miner = blocks slow to reach nodes; release = nodes behind the latest client release), ongoing incidents, mining decentralization (grade healthy/fair/concentrated/critical, the fewest miners holding a majority, top miner), the recommended confirmations to wait (blocks for 99/99.9/99.99% confidence against the largest miner, null when a majority makes no number safe), mempool pressure (pending transactions as a share of a block), stale blocks and reorgs, propagation medians, client versions and verified nodes' uptime.
From the node: its head, seconds since that block, sync status and peer count, and how far it is from the status page's best block; with the admin namespace also its uptime and address book.
"issues" lists what needs attention in one place. If either source is unreachable the other's data is still returned with the failure in "errors".` + coverageNote,
	}, t.health)

	server.Add(r, server.Spec{Group: group, Name: "network_nodes", Title: "Reporting nodes",
		Description: `The nodes reporting to the status page: name (operator-chosen for verified nodes, a pseudonym such as node-3f9a2c1b otherwise; untrusted), verified, client version, online/offline (and since when), syncing, mining and hashrate, peers, peer latency (ms), best block, propagation delay of the last block and on average (ms), pending transactions, uptime (%) and location (country and city; IP addresses are never published). Offline nodes are left out unless include_offline=true; verified_only=true keeps only operator-verified nodes.` + coverageNote,
	}, t.nodes)

	server.Add(r, server.Spec{Group: group, Name: "network_incidents", Title: "Incidents and uptime",
		Description: `The status page's incident record: periods when an alert condition held (majority, spread = slow propagation, split, stall, reorg), each with level, start and end (UTC; "ongoing" when it has not ended), duration in seconds and a description (untrusted). Covers the last "days" days (default 30, at most 90), newest first, plus verified nodes' average uptime and when recording began.`,
	}, t.incidents)

	server.Add(r, server.Spec{Group: group, Name: "network_propagation", Title: "Propagation and chain stability",
		Description: `How fast blocks spread and how stable the chain tip is, from the status page: propagation reports with median, p90 and p99 delay (ms) after the first node saw a block, the median time for recent blocks to reach 90% of verified nodes and any consistently slow miner, delays by band; stale blocks in the last 2,000 heights, reorganization count and the last and last deep (3+ blocks) reorg with height, depth and time, and any chain split.`,
	}, t.propagation)

	server.Add(r, server.Spec{Group: group, Name: "node_peers", Title: "Node peers", Requires: "admin",
		Description: `The configured node's own view of the peer-to-peer network (needs a node serving the admin namespace): its uptime, peer count, and each peer's client (untrusted), direction (inbound/outbound), trusted/static flags, protocols, its head block hash, and minimum ping in ms; plus the address book (addrman) size by source and the number of banned addresses. Peer IP addresses and node IDs are omitted unless the server runs on stdio with --expose-peer-addresses.`,
	}, t.peers)
}

type tools struct{ r *server.Registrar }

func (t *tools) snapshot(ctx context.Context) (status.Snapshot, error) {
	if t.r.Status == nil {
		return status.Snapshot{}, fmt.Errorf("no status page is configured (--status-url)")
	}
	return t.r.Status.Stats(ctx)
}

// ---- network_health ----

type noIn struct{}

type nodeView struct {
	Head            uint64  `json:"head_block"`
	SecondsSince    int64   `json:"seconds_since_head_block"`
	Syncing         bool    `json:"syncing"`
	Peers           uint64  `json:"peers"`
	BehindBestBlock *int64  `json:"blocks_behind_status_best,omitempty"`
	UptimeSeconds   *uint64 `json:"uptime_seconds,omitempty"`
	AddrBook        any     `json:"address_book,omitempty"`
}

type healthOut struct {
	server.Partial
	Issues     []string       `json:"issues"`
	StatusPage map[string]any `json:"status_page,omitempty"`
	Node       *nodeView      `json:"node,omitempty"`
}

func (t *tools) health(ctx context.Context, _ noIn) (any, error) {
	var (
		snap    status.Snapshot
		snapErr error
		nv      *nodeView
		nodeErr error
		wg      sync.WaitGroup
	)
	wg.Add(2)
	go func() { defer wg.Done(); snap, snapErr = t.snapshot(ctx) }()
	go func() { defer wg.Done(); nv, nodeErr = t.nodeView(ctx) }()
	wg.Wait()

	out := healthOut{Issues: []string{}}
	now := t.r.Now()
	if nodeErr != nil {
		out.Fail("node", nodeErr)
		out.Issues = append(out.Issues, "the configured node is unreachable: "+server.ErrorText(nodeErr))
	} else {
		out.Node = nv
		if nv.Syncing {
			out.Issues = append(out.Issues, "the configured node is still syncing")
		}
		if nv.Peers == 0 {
			out.Issues = append(out.Issues, "the configured node has no peers")
		}
	}
	if snapErr != nil {
		out.Fail("status_page", snapErr)
		out.Issues = append(out.Issues, "the status page is unreachable; network-wide figures are missing")
	} else {
		if snap.Stale {
			out.Fail("status_page", fmt.Errorf("serving a copy from %s: %v", snap.FetchedAt.UTC().Format(time.RFC3339), snap.Error))
		}
		s := snap.Stats
		out.StatusPage = summarizeStatus(s, now)
		if out.Node != nil && s.BestBlock != nil {
			behind := int64(s.BestBlock.Number) - int64(out.Node.Head)
			out.Node.BehindBestBlock = &behind
			if behind > 2 {
				out.Issues = append(out.Issues, fmt.Sprintf("the configured node is %d blocks behind the status page's best block", behind))
			}
		}
		for _, a := range s.Alerts {
			out.Issues = append(out.Issues, alertText(a))
		}
		if s.Nodes.Active > 0 && s.Nodes.Synced < s.Nodes.Active {
			out.Issues = append(out.Issues, fmt.Sprintf("%d of %d active reporting nodes are not synced", s.Nodes.Active-s.Nodes.Synced, s.Nodes.Active))
		}
	}
	since := int64(-1)
	if out.Node != nil {
		since = out.Node.SecondsSince
	}
	if since > 2*3600 {
		out.Issues = append(out.Issues, fmt.Sprintf("no block for %d minutes (a 2 hour gap is a 1 in 160,000 event at steady hashrate)", since/60))
	}
	return out, nil
}

func (t *tools) nodeView(ctx context.Context) (*nodeView, error) {
	var (
		head    evm.RPCHeader
		syncing json.RawMessage
		peers   hexutil.Big
	)
	batch := []server.BatchElem{
		{Method: "eth_getHeaderByNumber", Args: []any{"latest"}, Result: &head},
		{Method: "eth_syncing", Result: &syncing},
		{Method: "net_peerCount", Result: &peers},
	}
	if err := t.r.Node.Batch(ctx, batch); err != nil {
		return nil, err
	}
	for _, b := range batch {
		if b.Error != nil {
			return nil, b.Error
		}
	}
	v := &nodeView{
		Head:         uint64(head.Number),
		SecondsSince: t.r.Now().Unix() - int64(head.Timestamp),
		Syncing:      !bytes.Equal(bytes.TrimSpace(syncing), []byte("false")),
		Peers:        (*big.Int)(&peers).Uint64(),
	}
	if t.r.HasModule("admin") {
		var up uint64
		var book json.RawMessage
		ab := []server.BatchElem{
			{Method: "admin_uptime", Result: &up},
			{Method: "admin_addrbookStatus", Result: &book},
		}
		if err := t.r.Node.Batch(ctx, ab); err == nil {
			if ab[0].Error == nil {
				v.UptimeSeconds = &up
			}
			if ab[1].Error == nil {
				v.AddrBook = book
			}
		}
	}
	return v, nil
}

func summarizeStatus(s *status.Stats, now time.Time) map[string]any {
	out := map[string]any{"nodes": map[string]any{"total": s.Nodes.Total, "active": s.Nodes.Active, "synced": s.Nodes.Synced, "median_peer_latency_ms": s.Nodes.LatencyMs}}
	if s.Network != nil {
		out["network"] = *s.Network
	}
	if b := s.BestBlock; b != nil {
		bb := map[string]any{"number": b.Number, "hash": b.Hash, "miner": b.Miner, "timestamp": b.Timestamp / 1000, "seconds_since_found": now.Unix() - b.Timestamp/1000}
		if b.Pool != nil {
			bb["pool"] = untrusted.New(*b.Pool)
		}
		out["best_block"] = bb
	}
	if w := s.BlockTime.Week; w != nil {
		out["block_time_7d_mean_seconds"] = round(w.AverageSeconds, 1)
	}
	if s.Hashrate.HashesPerSecond != nil {
		out["hashrate_hashes_per_second"] = math.Round(*s.Hashrate.HashesPerSecond)
	}
	alerts := make([]map[string]any, 0, len(s.Alerts))
	for _, a := range s.Alerts {
		m := map[string]any{"type": a.Type, "level": a.Level, "details": wrapJSON(a.Details)}
		if a.Since != nil {
			m["since"] = *a.Since / 1000
		}
		alerts = append(alerts, m)
	}
	out["alerts"] = alerts
	if inc := s.Incidents; inc != nil {
		var ongoing []map[string]any
		for _, i := range inc.List {
			if i.End == nil {
				ongoing = append(ongoing, incident(i, now))
			}
		}
		out["ongoing_incidents"] = ongoing
	}
	if d := s.Decentralization; d != nil {
		dec := map[string]any{"grade": d.Grade, "majority_holders": d.MajorityHolders, "distinct_miners": d.DistinctMiners, "blocks": d.Blocks}
		if len(d.TopMiners) > 0 {
			dec["top_miner"] = miner(d.TopMiners[0])
		}
		out["decentralization_7d"] = dec
	}
	if c := s.Confirmations; c != nil {
		conf := map[string]any{"judged": c.Judged, "majority_miner_exists": c.Majority}
		if c.LargestMiner != nil {
			conf["largest_miner"] = miner(*c.LargestMiner)
		}
		var lv []map[string]any
		for _, l := range c.Levels {
			lv = append(lv, map[string]any{"confidence": l.Confidence, "blocks": l.Blocks, "wait_seconds": l.WaitSeconds})
		}
		conf["levels"] = lv
		if c.Majority {
			conf["note"] = "one miner holds a majority of recent blocks, so no number of confirmations makes a payment safe against it"
		}
		out["recommended_confirmations"] = conf
	}
	if m := s.Mempool; m != nil {
		out["mempool"] = map[string]any{"pending_median": m.Pending, "pressure_blocks": m.Pressure, "blocks_to_clear": m.BlocksToClear}
	}
	if sb := s.StaleBlocks; sb != nil {
		out["stale_blocks"] = map[string]any{"window": sb.Window, "count": sb.Count, "reorgs": sb.Reorgs.Count, "last_deep_reorg": reorg(sb.LastDeepReorg), "split": wrapJSON(sb.Split)}
	}
	if p := s.Propagation; p != nil {
		prop := map[string]any{"median_ms": p.MedianMs, "p90_ms": p.P90Ms}
		if p.Reach90 != nil {
			prop["reach_90_percent_median_ms"] = p.Reach90.Median
		}
		out["propagation"] = prop
	}
	if cv := s.ClientVersions; cv != nil {
		v := map[string]any{"online": cv.Online, "up_to_date": cv.UpToDate, "behind": cv.Behind}
		if cv.Latest != nil {
			v["latest_release"] = cv.Latest.Version
		}
		out["client_versions"] = v
	}
	if s.Uptime.VerifiedAveragePercent != nil {
		out["verified_uptime_percent"] = *s.Uptime.VerifiedAveragePercent
	}
	if len(s.SecurityBudget) > 0 {
		out["security_budget"] = json.RawMessage(s.SecurityBudget)
	}
	return out
}

func alertText(a status.Alert) string {
	var d map[string]any
	_ = json.Unmarshal(a.Details, &d)
	num := func(k string) float64 { f, _ := d[k].(float64); return f }
	switch a.Type {
	case "majority":
		return fmt.Sprintf("%s: one miner found %.0f%% of the last 7 days' blocks (%.0f of %.0f) and could rewrite recent history", a.Level, num("share")*100, num("blocks"), num("of"))
	case "stall":
		return fmt.Sprintf("%s: no new block for %.0f minutes", a.Level, num("waitingSeconds")/60)
	case "split":
		return fmt.Sprintf("%s: verified nodes are on different chains", a.Level)
	case "reorg":
		return fmt.Sprintf("%s: a %.0f block deep reorganization at height %.0f", a.Level, num("depth"), num("height"))
	case "propagation":
		return fmt.Sprintf("%s: blocks take a median %.0f ms to reach 90%% of verified nodes", a.Level, num("medianMs"))
	case "slow-miner":
		return fmt.Sprintf("%s: one miner's blocks propagate slowly (median %.0f ms)", a.Level, num("medianMs"))
	case "release":
		return fmt.Sprintf("%s: %.0f of %.0f online nodes run a client older than the latest release", a.Level, num("behind"), num("online"))
	}
	return a.Level + ": " + untrusted.Sanitize(a.Type)
}

func miner(m status.Miner) map[string]any {
	out := map[string]any{"address": m.Address, "share": round(m.Share, 4)}
	if m.Blocks > 0 {
		out["blocks"] = m.Blocks
	}
	if m.Name != nil {
		out["name"] = untrusted.New(*m.Name)
	}
	return out
}

func reorg(r *status.Reorg) any {
	if r == nil {
		return nil
	}
	return map[string]any{"height": r.Height, "depth": r.Depth, "nodes": r.Nodes, "at": r.At / 1000}
}

func incident(i status.Incident, now time.Time) map[string]any {
	m := map[string]any{"kind": i.Kind, "level": i.Level, "start": time.UnixMilli(i.Start).UTC().Format(time.RFC3339), "detail": wrapJSON(i.Detail)}
	if !i.StartKnown {
		m["start_is_lower_bound"] = true
	}
	end := now
	if i.End != nil {
		end = time.UnixMilli(*i.End)
		m["end"] = end.UTC().Format(time.RFC3339)
	} else {
		m["end"] = "ongoing"
	}
	m["duration_seconds"] = int64(end.Sub(time.UnixMilli(i.Start)).Seconds())
	return m
}

var hexRe = regexp.MustCompile(`^0x[0-9a-fA-F]*$`)

// wrapJSON decodes status page JSON, wrapping every string that is not a
// hex value as untrusted: pool names and descriptions come from node
// operators' reports and configuration the server does not control.
func wrapJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	var walk func(any) any
	walk = func(v any) any {
		switch v := v.(type) {
		case string:
			if hexRe.MatchString(v) {
				return v
			}
			return untrusted.New(v)
		case []any:
			for i := range v {
				v[i] = walk(v[i])
			}
		case map[string]any:
			for k := range v {
				v[k] = walk(v[k])
			}
		}
		return v
	}
	return walk(v)
}

// ---- network_nodes ----

type nodesIn struct {
	IncludeOffline bool `json:"include_offline,omitempty" jsonschema:"also list nodes that are offline"`
	VerifiedOnly   bool `json:"verified_only,omitempty" jsonschema:"list only operator-verified nodes"`
}

func (t *tools) nodes(ctx context.Context, in nodesIn) (any, error) {
	snap, err := t.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	type row struct {
		Name         untrusted.Text  `json:"name"`
		Verified     bool            `json:"verified"`
		Client       untrusted.Text  `json:"client"`
		Online       bool            `json:"online"`
		OfflineSince string          `json:"offline_since,omitempty"`
		Syncing      bool            `json:"syncing"`
		Mining       bool            `json:"mining"`
		Hashrate     float64         `json:"hashes_per_second,omitempty"`
		Peers        int             `json:"peers"`
		LatencyMs    *float64        `json:"peer_latency_ms,omitempty"`
		Block        uint64          `json:"block"`
		PropMs       *float64        `json:"propagation_ms,omitempty"`
		PropAvgMs    *float64        `json:"propagation_avg_ms,omitempty"`
		Pending      int             `json:"pending_transactions"`
		Uptime       *float64        `json:"uptime_percent,omitempty"`
		Location     *untrusted.Text `json:"location,omitempty"`
	}
	var rows []row
	offline := 0
	for _, n := range snap.NodeList {
		if !n.Active {
			offline++
		}
		if (!n.Active && !in.IncludeOffline) || (in.VerifiedOnly && !n.Verified) {
			continue
		}
		r := row{Name: untrusted.NewN(n.Name, 64), Verified: n.Verified, Client: untrusted.New(n.Client), Online: n.Active, Syncing: n.Syncing,
			Mining: n.Mining, Hashrate: n.HashesPerSecond, Peers: n.Peers, LatencyMs: n.PeerLatencyMs, Block: n.Block,
			PropMs: n.PropagationMs, PropAvgMs: n.PropagationAvgMs, Pending: n.Pending, Uptime: n.UptimePercent}
		if n.OfflineSince != nil {
			r.OfflineSince = time.UnixMilli(*n.OfflineSince).UTC().Format(time.RFC3339)
		}
		var loc []string
		if n.City != nil {
			loc = append(loc, *n.City)
		}
		if n.Country != nil {
			loc = append(loc, *n.Country)
		}
		if len(loc) > 0 {
			l := untrusted.New(strings.Join(loc, ", "))
			r.Location = &l
		}
		rows = append(rows, r)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Verified != rows[j].Verified {
			return rows[i].Verified
		}
		return rows[i].Block > rows[j].Block
	})
	out := map[string]any{"total": len(snap.NodeList), "offline": offline, "listed": len(rows), "nodes": rows}
	if snap.Stale {
		out["errors"] = map[string]string{"status_page": "serving an older copy: " + server.ErrorText(snap.Error)}
	}
	return out, nil
}

// ---- network_incidents ----

type incidentsIn struct {
	Days int `json:"days,omitempty" jsonschema:"how many days back (default 30, at most 90)"`
}

func (t *tools) incidents(ctx context.Context, in incidentsIn) (any, error) {
	days := in.Days
	if days == 0 {
		days = 30
	}
	if days < 1 || days > 90 {
		return nil, server.Inputf("days must be between 1 and 90")
	}
	snap, err := t.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	now := t.r.Now()
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
	out := map[string]any{"days": days}
	var list []map[string]any
	if inc := snap.Incidents; inc != nil {
		out["recording_since"] = time.UnixMilli(inc.Since).UTC().Format(time.RFC3339)
		for _, i := range inc.List {
			if i.End != nil && *i.End < cutoff {
				continue
			}
			list = append(list, incident(i, now))
		}
	}
	out["incidents"] = list
	out["count"] = len(list)
	if snap.Uptime.VerifiedAveragePercent != nil {
		out["verified_uptime_percent"] = *snap.Uptime.VerifiedAveragePercent
	}
	return out, nil
}

// ---- network_propagation ----

func (t *tools) propagation(ctx context.Context, _ noIn) (any, error) {
	snap, err := t.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if p := snap.Propagation; p != nil {
		prop := map[string]any{"reports": p.Reports, "median_ms": p.MedianMs, "p90_ms": p.P90Ms, "p99_ms": p.P99Ms}
		if p.Reach90 != nil {
			prop["reach_90_percent"] = map[string]any{"median_ms": p.Reach90.Median, "blocks": p.Reach90.Blocks, "slow_miner": wrapJSON(p.Reach90.SlowMiner)}
		}
		var bands []map[string]any
		for _, b := range p.Bands {
			bands = append(bands, map[string]any{"from_ms": b.FromMs, "to_ms": b.ToMs, "reports": b.Reports})
		}
		prop["bands"] = bands
		out["propagation"] = prop
	}
	if sb := snap.StaleBlocks; sb != nil {
		out["stale_blocks"] = map[string]any{
			"window_heights":  sb.Window,
			"count":           sb.Count,
			"recent_heights":  sb.Recent,
			"reorgs":          sb.Reorgs.Count,
			"reorgs_since":    time.UnixMilli(sb.Reorgs.Since).UTC().Format(time.RFC3339),
			"last_reorg":      reorg(sb.LastReorg),
			"last_deep_reorg": reorg(sb.LastDeepReorg),
			"split":           wrapJSON(sb.Split),
			"warning_level":   sb.Level,
		}
	}
	return out, nil
}

// ---- node_peers ----

type peerInfo struct {
	ID      string   `json:"id"`
	Enode   string   `json:"enode"`
	Name    string   `json:"name"`
	Caps    []string `json:"caps"`
	Network struct {
		RemoteAddress string `json:"remoteAddress"`
		Inbound       bool   `json:"inbound"`
		Trusted       bool   `json:"trusted"`
		Static        bool   `json:"static"`
	} `json:"network"`
	MinPing   *float64                   `json:"minPing"`
	Protocols map[string]json.RawMessage `json:"protocols"`
}

func (t *tools) peers(ctx context.Context, _ noIn) (any, error) {
	var (
		peers  []peerInfo
		uptime uint64
		book   json.RawMessage
		banned []json.RawMessage
	)
	batch := []server.BatchElem{
		{Method: "admin_peers", Result: &peers},
		{Method: "admin_uptime", Result: &uptime},
		{Method: "admin_addrbookStatus", Result: &book},
		{Method: "admin_listbanned", Result: &banned},
	}
	if err := t.r.Node.Batch(ctx, batch); err != nil {
		return nil, err
	}
	if batch[0].Error != nil {
		return nil, batch[0].Error
	}
	var p server.Partial
	expose := t.r.Cfg.PeerAddressesExposed()
	type peerOut struct {
		Client    untrusted.Text `json:"client"`
		Direction string         `json:"direction"`
		Trusted   bool           `json:"trusted,omitempty"`
		Static    bool           `json:"static,omitempty"`
		Protocols []string       `json:"protocols"`
		Head      string         `json:"head,omitempty"`
		PingMs    *float64       `json:"min_ping_ms,omitempty"`
		Address   string         `json:"address,omitempty"`
		ID        string         `json:"node_id,omitempty"`
	}
	list := make([]peerOut, 0, len(peers))
	inbound := 0
	for _, pi := range peers {
		o := peerOut{Client: untrusted.New(pi.Name), Direction: "outbound", Trusted: pi.Network.Trusted, Static: pi.Network.Static, Protocols: pi.Caps}
		if pi.Network.Inbound {
			o.Direction = "inbound"
			inbound++
		}
		if pi.MinPing != nil {
			ms := round(*pi.MinPing*1000, 1)
			o.PingMs = &ms
		}
		var proto struct {
			Head string `json:"head"`
		}
		if raw, ok := pi.Protocols["parallax"]; ok && json.Unmarshal(raw, &proto) == nil {
			o.Head = proto.Head
		}
		if expose {
			o.Address, o.ID = pi.Network.RemoteAddress, pi.ID
		}
		list = append(list, o)
	}
	out := map[string]any{"peer_count": len(peers), "inbound": inbound, "outbound": len(peers) - inbound, "peers": list}
	if !expose {
		out["addresses"] = "omitted: peer IP addresses are only shown on stdio with --expose-peer-addresses"
	}
	if batch[1].Error == nil {
		out["node_uptime_seconds"] = uptime
	} else {
		p.Fail("admin_uptime", batch[1].Error)
	}
	if batch[2].Error == nil {
		out["address_book"] = book
	} else {
		p.Fail("admin_addrbookStatus", batch[2].Error)
	}
	if batch[3].Error == nil {
		out["banned_count"] = len(banned)
	} else {
		p.Fail("admin_listbanned", batch[3].Error)
	}
	if p.Errors != nil {
		out["errors"] = p.Errors
	}
	return out, nil
}

func round(f float64, places int) float64 {
	pw := math.Pow(10, float64(places))
	return math.Round(f*pw) / pw
}
