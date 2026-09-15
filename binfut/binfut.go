// Package binfut reads Binance USD-M futures public positioning data:
// an open-interest TIME SERIES, and the whale-vs-retail long/short split.
//
// Why a second venue. The oi/ package samples BingX, which is the book the
// user's orders actually sit in — the right answer for "am I on the crowded
// side of my own exchange". But BingX publishes no OI history endpoint (both
// openInterestHist spellings answer "this api is not exist"), so the series
// has to be accumulated by polling, and the venue republishes the figure only
// about every ten minutes: 112 of 254 consecutive 5-minute samples came back
// byte-identical. The honest floor on that source is a 1-hour window.
//
// Binance publishes the series itself, in real 5m/15m/30m/1h/... buckets. That
// removes the accumulation problem outright — this package does not need a
// store that grows, only a cache that expires.
//
// It also answers a question BingX cannot: Binance splits long/short by TOP
// TRADERS versus ALL ACCOUNTS, which is the whale-vs-retail read directly
// measured rather than inferred from the shape of an accumulation curve.
//
// And it covers what BingX freezes. BingX publishes a FIXED open interest for
// its NCCO*/NCSK* synthetics — one distinct value across 30 samples — so XAU,
// XAG and the five US-stock names have no readable OI there at all. Binance
// lists them as TRADIFI_PERPETUAL contracts with real figures. All 14 roster
// symbols map.
//
// Keyless. Coinglass, the usual aggregator, answers "API key missing" on every
// endpoint without a paid plan.
//
// CROSS-REFERENCE, NOT REPLACEMENT. This is Binance's book. Positioning there
// can differ from BingX's, and an entry is filled against BingX's. Read the two
// together; do not silently substitute one for the other.
package binfut

