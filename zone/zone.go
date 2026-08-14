// Package zone holds the shared pivot-zone-fade alert model: the Zone type,
// symbol maps, and the AnalyzeStructure-derived auto-zone computation. Used
// by BOTH the monitor daemon (to fire ntfy) and the web /ops page (to show
// what's armed) — single source so the two never drift.
package zone

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"myFirstGo/trading-bot/bingx"
	"myFirstGo/trading-bot/market"
	"myFirstGo/trading-bot/signal"
)

const DefaultPath = "/opt/trading/zones.json"

// Zone is one price band to watch. Manual zones come from zones.json;
// auto zones are derived from structure each cycle.
type Zone struct {
	Symbol  string  `json:"symbol"` // BTC / ETH / XAU / XAG
	Lo      float64 `json:"lo"`
	Hi      float64 `json:"hi"`
	Dir     string  `json:"dir"`  // short / long / watch
	Note    string  `json:"note"` // free text shown in the push
	TF      string  `json:"tf,omitempty"`
	Enabled *bool   `json:"enabled,omitempty"`
}

var ShortToSym = map[string]market.Symbol{
	"BTC": market.BTCUSDT, "ETH": market.ETHUSDT,
	"XAU": market.XAUUSDT, "XAG": market.XAGUSDT,
}

var SymToShort = map[market.Symbol]string{
	market.BTCUSDT: "BTC", market.ETHUSDT: "ETH",
	market.XAUUSDT: "XAU", market.XAGUSDT: "XAG",
}

// Path — zones.json location (ZONES_PATH env override).
func Path() string {
	if p := os.Getenv("ZONES_PATH"); strings.TrimSpace(p) != "" {
		return p
	}
	return DefaultPath
}

// AutoEnabled — auto structure zones on unless ZONE_AUTO=off.
func AutoEnabled() bool {
	return strings.ToLower(strings.TrimSpace(os.Getenv("ZONE_AUTO"))) != "off"
}

// AutoTFs — which TFs to derive structure zones on (default 1h,2h).
func AutoTFs() []market.Timeframe {
	raw := os.Getenv("ZONE_TFS")
	if strings.TrimSpace(raw) == "" {
		raw = "1h,2h"
	}
	var out []market.Timeframe
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, market.Timeframe(p))
		}
	}
	return out
}

// LoadManual reads the optional manual pins from zones.json.
func LoadManual() []Zone {
	b, err := os.ReadFile(Path())
	if err != nil {
		return nil
	}
	var zs []Zone
	if err := json.Unmarshal(b, &zs); err != nil {
		return nil
	}
	return zs
}

// TrendWord renders a trend for the alert / display text.
func TrendWord(t signal.TrendStructure) string {
	switch t {
	case signal.StructDowntrend:
		return "LH-LL 下跌"
	case signal.StructUptrend:
		return "HH-HL 上升"
	}
	return "neutral"
}

// Key uniquely identifies a zone for debounce state.
func Key(z Zone) string {
	return fmt.Sprintf("%s|%s|%.4f|%.4f|%s", strings.ToUpper(z.Symbol), z.TF, z.Lo, z.Hi, z.Dir)
}

// ComputeAuto derives one zone per (symbol, TF) in a CONFIRMED trend with an
// active pivot zone. Trade dir = trend dir (downtrend → fade short, uptrend
// → fade long). Symbols not cleanly trending are skipped.
func ComputeAuto(ctx context.Context, client *bingx.Client, tfs []market.Timeframe) []Zone {
	var zs []Zone
	for _, sym := range market.All() {
		short := SymToShort[sym]
		for _, tf := range tfs {
			candles, err := client.Klines(ctx, sym, tf, 250)
			if err != nil || len(candles) < 60 {
				continue
			}
			st := signal.AnalyzeStructure(candles, 2)
			if st.Zone == nil {
				continue
			}
			var dir string
			switch st.Trend {
			case signal.StructDowntrend:
				dir = "short"
			case signal.StructUptrend:
				dir = "long"
			default:
				continue
			}
			lo, hi := st.Zone.Lo, st.Zone.Hi
			if hi < lo {
				lo, hi = hi, lo
			}
			note := fmt.Sprintf("auto %s %s 樞紐區 fade｜stop %.4g target %.4g",
				string(tf), TrendWord(st.Trend), st.Zone.Invalidate, st.Zone.Target)
			zs = append(zs, Zone{Symbol: short, Lo: lo, Hi: hi, Dir: dir, Note: note, TF: string(tf)})
		}
	}
	return zs
}
