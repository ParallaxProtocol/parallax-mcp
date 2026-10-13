package explorer

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ParallaxProtocol/parallax-mcp/internal/server"
)

const maxPoints = 400

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

type chartsIn struct {
	Chart      string `json:"chart,omitempty" jsonschema:"chart id, such as newTxns or averageGasPrice; omit to list the available charts"`
	Resolution string `json:"resolution,omitempty" jsonschema:"DAY (default), WEEK, MONTH or YEAR"`
	From       string `json:"from,omitempty" jsonschema:"first date, YYYY-MM-DD (default: 90 points before to)"`
	To         string `json:"to,omitempty" jsonschema:"last date, YYYY-MM-DD (default: today)"`
}

func (t *tools) charts(ctx context.Context, in chartsIn) (any, error) {
	sc := t.r.ExplorerStats
	if sc == nil {
		return nil, fmt.Errorf("no explorer stats service is configured")
	}
	catalog, err := sc.Charts(ctx)
	if err != nil {
		return nil, err
	}
	if in.Chart == "" {
		type entry struct {
			ID          string   `json:"id"`
			Section     string   `json:"section"`
			Title       string   `json:"title"`
			Description string   `json:"description"`
			Units       string   `json:"units,omitempty"`
			Resolutions []string `json:"resolutions"`
		}
		list := make([]entry, 0, len(catalog))
		for _, c := range catalog {
			e := entry{ID: c.ID, Section: c.Section, Title: c.Title, Description: c.Description, Resolutions: c.Resolutions}
			if c.Units != nil {
				e.Units = *c.Units
			}
			list = append(list, e)
		}
		return map[string]any{"charts": list, "note": "call again with chart set to one of these ids"}, nil
	}

	var info *struct {
		units string
		title string
		res   []string
	}
	for _, c := range catalog {
		if strings.EqualFold(c.ID, in.Chart) {
			u := ""
			if c.Units != nil {
				u = *c.Units
			}
			info = &struct {
				units string
				title string
				res   []string
			}{u, c.Title, c.Resolutions}
			in.Chart = c.ID
		}
	}
	if info == nil {
		return nil, server.Inputf("unknown chart %q; call explorer_charts without chart for the list", in.Chart)
	}
	res := strings.ToUpper(in.Resolution)
	if res == "" {
		res = "DAY"
	}
	ok := false
	for _, r := range info.res {
		ok = ok || r == res
	}
	if !ok {
		return nil, server.Inputf("%s has resolutions %s", in.Chart, strings.Join(info.res, ", "))
	}
	for _, d := range []string{in.From, in.To} {
		if d != "" && !dateRe.MatchString(d) {
			return nil, server.Inputf("dates are YYYY-MM-DD, got %q", d)
		}
	}
	from := in.From
	if from == "" {
		end := t.r.Now().UTC()
		if in.To != "" {
			end, _ = time.Parse("2006-01-02", in.To)
		}
		step := map[string]time.Duration{"DAY": 24 * time.Hour, "WEEK": 7 * 24 * time.Hour, "MONTH": 30 * 24 * time.Hour, "YEAR": 365 * 24 * time.Hour}[res]
		from = end.Add(-90 * step).Format("2006-01-02")
	}
	points, err := sc.Chart(ctx, in.Chart, res, from, in.To)
	if err != nil {
		return nil, explorerErr(err, "chart "+in.Chart)
	}
	type point struct {
		Date        string  `json:"date"`
		DateTo      string  `json:"date_to,omitempty"`
		Value       float64 `json:"value"`
		Approximate bool    `json:"approximate,omitempty"`
	}
	out := map[string]any{"chart": in.Chart, "title": info.title, "resolution": res, "from": from}
	if info.units != "" {
		out["units"] = info.units
	}
	if len(points) > maxPoints {
		points = points[len(points)-maxPoints:]
		out["truncated_to_last"] = maxPoints
	}
	list := make([]point, 0, len(points))
	for _, p := range points {
		v, err := strconv.ParseFloat(p.Value, 64)
		if err != nil {
			continue
		}
		pt := point{Date: p.Date, Value: v, Approximate: p.IsApproximate}
		if p.DateTo != p.Date {
			pt.DateTo = p.DateTo
		}
		list = append(list, pt)
	}
	// The stats service pads series with zeros back to dates before the
	// chain existed; leading zeros carry no information.
	lead := 0
	for lead < len(list) && list[lead].Value == 0 {
		lead++
	}
	if lead > 0 {
		list = list[lead:]
		out["leading_zero_points_removed"] = lead
	}
	out["points"] = list
	return out, nil
}
