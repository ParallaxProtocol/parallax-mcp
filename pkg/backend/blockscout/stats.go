package blockscout

import (
	"context"
	"net/url"
	"sync"
	"time"

	"github.com/ParallaxProtocol/parallax-mcp/pkg/httpx"
)

// StatsClient talks to Blockscout's stats service, which serves the
// explorer's charts and counters.
type StatsClient struct {
	http *httpx.Client

	mu      sync.Mutex
	catalog []ChartInfo
	fetched time.Time
}

// NewStats returns a StatsClient for the stats service at opts.BaseURL,
// such as https://explorer.example.org/stats-service.
func NewStats(opts httpx.Options) *StatsClient {
	opts.Name = "explorer_stats"
	return &StatsClient{http: httpx.New(opts)}
}

// ChartInfo describes one chart.
type ChartInfo struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Units       *string  `json:"units"`
	Resolutions []string `json:"resolutions"`
	Section     string   `json:"section"`
}

// Point is one value of a chart. Value is a decimal string.
type Point struct {
	Date          string `json:"date"`
	DateTo        string `json:"date_to"`
	Value         string `json:"value"`
	IsApproximate bool   `json:"is_approximate"`
}

// Counter is one of the stats service's headline figures.
type Counter struct {
	ID          string  `json:"id"`
	Value       string  `json:"value"`
	Title       string  `json:"title"`
	Units       *string `json:"units"`
	Description string  `json:"description"`
}

// Charts lists the available charts, cached for an hour.
func (c *StatsClient) Charts(ctx context.Context) ([]ChartInfo, error) {
	c.mu.Lock()
	if c.catalog != nil && time.Since(c.fetched) < time.Hour {
		defer c.mu.Unlock()
		return c.catalog, nil
	}
	c.mu.Unlock()
	var resp struct {
		Sections []struct {
			ID     string      `json:"id"`
			Charts []ChartInfo `json:"charts"`
		} `json:"sections"`
	}
	if err := c.http.GetJSON(ctx, "/api/v1/lines", nil, &resp); err != nil {
		return nil, err
	}
	var out []ChartInfo
	for _, s := range resp.Sections {
		for _, ch := range s.Charts {
			ch.Section = s.ID
			out = append(out, ch)
		}
	}
	c.mu.Lock()
	c.catalog, c.fetched = out, time.Now()
	c.mu.Unlock()
	return out, nil
}

// Chart returns a chart's points at a resolution (DAY, WEEK, MONTH, YEAR)
// between from and to (YYYY-MM-DD, either may be empty).
func (c *StatsClient) Chart(ctx context.Context, id, resolution, from, to string) ([]Point, error) {
	q := url.Values{"resolution": {resolution}}
	if from != "" {
		q.Set("from", from)
	}
	if to != "" {
		q.Set("to", to)
	}
	var resp struct {
		Chart []Point `json:"chart"`
	}
	return resp.Chart, c.http.GetJSON(ctx, "/api/v1/lines/"+url.PathEscape(id), q, &resp)
}

// Counters returns the headline counters.
func (c *StatsClient) Counters(ctx context.Context) ([]Counter, error) {
	var resp struct {
		Counters []Counter `json:"counters"`
	}
	return resp.Counters, c.http.GetJSON(ctx, "/api/v1/counters", nil, &resp)
}
