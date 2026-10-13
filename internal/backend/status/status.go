// Package status is the network status page backend
// (github.com/ParallaxProtocol/parallax-stats): GET /api/v2/stats, the
// dashboard's whole state as JSON.
//
// The endpoint allows 60 requests per minute per address. Every MCP client
// of a remote deployment shares this server's address, so responses are
// cached for a short TTL and concurrent misses share one request. A stale
// copy is served, flagged as such, when a refresh fails.
package status

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/ParallaxProtocol/parallax-mcp/internal/httpx"
	"github.com/ParallaxProtocol/parallax-mcp/internal/metrics"
)

// maxStale is how old a cached copy may be and still be served when the
// status page cannot be reached.
const maxStale = 10 * time.Minute

// Client fetches and caches the status page.
type Client struct {
	http *httpx.Client
	ttl  time.Duration
	now  func() time.Time

	group   singleflight.Group
	mu      sync.Mutex
	cached  *Stats
	fetched time.Time
}

// New returns a Client.
func New(opts httpx.Options, ttl time.Duration) *Client {
	opts.Name = "status"
	return &Client{http: httpx.New(opts), ttl: ttl, now: time.Now}
}

// Snapshot is a Stats with how fresh it is.
type Snapshot struct {
	*Stats
	// FetchedAt is when this server fetched it.
	FetchedAt time.Time
	// Stale is set when a refresh failed and an older copy is served; Error
	// says why.
	Stale bool
	Error error
}

// Stats returns the status page's figures, from cache when fresh.
func (c *Client) Stats(ctx context.Context) (Snapshot, error) {
	c.mu.Lock()
	cached, fetched := c.cached, c.fetched
	c.mu.Unlock()
	if cached != nil && c.now().Sub(fetched) < c.ttl {
		metrics.Backend("status", "cache_hit")
		return Snapshot{Stats: cached, FetchedAt: fetched}, nil
	}

	// The shared fetch must not die with the first caller's context.
	ch := c.group.DoChan("stats", func() (any, error) {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		var s Stats
		if err := c.http.GetJSON(fctx, "/api/v2/stats", nil, &s); err != nil {
			return nil, err
		}
		now := c.now()
		c.mu.Lock()
		c.cached, c.fetched = &s, now
		c.mu.Unlock()
		return Snapshot{Stats: &s, FetchedAt: now}, nil
	})

	select {
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	case res := <-ch:
		if res.Err == nil {
			return res.Val.(Snapshot), nil
		}
		if cached != nil && c.now().Sub(fetched) < maxStale {
			return Snapshot{Stats: cached, FetchedAt: fetched, Stale: true, Error: res.Err}, nil
		}
		return Snapshot{}, res.Err
	}
}

