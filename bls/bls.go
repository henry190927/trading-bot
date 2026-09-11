// Package bls reads released macro numbers from the Bureau of Labor
// Statistics public API, so the calendar can show what a print ACTUALLY was
// next to what it was forecast to be.
//
// Why this exists. The calendar had forecasts and nothing else. On 2026-09-10
// a PPI release moved BTC 1,290 points in one hour and the only way to ask
// "did it actually come in hot?" was to infer it from the price — which is
// backwards, and which produced the wrong answer: headline PPI final demand
// printed +0.40% m/m against a +0.4% forecast. Exactly in line. The move came
// from somewhere else.
//
// Source choice. The ForexFactory feed the forecasts come from carries an
// `actual` field that is empty on every row, released or not, so it cannot
// answer this. BLS is the agency that publishes CPI, PPI and the Employment
// Situation, which makes it the authority rather than a mirror. Its v1 API
// needs no registration key and answered 200 on first contact. FRED
// (api.stlouisfed.org) refuses without a key; DBnomics works keyless but
// mirrors BLS, so it would add a hop and a trust boundary for nothing.
//
// Rate limit is the design constraint: v1 allows 25 QUERIES per day, but up
// to 25 SERIES per query. So this asks for every series in one POST and
// caches the answer to disk — a refresh costs 1 of 25, and the data changes
// at most once a month per series.
//
// What this does NOT do: predict. It reports released numbers. The index
// levels are authoritative; the month-over-month and year-over-year figures
// are computed here from consecutive periods, which is how the headline
// percentages the market quotes are derived.
package bls

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const apiURL = "https://api.bls.gov/publicAPI/v1/timeseries/data/"

// Series are the BLS series this package trusts itself to label.
//
// Deliberately short. v1 disables the catalog endpoint ("The catalog has been
// disabled for this request"), so a series ID cannot be verified against its
// own title over this API — and the whole point is to put a number next to a
// forecast, where a mislabelled series is worse than a blank. Every ID here
// is one whose identity is unambiguous from the BLS naming scheme and whose
// level cross-checks against the published headline.
//
// Core PPI is absent on purpose. WPSFD4131 / WPSFD49104 / WPSFD49116 /
// WPSFD41 all return data and their m/m for 2026-08 ranges from +0.16% to
// +1.10%, which is the spread between "in line" and "four times forecast".
// Picking one without the catalog would be a guess wearing a number's
// clothes. Registering for a v2 key unlocks catalog=true and settles it.
var Series = map[string]string{
	"CUSR0000SA0":    "CPI, all items, seasonally adjusted",
	"CUUR0000SA0":    "CPI, all items, not seasonally adjusted",
	"CUSR0000SA0L1E": "Core CPI (less food and energy), seasonally adjusted",
	"CUUR0000SA0L1E": "Core CPI (less food and energy), not seasonally adjusted",
	"WPSFD4":         "PPI, final demand, seasonally adjusted",
	"CES0000000001":  "Total nonfarm payrolls, seasonally adjusted (thousands)",
}

// Point is one monthly observation.
type Point struct {
	Year  int     `json:"year"`
	Month int     `json:"month"` // 1-12
	Value float64 `json:"value"`
}

// Store is the cached payload: every trusted series, oldest observation
// first, plus when it was fetched.
type Store struct {
	FetchedAt time.Time          `json:"fetched_at"`
	Series    map[string][]Point `json:"series"`
}

// Path is the on-disk cache (env BLS_CACHE or default).
func Path() string {
	if p := strings.TrimSpace(os.Getenv("BLS_CACHE")); p != "" {
		return p
	}
	return "/opt/trading/bls.json"
}

// MinRefresh is the shortest gap between live fetches.
//
// Six hours puts a full day inside 4 of the 25 daily queries, leaving room
// for manual refreshes, and no BLS series updates more than monthly — the
// only thing a tighter cadence would buy is picking up a release sooner on
// the one morning a month it lands.
const MinRefresh = 6 * time.Hour

type apiResponse struct {
	Status  string   `json:"status"`
	Message []string `json:"message"`
	Results struct {
		Series []struct {
			SeriesID string `json:"seriesID"`
			Data     []struct {
				Year   string `json:"year"`
				Period string `json:"period"` // "M01".."M13"
				Value  string `json:"value"`
			} `json:"data"`
		} `json:"series"`
	} `json:"Results"`
}

