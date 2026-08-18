// Package twse builds a Taiwan-stock fundamental board from the TWSE open-data
// API (free, no token). Unlike Finnhub (per-symbol, US-only, TW gated behind
// paid), TWSE serves BULK datasets — one call returns every listed stock — so
// there's no rate limit and no cron: fetch 3 endpoints, join by 公司代號, score.
//
// This is a spot / buy-hold indicator for REAL Taiwan shares (no BingX, no
// technical layer). TW investing weights dividend yield heavily (存股), so the
// rating surfaces yield alongside the quality × valuation matrix.
package twse

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const base = "https://openapi.twse.com.tw/v1"

// Stock is the joined per-symbol fundamentals.
type Stock struct {
	Code           string  `json:"code"`
	Name           string  `json:"name"`
	Industry       string  `json:"industry"`
	PE             float64 `json:"pe"`
	PB             float64 `json:"pb"`
	DividendYield  float64 `json:"dividend_yield"` // percent
	RevYoYPct      float64 `json:"rev_yoy_pct"`    // monthly revenue YoY %
	GrossMarginPct float64 `json:"gross_margin_pct"`
	TradeValue     float64 `json:"trade_value"` // daily NTD turnover — size/liquidity proxy
	Close          float64 `json:"close"`
	HasRev         bool    `json:"-"`
	HasMargin      bool    `json:"-"`
}

// atof parses TWSE's string numbers: strips commas, treats ""/"-"/"N/A" as absent.
func atof(s string) (float64, bool) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", ""))
	if s == "" || s == "-" || s == "N/A" || s == "--" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