// Stats mirrors the fields of /api/v2/stats this server uses. Times are
// Unix milliseconds; difficulties are decimal strings.
type Stats struct {
	ServerTime int64   `json:"serverTime"`
	Network    *string `json:"network"`
	BestBlock  *struct {
		Number    uint64  `json:"number"`
		Hash      string  `json:"hash"`
		Miner     string  `json:"miner"`
		Pool      *string `json:"pool"`
		Timestamp int64   `json:"timestamp"`
		Arrived   int64   `json:"arrived"`
	} `json:"bestBlock"`
	Nodes struct {
		Total     int  `json:"total"`
		Active    int  `json:"active"`
		Synced    int  `json:"synced"`
		LatencyMs *int `json:"latencyMs"`
	} `json:"nodes"`
	BlockTime struct {
		TargetSeconds int `json:"targetSeconds"`
		Week          *struct {
			AverageSeconds float64 `json:"averageSeconds"`
			Blocks         int     `json:"blocks"`
			SpanSeconds    int64   `json:"spanSeconds"`
			FullWeek       bool    `json:"fullWeek"`
		} `json:"week"`
		Recent *struct {
			AverageSeconds float64 `json:"averageSeconds"`
			Blocks         int     `json:"blocks"`
		} `json:"recent"`
	} `json:"blockTime"`
	Hashrate struct {
		HashesPerSecond *float64 `json:"hashesPerSecond"`
	} `json:"hashrate"`
	Difficulty struct {
		Latest        *string  `json:"latest"`
		Next          *string  `json:"next"`
		NextPoolUnits *float64 `json:"nextPoolUnits"`
		ChangeWeek    *float64 `json:"changeWeek"`
	} `json:"difficulty"`
	RecommendedFee *struct {
		Wei         string  `json:"wei"`
		Gwei        float64 `json:"gwei"`
		TransferLax float64 `json:"transferLax"`
	} `json:"recommendedFee"`
	Hashprice      json.RawMessage `json:"hashprice"`
	SecurityBudget json.RawMessage `json:"securityBudget"`
	Mempool        *struct {
		Pending       *float64 `json:"pending"`
		Pressure      *float64 `json:"pressure"`
		BlocksToClear *float64 `json:"blocksToClear"`
	} `json:"mempool"`
	Confirmations *struct {
		LargestMiner *Miner `json:"largestMiner"`
		Judged       bool   `json:"judged"`
		Majority     bool   `json:"majority"`
		Levels       []struct {
			Confidence  float64  `json:"confidence"`
			Blocks      *int     `json:"blocks"`
			WaitSeconds *float64 `json:"waitSeconds"`
		} `json:"levels"`
	} `json:"confirmations"`
	Supply json.RawMessage `json:"supply"`
	Uptime struct {
		VerifiedAveragePercent *float64 `json:"verifiedAveragePercent"`
	} `json:"uptime"`
	Decentralization *struct {
		Blocks          int     `json:"blocks"`
		SpanSeconds     int64   `json:"spanSeconds"`
		FullWeek        bool    `json:"fullWeek"`
		Grade           string  `json:"grade"`
		MajorityHolders *int    `json:"majorityHolders"`
		DistinctMiners  int     `json:"distinctMiners"`
		TopMiners       []Miner `json:"topMiners"`
	} `json:"decentralization"`
	StaleBlocks *struct {
		Window int      `json:"window"`
		Count  int      `json:"count"`
		Recent []uint64 `json:"recent"`
		Reorgs struct {
			Count int   `json:"count"`
			Since int64 `json:"since"`
		} `json:"reorgs"`
		LastReorg     *Reorg          `json:"lastReorg"`
		LastDeepReorg *Reorg          `json:"lastDeepReorg"`
		Split         json.RawMessage `json:"split"`
		Level         *string         `json:"level"`
	} `json:"staleBlocks"`
	Propagation *struct {
		Reports  int      `json:"reports"`
		MedianMs *float64 `json:"medianMs"`
		P90Ms    *float64 `json:"p90Ms"`
		P99Ms    *float64 `json:"p99Ms"`
		Reach90  *struct {
			Median    *float64        `json:"median"`
			Blocks    int             `json:"blocks"`
			SlowMiner json.RawMessage `json:"slowMiner"`
		} `json:"reach90"`
		Bands []struct {
			FromMs  float64  `json:"fromMs"`
			ToMs    *float64 `json:"toMs"`
			Reports int      `json:"reports"`
		} `json:"bands"`
	} `json:"propagation"`
	ClientVersions *struct {
		Online int `json:"online"`
		Latest *struct {
			Version string `json:"version"`
			URL     string `json:"url"`
		} `json:"latest"`
		UpToDate *int `json:"upToDate"`
		Behind   *int `json:"behind"`
		Versions []struct {
			Version string `json:"version"`
			Nodes   int    `json:"nodes"`
		} `json:"versions"`
		Platforms []struct {
			Platform string `json:"platform"`
			Nodes    int    `json:"nodes"`
		} `json:"platforms"`
	} `json:"clientVersions"`
	NodeList  []NodeRow `json:"nodeList"`
	Alerts    []Alert   `json:"alerts"`
	Incidents *struct {
		Since       int64      `json:"since"`
		RebuiltFrom *int64     `json:"rebuiltFrom"`
		List        []Incident `json:"list"`
	} `json:"incidents"`
}

// Miner is a block producer; pools count as one.
type Miner struct {
	Name    *string `json:"name"`
	Website *string `json:"website"`
	Address string  `json:"address"`
	Blocks  int     `json:"blocks"`
	Share   float64 `json:"share"`
}

// Reorg is a chain reorganization a node reported.
type Reorg struct {
	Height uint64 `json:"height"`
	At     int64  `json:"at"`
	Nodes  int    `json:"nodes"`
	Depth  int    `json:"depth"`
}

// NodeRow is one row of the node table. Unverified rows are self-reported.
type NodeRow struct {
	Name             string   `json:"name"`
	Verified         bool     `json:"verified"`
	Client           string   `json:"client"`
	Version          string   `json:"version"`
	Active           bool     `json:"active"`
	OfflineSince     *int64   `json:"offlineSince"`
	Syncing          bool     `json:"syncing"`
	Mining           bool     `json:"mining"`
	HashesPerSecond  float64  `json:"hashesPerSecond"`
	Peers            int      `json:"peers"`
	PeerLatencyMs    *float64 `json:"peerLatencyMs"`
	Block            uint64   `json:"block"`
	PropagationMs    *float64 `json:"propagationMs"`
	PropagationAvgMs *float64 `json:"propagationAvgMs"`
	Pending          int      `json:"pending"`
	UptimePercent    *float64 `json:"uptimePercent"`
	Country          *string  `json:"country"`
	City             *string  `json:"city"`
}

// Alert is an alert showing on the page.
type Alert struct {
	Type    string          `json:"type"`
	Level   string          `json:"level"`
	Since   *int64          `json:"since"`
	Details json.RawMessage `json:"details"`
}

// Incident is a recorded period of an alert condition.
type Incident struct {
	Kind       string          `json:"kind"`
	Level      string          `json:"level"`
	Start      int64           `json:"start"`
	StartKnown bool            `json:"startKnown"`
	End        *int64          `json:"end"`
	Detail     json.RawMessage `json:"detail"`
}