// Fetch asks BLS for every trusted series in one query and returns a Store.
//
// Years: BLS v1 defaults to the most recent ~3 years when no range is given,
// which is more than a year-over-year needs and costs the same one query, so
// the range is left to the default rather than pinned to a window that would
// silently truncate a y/y across a January.
func Fetch(ctx context.Context) (Store, error) {
	ids := make([]string, 0, len(Series))
	for id := range Series {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	body, err := json.Marshal(map[string]any{"seriesid": ids})
	if err != nil {
		return Store{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return Store{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return Store{}, fmt.Errorf("bls fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Store{}, fmt.Errorf("bls fetch: HTTP %d", resp.StatusCode)
	}

	var ar apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return Store{}, fmt.Errorf("bls decode: %w", err)
	}
	// BLS answers 200 with a status field for its own errors (throttling, a
	// bad series ID), so the HTTP code alone does not mean success.
	if ar.Status != "REQUEST_SUCCEEDED" {
		return Store{}, fmt.Errorf("bls: %s %v", ar.Status, ar.Message)
	}

	out := Store{FetchedAt: time.Now().UTC(), Series: map[string][]Point{}}
	for _, s := range ar.Results.Series {
		var pts []Point
		for _, d := range s.Data {
			// M13 is BLS's annual average, not a month. Including it would
			// put a 13th period between December and January and corrupt
			// every m/m that crossed it.
			if !strings.HasPrefix(d.Period, "M") || d.Period == "M13" {
				continue
			}
			m, err := strconv.Atoi(strings.TrimPrefix(d.Period, "M"))
			if err != nil || m < 1 || m > 12 {
				continue
			}
			y, err := strconv.Atoi(d.Year)
			if err != nil {
				continue
			}
			v, err := strconv.ParseFloat(d.Value, 64)
			if err != nil {
				continue
			}
			pts = append(pts, Point{Year: y, Month: m, Value: v})
		}
		sort.Slice(pts, func(i, j int) bool {
			if pts[i].Year != pts[j].Year {
				return pts[i].Year < pts[j].Year
			}
			return pts[i].Month < pts[j].Month
		})
		if len(pts) > 0 {
			out.Series[s.SeriesID] = pts
		}
	}
	if len(out.Series) == 0 {
		return Store{}, fmt.Errorf("bls: response carried no usable series")
	}
	return out, nil
}

// Load reads the cache. A missing or unreadable file returns a zero Store and
// no error — that is the state before the first fetch, not a failure.
func Load() Store {
	b, err := os.ReadFile(Path())
	if err != nil {
		return Store{}
	}
	var s Store
	if json.Unmarshal(b, &s) != nil || len(s.Series) == 0 {
		return Store{}
	}
	return s
}

// Save writes the cache via tmp -> rename, so a reader never sees a partial
// file and silently treat it as "no data released yet".
func Save(s Store) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	p := Path()
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	var check Store
	if b2, err := os.ReadFile(tmp); err != nil || json.Unmarshal(b2, &check) != nil {
		os.Remove(tmp)
		return fmt.Errorf("bls: cache failed its own re-read")
	}
	return os.Rename(tmp, p)
}

// Stale reports whether the cache is old enough to refresh.
func (s Store) Stale(now time.Time) bool {
	return s.FetchedAt.IsZero() || now.UTC().Sub(s.FetchedAt) >= MinRefresh
}

// At returns the observation for a specific year/month.
func (s Store) At(seriesID string, year, month int) (Point, bool) {
	for _, p := range s.Series[seriesID] {
		if p.Year == year && p.Month == month {
			return p, true
		}
	}
	return Point{}, false
}

// MoM returns the month-over-month percent change into (year, month), i.e.
// the headline figure the market quotes for CPI and PPI.
func (s Store) MoM(seriesID string, year, month int) (float64, bool) {
	cur, ok := s.At(seriesID, year, month)
	if !ok {
		return 0, false
	}
	py, pm := year, month-1
	if pm == 0 {
		py, pm = year-1, 12
	}
	prev, ok := s.At(seriesID, py, pm)
	if !ok || prev.Value == 0 {
		return 0, false
	}
	return (cur.Value/prev.Value - 1) * 100, true
}

// YoY returns the year-over-year percent change into (year, month).
func (s Store) YoY(seriesID string, year, month int) (float64, bool) {
	cur, ok := s.At(seriesID, year, month)
	if !ok {
		return 0, false
	}
	prev, ok := s.At(seriesID, year-1, month)
	if !ok || prev.Value == 0 {
		return 0, false
	}
	return (cur.Value/prev.Value - 1) * 100, true
}

// Diff returns the absolute month-over-month change, which is how payrolls
// are reported (a level change in thousands, not a percent).
func (s Store) Diff(seriesID string, year, month int) (float64, bool) {
	cur, ok := s.At(seriesID, year, month)
	if !ok {
		return 0, false
	}
	py, pm := year, month-1
	if pm == 0 {
		py, pm = year-1, 12
	}
	prev, ok := s.At(seriesID, py, pm)
	if !ok {
		return 0, false
	}
	return cur.Value - prev.Value, true
}

// Latest returns the most recent observation held for a series.
func (s Store) Latest(seriesID string) (Point, bool) {
	pts := s.Series[seriesID]
	if len(pts) == 0 {
		return Point{}, false
	}
	return pts[len(pts)-1], true
}