func getJSON(ctx context.Context, hc *http.Client, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("twse GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("twse %s: HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// FetchAll pulls the 3 bulk datasets and joins them by 公司代號. Valuation
// (BWIBBU_ALL) is the spine — a stock must have PE/PB to appear; revenue and
// margin are best-effort overlays.
func FetchAll(ctx context.Context, hc *http.Client) (map[string]*Stock, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}

	// 1) valuation: PE / PB / yield (the spine)
	var val []struct {
		Code, Name, PEratio, DividendYield, PBratio string
	}
	if err := getJSON(ctx, hc, "exchangeReport/BWIBBU_ALL", &val); err != nil {
		return nil, err
	}
	out := make(map[string]*Stock, len(val))
	for _, r := range val {
		s := &Stock{Code: r.Code, Name: r.Name}
		s.PE, _ = atof(r.PEratio)
		s.PB, _ = atof(r.PBratio)
		s.DividendYield, _ = atof(r.DividendYield)
		out[r.Code] = s
	}

	// 2) monthly revenue YoY (best-effort overlay)
	var rev []map[string]string
	if err := getJSON(ctx, hc, "opendata/t187ap05_L", &rev); err == nil {
		for _, r := range rev {
			code := r["公司代號"]
			s := out[code]
			if s == nil {
				continue
			}
			if s.Industry == "" {
				s.Industry = r["產業別"]
			}
			if v, ok := atof(r["營業收入-去年同月增減(%)"]); ok {
				s.RevYoYPct = v
				s.HasRev = true
			}
		}
	}

	// 2b) daily trading value → size/liquidity proxy + close price
	var day []struct {
		Code, TradeValue, ClosingPrice string
	}
	if err := getJSON(ctx, hc, "exchangeReport/STOCK_DAY_ALL", &day); err == nil {
		for _, r := range day {
			if s := out[r.Code]; s != nil {
				s.TradeValue, _ = atof(r.TradeValue)
				s.Close, _ = atof(r.ClosingPrice)
			}
		}
	}

	// 3) income statement → gross margin (best-effort overlay)
	var inc []map[string]string
	if err := getJSON(ctx, hc, "opendata/t187ap06_L_ci", &inc); err == nil {
		for _, r := range inc {
			s := out[r["公司代號"]]
			if s == nil {
				continue
			}
			revenue, okR := atof(r["營業收入"])
			gross, okG := atof(r["營業毛利（毛損）淨額"])
			if okR && okG && revenue != 0 {
				s.GrossMarginPct = gross / revenue * 100
				s.HasMargin = true
			}
		}
	}
	return out, nil
}

// Rated is a scored TW stock.
type Rated struct {
	Stock
	Quality   float64 `json:"quality"`   // 0-100 (rev growth + gross margin)
	Valuation float64 `json:"valuation"` // 0-100, higher = cheaper (PE + PB)
	Label     string  `json:"label"`     // buy / hold / avoid / unknown
	HighYield bool    `json:"high_yield"`// dividend yield >= 4% (存股-friendly)
	Note      string  `json:"note"`
}

func bandHigh(v float64, ok bool, cuts [][2]float64) (float64, bool) {
	if !ok {
		return 0, false
	}
	for _, c := range cuts {
		if v >= c[0] {
			return c[1], true
		}
	}
	return 15, true
}
func bandLow(v float64, ok bool, cuts [][2]float64) (float64, bool) {
	if !ok || v <= 0 {
		return 0, false
	}
	for _, c := range cuts {
		if v <= c[0] {
			return c[1], true
		}
	}
	return 10, true
}
func meanOK(xs ...struct {
	v  float64
	ok bool
}) (float64, bool) {
	sum, n := 0.0, 0
	for _, x := range xs {
		if x.ok {
			sum += x.v
			n++
		}
	}
	if n == 0 {
		return 0, false
	}
	return sum / float64(n), true
}

// Rate scores a stock. TW valuation bands are set lower than US (the TAIEX
// trades cheaper on average); quality is thinner than US (only rev-growth +
// gross-margin from free data) so confidence is inherently lower.
func Rate(s Stock) Rated {
	r := Rated{Stock: s}

	pe, peOK := bandLow(s.PE, s.PE > 0, [][2]float64{{12, 100}, {18, 70}, {25, 40}})
	pb, pbOK := bandLow(s.PB, s.PB > 0, [][2]float64{{1.5, 100}, {3, 70}, {5, 40}})
	val, valOK := meanOK(sub(pe, peOK), sub(pb, pbOK))

	rev, revOK := bandHigh(s.RevYoYPct, s.HasRev, [][2]float64{{20, 100}, {10, 75}, {0.0001, 50}})
	gm, gmOK := bandHigh(s.GrossMarginPct, s.HasMargin, [][2]float64{{30, 100}, {20, 75}, {10, 50}, {0.0001, 30}})
	qual, qualOK := meanOK(sub(rev, revOK), sub(gm, gmOK))

	r.Valuation = round1(val)
	r.Quality = round1(qual)
	r.HighYield = s.DividendYield >= 4.0

	switch {
	case !valOK:
		r.Label = "unknown"
		r.Note = "no valuation data"
	case !qualOK:
		// Valuation-only (no growth/margin) — rate on value + yield, flag low conf.
		if val >= 70 {
			r.Label = "buy"
			r.Note = fmt.Sprintf("cheap (val %.0f) — valuation-only, quality unknown", val)
		} else if val < 30 {
			r.Label = "avoid"
			r.Note = fmt.Sprintf("expensive (val %.0f), quality unknown", val)
		} else {
			r.Label = "hold"
			r.Note = "quality data unavailable"
		}
	case qual < 40:
		r.Label = "avoid"
		r.Note = fmt.Sprintf("weak quality (%.0f) — value-trap risk", qual)
	case qual >= 60 && val >= 40:
		r.Label = "buy"
		r.Note = fmt.Sprintf("solid quality (%.0f) at a fair price (val %.0f)", qual, val)
	case qual >= 60 && val < 40:
		r.Label = "hold"
		r.Note = fmt.Sprintf("solid quality (%.0f) but rich (val %.0f)", qual, val)
	default:
		r.Label = "hold"
		r.Note = fmt.Sprintf("mixed: quality %.0f / valuation %.0f", qual, val)
	}
	if r.HighYield && (r.Label == "buy" || r.Label == "hold") {
		r.Note += fmt.Sprintf(" · 高股息 %.1f%%", s.DividendYield)
	}
	return r
}

func sub(v float64, ok bool) struct {
	v  float64
	ok bool
} {
	return struct {
		v  float64
		ok bool
	}{v, ok}
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }

// RateAll scores + sorts a fetched map: buy → hold → avoid → unknown, then
// quality desc.
func RateAll(m map[string]*Stock) []Rated {
	out := make([]Rated, 0, len(m))
	for _, s := range m {
		out = append(out, Rate(*s))
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := labelRank(out[i].Label), labelRank(out[j].Label)
		if ri != rj {
			return ri < rj
		}
		return out[i].Quality > out[j].Quality
	})
	return out
}

func labelRank(l string) int {
	switch l {
	case "buy":
		return 0
	case "hold":
		return 1
	case "avoid":
		return 2
	default:
		return 3
	}
}
