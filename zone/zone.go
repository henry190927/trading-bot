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
	// Confirm selects what counts as a trigger. Empty = the original
	// live-intrabar touch, which is right for a fade zone (you want the wick
	// into the band). It is WRONG for a breakout gate: 2026-09-02 a BTC wick
	// tagged 77413 inside the 77390-77476 cluster and the bar closed back at
	// 77132, so the push said "gate open" about a bar that never closed there.
	// See ConfirmCloseAbove etc.
	Confirm string `json:"confirm,omitempty"`
}

// Confirmation modes for Zone.Confirm.
const (
	ConfirmTouch      = ""            // live mark price inside [Lo,Hi] (default)
	ConfirmCloseIn    = "close-in"    // a closed bar closes inside the band
	ConfirmCloseAbove = "close-above" // a closed bar closes above Hi
	ConfirmCloseBelow = "close-below" // a closed bar closes below Lo
)

// NeedsClose reports whether this zone must be evaluated on a closed bar
// rather than on live price. Such a zone REQUIRES TF — without one there is
// no bar to close.
func (z Zone) NeedsClose() bool {
	return strings.TrimSpace(z.Confirm) != ConfirmTouch
}

// EvalConfirm applies the zone's confirmation mode to one closed bar's close.
//
// known=false means the mode string is unrecognised. The caller must SKIP the
// zone in that case, never fall back to a touch alert: a zone that reads as
// close-confirmed while firing on wicks is the display-vs-reality bug class
// this field exists to remove.
func EvalConfirm(z Zone, closePx float64) (fired bool, known bool) {
	switch strings.ToLower(strings.TrimSpace(z.Confirm)) {
	case ConfirmCloseIn:
		return closePx >= z.Lo && closePx <= z.Hi, true
	case ConfirmCloseAbove:
		return closePx > z.Hi, true
	case ConfirmCloseBelow:
		return closePx < z.Lo, true
	}
	return false, false
}

// TriggerFault returns a non-empty human reason when the zone-alert loop would
// SKIP this zone instead of evaluating it. /ops calls the same function so the
// "armed" list cannot advertise a band the daemon refuses to look at — the
// display-vs-reality class of bug that the 2026-09-02 ops sweep was all about.
func TriggerFault(z Zone) string {
	// Symbol and band first: these were checked inline in the daemon and
	// nowhere else, which is how an unresolvable symbol could sit in the
	// armed list looking healthy.
	if _, ok := ShortToSym[strings.ToUpper(strings.TrimSpace(z.Symbol))]; !ok {
		return "未知 symbol: " + z.Symbol
	}
	if z.Hi <= z.Lo {
		return fmt.Sprintf("帶無效: hi %g <= lo %g", z.Hi, z.Lo)
	}
	if !z.NeedsClose() {
		return ""
	}
	if strings.TrimSpace(z.TF) == "" {
		return "confirm 需要 tf"
	}
	if _, known := EvalConfirm(z, 0); !known {
		return "未知 confirm: " + z.Confirm
	}
	return ""
}

// ConfirmWord renders the mode for the push text and the /ops list, so the
// notification itself says which rule fired it.
func ConfirmWord(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case ConfirmCloseIn:
		return "收在帶內"
	case ConfirmCloseAbove:
		return "收破上緣"
	case ConfirmCloseBelow:
		return "收破下緣"
	case ConfirmTouch:
		return "即時觸價"
	}
	return "未知模式"
}

// ShortToSym resolves a zones.json symbol. The US-stock synthetics are
// included even though market.All() deliberately excludes them: All() gates
// what gets AUTO-derived (ComputeAuto iterates it, so auto pivot zones stay
// on the four majors), while this map gates what a MANUAL zone may name.
// Before 2026-09-02 the two were conflated, so a hand-written SNDK/NVDA band
// was skipped by the daemon while /ops still listed it as ARMED.
//
// SUI added 2026-09-08 for discretionary ("discussed") entries. zones.json has
// carried a hand-written SUI band since the alt expansion, but with no entry
// here TriggerFault rejected it as "未知 symbol: SUI" — so the one channel that
// would actually page a manual SUI setup could never be armed. Deliberately
// NOT added to market.All(): that would enrol SUI in the engine daemon
// universe and in ComputeAuto's auto pivot zones, which is a wiring decision
// an n=8 forward record does not support. Manual zone only.
//
// SOL / LINK / HYPE / NEAR stay absent on purpose. They have bands in
// zones.json too, but they are outside the current traded roster.
var ShortToSym = map[string]market.Symbol{
	"BTC": market.BTCUSDT, "ETH": market.ETHUSDT,
	"XAU": market.XAUUSDT, "XAG": market.XAGUSDT,
	"SNDK": market.SNDKUSDT, "NVDA": market.NVDAUSDT,
	"SPCX": market.SPCXUSDT, "MSTR": market.MSTRUSDT, "APP": market.APPUSDT,
	"SUI": market.SUIUSDT,
}

