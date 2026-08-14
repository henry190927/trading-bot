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
	"strconv"
	"strings"

	"myFirstGo/trading-bot/bingx"
	"myFirstGo/trading-bot/market"
	"myFirstGo/trading-bot/signal"
)

// FmtPrice renders a price for humans — thousands-separated for BTC/ETH/XAU
// scale (avoids %g scientific notation like "6.282e+04"), decimals kept for
// sub-1000 (XAG). e.g. 62820 → "62,820", 4343.9 → "4,343.9", 64.91 → "64.91".
func FmtPrice(v float64) string {
	if v < 1000 && v > -1000 {
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	s := strconv.FormatFloat(v, 'f', 1, 64)
	s = strings.TrimSuffix(s, ".0")
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	intPart, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, frac = s[:i], s[i:]
	}
	n := len(intPart)
	var b strings.Builder
	for i, c := range intPart {
		if i > 0 && (n-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	out := b.String() + frac
	if neg {
		out = "-" + out
	}
	return out
}

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

// Config is the live-editable zone-alert config, stored as JSON and re-read
// each cycle by the monitor goroutine — so /ops edits apply within one poll
// (no restart). Separate from the secrets .env.
type Config struct {
	Enabled bool   `json:"enabled"` // master on/off for the whole channel
	Auto    bool   `json:"auto"`    // auto-derive zones from structure
	TFs     string `json:"tfs"`     // comma-separated, e.g. "1h,2h"
}

// ConfigPath — zone-config.json location (ZONE_CONFIG_PATH override).
func ConfigPath() string {
	if p := os.Getenv("ZONE_CONFIG_PATH"); strings.TrimSpace(p) != "" {
		return p
	}
	return "/opt/trading/zone-config.json"
}

// DefaultConfig falls back to env/defaults when no config file exists yet.
func DefaultConfig() Config {
	tfs := strings.TrimSpace(os.Getenv("ZONE_TFS"))
	if tfs == "" {
		tfs = "1h,2h"
	}
	return Config{Enabled: true, Auto: AutoEnabled(), TFs: tfs}
}

// ReadConfig reads the live config (call each cycle). Missing/bad file →
// defaults.
func ReadConfig() Config {
	b, err := os.ReadFile(ConfigPath())
	if err != nil {
		return DefaultConfig()
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return DefaultConfig()
	}
	if strings.TrimSpace(c.TFs) == "" {
		c.TFs = "1h,2h"
	}
	return c
}

// WriteConfig persists the config (used by /ops).
func WriteConfig(c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ConfigPath(), b, 0644)
}

// TFList parses the comma-separated TFs into timeframes.
func (c Config) TFList() []market.Timeframe {
	var out []market.Timeframe
	for _, p := range strings.Split(c.TFs, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, market.Timeframe(p))
		}
	}
	return out
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
			note := fmt.Sprintf("auto %s %s 樞紐區 fade｜stop %s target %s",
				string(tf), TrendWord(st.Trend), FmtPrice(st.Zone.Invalidate), FmtPrice(st.Zone.Target))
			zs = append(zs, Zone{Symbol: short, Lo: lo, Hi: hi, Dir: dir, Note: note, TF: string(tf)})
		}
	}
	return zs
}