import (
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

const apiBase = "https://fapi.binance.com/futures/data/"

// Symbols maps this project's short names to Binance USD-M symbols.
//
// Verified against /fapi/v1/exchangeInfo on 2026-09-12: all fourteen exist and
// are TRADING. The seven that BingX carries as synthetics are contractType
// TRADIFI_PERPETUAL here — the same underlyings, a different wrapper.
var Symbols = map[string]string{
	"BTC": "BTCUSDT", "ETH": "ETHUSDT", "SOL": "SOLUSDT", "LINK": "LINKUSDT",
	"SUI": "SUIUSDT", "NEAR": "NEARUSDT", "HYPE": "HYPEUSDT",
	"XAU": "XAUUSDT", "XAG": "XAGUSDT",
	"SNDK": "SNDKUSDT", "NVDA": "NVDAUSDT", "MSTR": "MSTRUSDT",
	"SPCX": "SPCXUSDT", "APP": "APPUSDT",
}

// Period is the bucket size requested from Binance. 5m matches the cadence
// the venue itself publishes these at, so a shorter refresh would re-read the
// same bucket.
const Period = "5m"

// histLimit is how many buckets to pull. 36 x 5m = 3 hours, enough to compute
// a 1h delta with room either side and still fit one small response.
const histLimit = 36

// OIPoint is one open-interest bucket. Value is the USDT notional, which is
// what compares across symbols; Contracts is the base-asset count.
type OIPoint struct {
	Time      time.Time `json:"time"`
	Contracts float64   `json:"contracts"`
	Value     float64   `json:"value"`
}

// RatioPoint is one long/short reading. Ratio is longs divided by shorts, so
// 1.0 is balanced and above 1.0 leans long.
type RatioPoint struct {
	Time  time.Time `json:"time"`
	Ratio float64   `json:"ratio"`
	Long  float64   `json:"long"`  // fraction, 0-1
	Short float64   `json:"short"` // fraction, 0-1
}

// SymbolData is everything held for one symbol, oldest bucket first.
type SymbolData struct {
	OI []OIPoint `json:"oi"`
	// TopAccounts and AllAccounts are BOTH by account count, which is what
	// makes them comparable. TopPositions is size-weighted and is a third
	// reading, not a substitute: differencing it against AllAccounts would
	// mix whale-vs-retail with size-vs-headcount.
	TopAccounts  []RatioPoint `json:"top_accounts"`
	AllAccounts  []RatioPoint `json:"all_accounts"`
	TopPositions []RatioPoint `json:"top_positions"`
}

// Store is the cached payload, keyed by this project's SHORT name.
type Store struct {
	FetchedAt time.Time             `json:"fetched_at"`
	Symbols   map[string]SymbolData `json:"symbols"`
}

// Path is the on-disk cache (env BINFUT_CACHE or default).
func Path() string {
	if p := strings.TrimSpace(os.Getenv("BINFUT_CACHE")); p != "" {
		return p
	}
	return "/opt/trading/binfut.json"
}

// MinRefresh matches Binance's own 5-minute bucket: refreshing faster would
// re-read a bucket that has not changed.
const MinRefresh = 5 * time.Minute

// Stale reports whether the cache is old enough to refresh.
func (s Store) Stale(now time.Time) bool {
	return s.FetchedAt.IsZero() || now.UTC().Sub(s.FetchedAt) >= MinRefresh
}

func getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("binance %s: HTTP %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type rawOI struct {
	SumOpenInterest      string `json:"sumOpenInterest"`
	SumOpenInterestValue string `json:"sumOpenInterestValue"`
	Timestamp            int64  `json:"timestamp"`
}

type rawRatio struct {
	LongAccount    string `json:"longAccount"`
	ShortAccount   string `json:"shortAccount"`
	LongShortRatio string `json:"longShortRatio"`
	Timestamp      int64  `json:"timestamp"`
}

func f(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }

func ms(t int64) time.Time { return time.UnixMilli(t).UTC() }

func fetchOI(ctx context.Context, sym string) ([]OIPoint, error) {
	var raw []rawOI
	url := fmt.Sprintf("%sopenInterestHist?symbol=%s&period=%s&limit=%d", apiBase, sym, Period, histLimit)
	if err := getJSON(ctx, url, &raw); err != nil {
		return nil, err
	}
	out := make([]OIPoint, 0, len(raw))
	for _, r := range raw {
		if v := f(r.SumOpenInterestValue); v > 0 {
			out = append(out, OIPoint{Time: ms(r.Timestamp), Contracts: f(r.SumOpenInterest), Value: v})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

func fetchRatio(ctx context.Context, endpoint, sym string) ([]RatioPoint, error) {
	var raw []rawRatio
	url := fmt.Sprintf("%s%s?symbol=%s&period=%s&limit=%d", apiBase, endpoint, sym, Period, histLimit)
	if err := getJSON(ctx, url, &raw); err != nil {
		return nil, err
	}
	out := make([]RatioPoint, 0, len(raw))
	for _, r := range raw {
		if v := f(r.LongShortRatio); v > 0 {
			out = append(out, RatioPoint{Time: ms(r.Timestamp), Ratio: v, Long: f(r.LongAccount), Short: f(r.ShortAccount)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

// Fetch pulls every mapped symbol. A symbol that fails is omitted rather than
// failing the batch: one delisting must not blank the other thirteen.
func Fetch(ctx context.Context) (Store, error) {
	shorts := make([]string, 0, len(Symbols))
	for s := range Symbols {
		shorts = append(shorts, s)
	}
	sort.Strings(shorts)

	out := Store{FetchedAt: time.Now().UTC(), Symbols: map[string]SymbolData{}}
	var firstErr error
	for _, short := range shorts {
		sym := Symbols[short]
		var d SymbolData
		var err error
		if d.OI, err = fetchOI(ctx, sym); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		// The ratios are best-effort on top of a successful OI read: a symbol
		// with OI and no ratios is still useful, and dropping it whole would
		// lose the half that did arrive.
		d.TopAccounts, _ = fetchRatio(ctx, "topLongShortAccountRatio", sym)
		d.AllAccounts, _ = fetchRatio(ctx, "globalLongShortAccountRatio", sym)
		d.TopPositions, _ = fetchRatio(ctx, "topLongShortPositionRatio", sym)
		out.Symbols[short] = d
	}
	if len(out.Symbols) == 0 {
		if firstErr != nil {
			return Store{}, fmt.Errorf("binfut: no symbols fetched: %w", firstErr)
		}
		return Store{}, fmt.Errorf("binfut: no symbols fetched")
	}
	return out, nil
}

// Load reads the cache. A missing file is the state before the first fetch,
// not an error.
func Load() Store {
	b, err := os.ReadFile(Path())
	if err != nil {
		return Store{}
	}
	var s Store
	if json.Unmarshal(b, &s) != nil || len(s.Symbols) == 0 {
		return Store{}
	}
	return s
}

// Save writes via tmp -> rename so a reader never sees a partial file and
// treats it as "no data".
func Save(s Store) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	p := Path()
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	var check Store
	if b2, err := os.ReadFile(tmp); err != nil || json.Unmarshal(b2, &check) != nil || len(check.Symbols) == 0 {
		_ = os.Remove(tmp)
		return fmt.Errorf("binfut: cache failed its own re-read")
	}
	return os.Rename(tmp, p)
}
