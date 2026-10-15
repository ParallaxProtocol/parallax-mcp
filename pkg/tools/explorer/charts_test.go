package explorer_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ParallaxProtocol/parallax-mcp/internal/testutil"
)

const lines = `{"sections":[{"id":"transactions","title":"Transactions","charts":[
 {"id":"newTxns","title":"New transactions","description":"Number of new transactions","units":null,"resolutions":["DAY","WEEK","MONTH","YEAR"]},
 {"id":"averageGasPrice","title":"Average gas price","description":"Average gas price","units":"Gwei","resolutions":["DAY","MONTH"]}]}]}`

func chartsEnv(t *testing.T) *testutil.Env {
	e := newEnv(t, func(e *testutil.Env) {
		e.Explorer.Route("/stats-service/api/v1/lines", lines)
		e.Explorer.Route("/stats-service/api/v1/lines/averageGasPrice", `{"chart":[
		  {"date":"2019-05-01","date_to":"2019-05-31","value":"0"},{"date":"2025-10-01","date_to":"2025-10-31","value":"4.69"},
		  {"date":"2025-11-01","date_to":"2025-11-30","value":"19.46","is_approximate":true}]}`)
		e.Explorer.Route("/stats-service/api/v1/counters", `{"counters":[{"id":"txnsFee24h","value":"1.0","title":"Transactions fees (24h)","units":"LAX"}]}`)
		e.Explorer.Route("/api/v2/stats", `{"total_blocks":"59634","average_block_time":432030.0}`)
	})
	e.Now = time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	return e
}

func TestChartsCatalogAndSeries(t *testing.T) {
	e := chartsEnv(t)
	out := e.MustCall(t, "explorer_charts", nil)
	if len(out["charts"].([]any)) != 2 || testutil.Get(out, "charts", 1, "units") != "Gwei" {
		t.Fatalf("catalog: %v", out)
	}
	out = e.MustCall(t, "explorer_charts", map[string]any{"chart": "averagegasprice", "resolution": "month"})
	if out["chart"] != "averageGasPrice" || out["leading_zero_points_removed"] != float64(1) || len(out["points"].([]any)) != 2 {
		t.Fatalf("series: %v", out)
	}
	if testutil.Get(out, "points", 1, "approximate") != true || testutil.Get(out, "points", 0, "value") != 4.69 {
		t.Fatalf("points: %v", out["points"])
	}
	for name, args := range map[string]map[string]any{
		"unknown":    {"chart": "nope"},
		"resolution": {"chart": "averageGasPrice", "resolution": "WEEK"},
		"date":       {"chart": "newTxns", "from": "13/10/2026"},
	} {
		if _, errText := e.Call(t, "explorer_charts", args); errText == "" {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestStatsIncludesCounters(t *testing.T) {
	e := chartsEnv(t)
	out := e.MustCall(t, "explorer_stats", nil)
	if testutil.Get(out, "counters", "txnsFee24h", "units") != "LAX" || out["average_block_time_seconds"] != 432.03 {
		t.Fatalf("got %v", out)
	}
	e.Explorer.SetFail(503)
	if _, errText := e.Call(t, "explorer_stats", nil); !strings.Contains(errText, "503") {
		t.Fatalf("both down: %q", errText)
	}
}
