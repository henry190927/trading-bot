// Package autotrade holds the config for the auto-executor (docs/auto_executor_design.md).
// The executor (cmd/monitor/autoexec.go) reads autotrade.json each tick (hot-reload)
// and, ONLY when every safety gate passes, places bracket orders per rule.
//
// SAFETY: two independent kill switches — Config.Enabled AND env AUTOTRADE_ENABLED
// must BOTH be true, and Config.Paper must be false, before any REAL order. Defaults
// are the safe state (disabled + paper).
package autotrade

import (
	"encoding/json"
	"os"
	"strings"
)

// Rule is one per-symbol auto-trade setup the user pre-fills once.
type Rule struct {
	Enabled      bool    `json:"enabled"`
	Symbol       string  `json:"symbol"`   // BTC / ETH / SOL / ...
	Strategy     string  `json:"strategy"` // "range-edge" | "zone-retrace" | "struct-momentum"
	TF           string  `json:"tf"`       // "1h" etc
	Side         string  `json:"side"`     // "long" | "short" | "auto"
	MarginUSDT   float64 `json:"margin_usdt"`
	Leverage     int     `json:"leverage"`
	StopPct      float64 `json:"stop_pct"` // buffer beyond box/structure for the stop; ONLY the range-edge path reads it — the engine/sweep-reject/htf-snr paths use their own Plan.StopLoss
	CooldownBars int     `json:"cooldown_bars_after_stop"`

	// REMOVED 2026-09-02, do not re-add without an implementation:
	//   MaxConcurrent  (per-symbol) — had zero consumers anywhere. One position
	//     per RULE is already enforced structurally (live: client.OpenPositions,
	//     paper: paperBlocked), so the value 1 matched behaviour by accident and
	//     any other value would have done nothing.
	//   RequireAligned — had zero consumers anywhere, while every rule set it
	//     true, so it read as a TF-alignment safety filter and was decoration.
	//     Note that gating range-edge on trend/alignment was A/B-tested on
	//     2026-08-27 and made results WORSE in every window, so this is not a
	//     missing feature — it is a knob that should never have been declared.
}

// Config is the whole auto-executor config (autotrade.json).
type Config struct {
	Enabled            bool    `json:"enabled"`               // master (also needs env AUTOTRADE_ENABLED)
	Paper              bool    `json:"paper"`                 // true = mock only, NO real orders
	MaxConcurrentTotal int     `json:"max_concurrent_total"`  // across all symbols
	MaxMarginTotalUSDT float64 `json:"max_margin_total_usdt"` // total capital-at-risk cap
	DailyLossHaltR     float64 `json:"daily_loss_halt_r"`     // kill-switch: halt all if today's R <= this
	// MaxSameSymbolSide caps positions sharing a (symbol, direction). 0 =
	// UNLIMITED, matching the other caps, so an existing file keeps today's
	// behaviour. 1 = never repeat an opinion already on the book.
	MaxSameSymbolSide int    `json:"max_same_symbol_side"`
	Rules             []Rule `json:"rules"`
}

// Path is the on-disk config (env AUTOTRADE_PATH or the default).
func Path() string {
	if p := strings.TrimSpace(os.Getenv("AUTOTRADE_PATH")); p != "" {
		return p
	}
	return "/opt/trading/autotrade.json"
}

// Default is the SAFE fallback when no file exists: fully disabled + paper.
func Default() Config {
	return Config{Enabled: false, Paper: true, MaxConcurrentTotal: 2, MaxMarginTotalUSDT: 100, DailyLossHaltR: -3.0}
}

// Load reads autotrade.json; a missing/invalid file returns the safe default
// (disabled) so the executor never trades on a parse error.
func Load() Config {
	b, err := os.ReadFile(Path())
	if err != nil {
		return Default()
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Default()
	}
	return c
}

// LiveArmed reports whether REAL orders are permitted: config master on, paper
// off, AND the env kill-switch set. Any one off → paper/no-op.
func (c Config) LiveArmed() bool {
	return c.Enabled && !c.Paper && strings.EqualFold(strings.TrimSpace(os.Getenv("AUTOTRADE_ENABLED")), "true")
}