var SymToShort = map[market.Symbol]string{
	market.BTCUSDT: "BTC", market.ETHUSDT: "ETH",
	market.XAUUSDT: "XAU", market.XAGUSDT: "XAG",
	market.SNDKUSDT: "SNDK", market.NVDAUSDT: "NVDA",
	market.SPCXUSDT: "SPCX", market.MSTRUSDT: "MSTR", market.APPUSDT: "APP",
	market.SUIUSDT: "SUI",
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

// Key uniquely identifies a zone for debounce state. Confirm is part of the
// identity on purpose: switching a band from touch to close-confirmed must
// re-arm it, not inherit the "already inside" state the touch mode left behind.
func Key(z Zone) string {
	return fmt.Sprintf("%s|%s|%.4f|%.4f|%s|%s", strings.ToUpper(z.Symbol), z.TF, z.Lo, z.Hi, z.Dir,
		strings.ToLower(strings.TrimSpace(z.Confirm)))
}

// AutoZoneFrom turns one structural snapshot into an armable zone. Split out
// of ComputeAuto so the arming RULES are testable without a live client —
// ComputeAuto's own body is now just fetch-and-loop.
//
// ok=false means "nothing to arm here": no active 樞紐區 (the leg was
// invalidated by a CHoCH), or a trend that is neither clean up nor clean down.
// Fading a neutral tape is the counter-trend trade with none of the context
// that justifies it.
func AutoZoneFrom(short string, tf market.Timeframe, st signal.StructureState) (Zone, bool) {
	if st.Zone == nil {
		return Zone{}, false
	}
	var dir string
	switch st.Trend {
	case signal.StructDowntrend:
		dir = "short"
	case signal.StructUptrend:
		dir = "long"
	default:
		return Zone{}, false
	}
	// The direction above comes from Trend (the 3-swing HH-HL/LH-LL context);
	// every price below comes from st.Zone (the CURRENT LEG). Those two can
	// disagree, and when they do the emitted zone described a trade nobody
	// intended: measured 2026-09-10, XAG read Trend=HH-HL uptrend with
	// Zone.Dir=LH-LL downtrend, so two armed zones went out labelled
	// dir="long" while carrying stop 68.37 ABOVE the band and target 66.05
	// BELOW it — a short's bracket under a green LONG push (zonealert.go
	// renders 🟢 + "LONG" straight off Dir). All 8 (symbol, TF) combinations
	// disagreed that morning.
	//
	// REFUSING is the fix rather than relabelling from Zone.Dir. This channel
	// is documented to "fade WITH the trend, never against" — relabelling
	// would have silently converted it into an auto-armed COUNTER-trend fader,
	// which is a strategy change, not a display fix. Disagreement is a
	// transitional state (an HH-HL classification whose latest leg already
	// turned down); there is no trend-aligned pivot zone to arm, so arm
	// nothing.
	if st.Zone.Dir != st.Trend {
		return Zone{}, false
	}
	lo, hi := st.Zone.Lo, st.Zone.Hi
	if hi < lo {
		lo, hi = hi, lo
	}
	note := fmt.Sprintf("auto %s %s 樞紐區 fade｜stop %s target %s",
		string(tf), TrendWord(st.Trend), FmtPrice(st.Zone.Invalidate), FmtPrice(st.Zone.Target))
	return Zone{
		Symbol: short, Lo: lo, Hi: hi, Dir: dir, Note: note, TF: string(tf),
		// close-in, NOT the ConfirmTouch zero value. A 樞紐區 is a 0.5–0.705
		// retrace BAND — on BTC 2h that is ~1,000 points wide — so touch
		// semantics fire the moment the mark grazes the far edge, a full
		// band-width before the level worth watching. cmd/confirmbt measured
		// the same thing as R: touch -0.41R/trade vs close-in -0.12R/trade.
		// The hand-armed zones already carry close-in; leaving the auto ones
		// on the default meant the generated half of the channel ran on the
		// mode our own A/B rejected.
		Confirm: ConfirmCloseIn,
	}, true
}

// ComputeAuto derives one zone per (symbol, TF) in a CONFIRMED trend with an
// active pivot zone, over market.All() — the core universe only, NOT the alts
// or the stock synthetics (those are forward-log symbols with no zone channel).
func ComputeAuto(ctx context.Context, client *bingx.Client, tfs []market.Timeframe) []Zone {
	var zs []Zone
	for _, sym := range market.All() {
		short := SymToShort[sym]
		for _, tf := range tfs {
			candles, err := client.Klines(ctx, sym, tf, 250)
			if err != nil || len(candles) < 60 {
				continue
			}
			z, ok := AutoZoneFrom(short, tf, signal.AnalyzeStructure(candles, 2))
			if !ok {
				continue
			}
			zs = append(zs, z)
		}
	}
	return zs
}
