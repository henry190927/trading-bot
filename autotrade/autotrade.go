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
	"log"
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
	// ExpiryBars is how long this rule's resting limits wait for a fill, in
	// closed bars of its TF. 0 = the default 6 that every rule used before
	// 2026-11-01. Copied onto each fire so replays honour it per fire.
	ExpiryBars int `json:"expiry_bars,omitempty"`
	// RiskPct overrides Config.RiskPerTradePct for this rule's fires (percent
	// of equity lost at the initial stop). 0 = the book-wide value. Exists
	// because htf-trend runs 9+ correlated positions at once with a 46R
	// backtest MDD: at the book's 1.5% that is a ~70% drawdown, at 0.25% ~12%
	// (docs/htf_trend_executor_design.md §4.1, user decision §6.1).
	RiskPct float64 `json:"risk_pct,omitempty"`

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
	MaxSameSymbolSide int `json:"max_same_symbol_side"`
	// RiskPerTradePct sizes every fire from its stop: lose this percent of
	// account equity if the stop fills (1.0 = 1%). Each rule's leverage
	// becomes a CEILING, lowered as needed to keep the stop inside the
	// liquidation guard, and margin_usdt is no longer read. 0 = the old fixed
	// margin_usdt × leverage sizing. See sizing.go.
	RiskPerTradePct float64 `json:"risk_per_trade_pct"`
	// PaperBaseUSDT and PaperSince set up the panel's paper balance: a
	// starting balance and the time it starts from (RFC3339 or YYYY-MM-DD).
	// Display only — nothing sizes off it. 0 / "" = not shown.
	PaperBaseUSDT float64 `json:"paper_base_usdt,omitempty"`
	PaperSince    string  `json:"paper_since,omitempty"`
	// StrategyCaps gives a group of strategies its OWN slot budget, keyed by
	// group name. See StrategyCap and CheckCaps. Absent = every strategy
	// shares max_concurrent_total and max_same_symbol_side, as before.
	StrategyCaps map[string]StrategyCap `json:"strategy_caps,omitempty"`
	Rules        []Rule                 `json:"rules"`
}

// StrategyCap is a separate budget for a group of strategies (user decision
// 2026-11-15: htf-trend gets its own slots rather than competing with the
// fade rules for max_concurrent_total — its positions sit for weeks and
// would starve the fades, and the fades would make it skip breakouts its
// backtest takes).
//
// A position whose strategy is in a group counts ONLY against that group's
// caps: it is invisible to max_concurrent_total and max_same_symbol_side,
// and the group's caps do not see positions outside it. The daily-loss halt
// and max_margin_total_usdt stay book-wide — they protect the account, not a
// strategy's share of it. Every count is 0 = UNLIMITED, like the other caps.
type StrategyCap struct {
	// Strategies in the group. Empty = the group key itself is the one
	// strategy, so {"htf-trend": {...}} needs no list.
	Strategies []string `json:"strategies,omitempty"`
	// MaxConcurrent caps open positions in the group.
	MaxConcurrent int `json:"max_concurrent,omitempty"`
	// MaxSameSide caps open positions in the group on one side, ACROSS
	// symbols (htf-trend's backtest had 9 of 9 symbols long at once). Not
	// per symbol like max_same_symbol_side: the risk here is one market move
	// hitting every leg. Slots go first-come in rule order, which is a
	// selection rule — the value comes from a pre-registered trendbt run
	// (docs/htf_trend_design.md §7), not from a concurrency count.
	MaxSameSide int `json:"max_same_side,omitempty"`
}

// capGroup returns the StrategyCaps group a strategy belongs to, or "".
// A strategy listed in two groups takes the alphabetically first, so the
// answer does not depend on map iteration order.
func (c Config) capGroup(strategy string) string {
	best := ""
	for g, sc := range c.StrategyCaps {
		in := len(sc.Strategies) == 0 && g == strategy
		for _, s := range sc.Strategies {
			if s == strategy {
				in = true
			}
		}
		if in && (best == "" || g < best) {
			best = g
		}
	}
	return best
}

// EffectiveRiskPct is the percent of equity a rule's fire risks: the rule's
// own risk_pct when set, else the book-wide risk_per_trade_pct. 0 = the
// fixed margin_usdt × leverage sizing.
func (c Config) EffectiveRiskPct(r Rule) float64 {
	if r.RiskPct > 0 {
		return r.RiskPct
	}
	return c.RiskPerTradePct
}

// NeedsEquity reports whether any enabled rule sizes from equity, so the
// executor reads the balance only when something will use it.
func (c Config) NeedsEquity() bool {
	for _, r := range c.Rules {
		if r.Enabled && c.EffectiveRiskPct(r) > 0 {
			return true
		}
	}
	return false
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
	for _, name := range c.UnknownStrategies() {
		log.Printf("autotrade: rule names unknown strategy %q — it will never fire. Known: %s",
			name, strings.Join(KnownStrategies, " "))
	}
	for _, r := range c.Rules {
		if want := TrailStrategyTF(r.Strategy); want != "" && r.TF != want {
			log.Printf("autotrade: %s rule %s has tf %q — it is pre-registered for %s only and will never fire",
				r.Strategy, r.Symbol, r.TF, want)
		}
	}
	return c
}

// KnownStrategies is every value autostrat.EvalAutoTrigger dispatches on. A
// rule naming anything else falls through that switch to an empty Trigger, so
// it loads, renders on the /ops panel, and silently never fires — which looks
// exactly like a rule whose trigger conditions have not been met.
//
// Kept here rather than in autostrat because autostrat imports autotrade; a
// test in autostrat pins the two lists together so adding a case there without
// adding it here fails the build gate rather than the next hand-edit.
var KnownStrategies = []string{"range-edge", "engine", "sweep-reject", "htf-snr", "struct-momentum", "vp-node", "htf-trend", "htf-trend-1d"}

// trailStrategyTF is every strategy whose fires exit on a trailing stop, and
// the one TF each is pre-registered for (docs/htf_trend_design.md §3). Two
// strategy NAMES rather than one name with the TF from the rule: the slot key
// everywhere is Symbol|Strategy (B11), so a 4h and a 1d rule on one symbol
// under one name would block each other, and re-keying every slot map on TF
// would change every existing strategy's dedup (executor design §2.6).
var trailStrategyTF = map[string]string{"htf-trend": "4h", "htf-trend-1d": "1d"}

// IsTrailStrategy reports whether a strategy exits on a trailing stop. Used
// before a trigger exists (kline depth, full-log slot lookup); after one, the
// fire's own TrailM is the authority.
func IsTrailStrategy(name string) bool { _, ok := trailStrategyTF[name]; return ok }

// TrailStrategyTF is the TF a trail strategy is pre-registered for ("" when
// name is not one). A rule on any other TF never fires (autostrat refuses it).
func TrailStrategyTF(name string) string { return trailStrategyTF[name] }

// UnknownStrategies returns the rule strategy names this build cannot execute.
func (c Config) UnknownStrategies() []string {
	known := map[string]bool{}
	for _, k := range KnownStrategies {
		known[k] = true
	}
	seen, out := map[string]bool{}, []string(nil)
	for _, r := range c.Rules {
		n := strings.TrimSpace(r.Strategy)
		if n == "" || known[n] || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// LiveArmed reports whether REAL orders are permitted: config master on, paper
// off, AND the env kill-switch set. Any one off → paper/no-op.
func (c Config) LiveArmed() bool {
	return c.Enabled && !c.Paper && strings.EqualFold(strings.TrimSpace(os.Getenv("AUTOTRADE_ENABLED")), "true")
}
