package autotrade

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"time"
)

// PaperFire is one paper-mode auto-trade the executor WOULD have placed. Appended
// to a JSONL log so the /ops autotrade panel can show what the executor is doing
// without any real orders.
type PaperFire struct {
	Time     time.Time `json:"time"`
	Symbol   string    `json:"symbol"`
	TF       string    `json:"tf"`       // timeframe of the box/trigger — needed to replay outcome
	Strategy string    `json:"strategy"` // "range-edge" | "engine" — which A/B strategy fired this
	Side     string    `json:"side"`
	Market   bool      `json:"market"` // true = marketable entry (fills at fire), false = resting limit (pending until touched)
	Entry    float64   `json:"entry"`
	Stop     float64   `json:"stop"`
	TP       float64   `json:"tp"`
	Qty      float64   `json:"qty"`
	Margin   float64   `json:"margin"`
	Lev      int       `json:"lev"`
	Why      string    `json:"why"`
	Score    float64   `json:"score,omitempty"` // validator structural-fit score /100 at fire time (0 = not scored / legacy)
	Live     bool      `json:"live"`            // false = paper, true = real order placed
	// Sizing inputs at fire time (2026-10-16). Equity is recorded because it
	// was recorded nowhere before, so no row could say what share of the
	// account was at risk. Both 0 on older rows and in fixed-margin mode
	// (RiskUSDT is still set there).
	RiskUSDT float64 `json:"risk_usdt,omitempty"`
	Equity   float64 `json:"equity,omitempty"`
	// ExpiryBars is how many closed bars a resting limit waits before it
	// counts as a no-fill, copied from the rule at fire time (2026-11-01).
	// 0 = the caller's default (6 everywhere), which is every older row.
	ExpiryBars int `json:"expiry_bars,omitempty"`

	// Trail exits (htf-trend, 2026-11-15). TrailM > 0 means the fire has no
	// take-profit and exits on the ratcheted M-bar opposite channel; it is
	// scored by EvaluateTrailFire, recomputed from the candles (Resolve
	// dispatches). 0 = fixed bracket, which is every older row.
	TrailM int `json:"trail_m,omitempty"`
	// BarTime is the signal bar's CloseTime, so a replay finds the bar the
	// fire was taken on instead of inferring it from Time.
	BarTime time.Time `json:"bar_time,omitzero"`
	// CooldownBars is the rule's post-stop cooldown at fire time, so a replay
	// stops guessing one value for every rule (B6). Only trail fires are
	// replayed with it today; fixed-bracket replays keep the panel's
	// max-across-rules convention so their totals do not move.
	CooldownBars int `json:"cooldown_bars,omitempty"`
}

// Expiry returns the fire's own limit expiry, or def when it carries none.
func (f PaperFire) Expiry(def int) int {
	if f.ExpiryBars > 0 {
		return f.ExpiryBars
	}
	return def
}

// PaperLogPath is the on-disk JSONL store (env AUTOTRADE_PAPER_LOG or default).
func PaperLogPath() string {
	if p := strings.TrimSpace(os.Getenv("AUTOTRADE_PAPER_LOG")); p != "" {
		return p
	}
	return "/opt/trading/autotrade_paper.jsonl"
}

// AppendFire appends one fire to the paper log (best-effort; never fatal).
func AppendFire(pf PaperFire) {
	f, err := os.OpenFile(PaperLogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	b, err := json.Marshal(pf)
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
}

// ReadFires returns the most recent `limit` fires (newest first).
func ReadFires(limit int) []PaperFire {
	all := readAllFires()
	// newest first, cap
	out := make([]PaperFire, 0, min(limit, len(all)))
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, all[i])
	}
	return out
}

// readAllFires is the whole log, oldest first.
func readAllFires() []PaperFire {
	f, err := os.Open(PaperLogPath())
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var all []PaperFire
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var pf PaperFire
		if json.Unmarshal([]byte(line), &pf) == nil {
			all = append(all, pf)
		}
	}
	_ = sc.Err()
	return all
}

// ReadFiresWithOpenTrails is ReadFires(limit) plus, appended after the
// window (so still newest-first), the newest fire of every trail rule whose
// newest fire has scrolled out of it (B8). A trail position can stay open
// for weeks while other rules keep writing rows; at a fixed depth it would
// silently leave the open book and its rule's slot, and the next breakout
// would be re-entered while the first is still on. Only TRAIL fires are
// added: a fixed-bracket row older than the window resolved long ago, and
// adding it would change every existing number. With no trail fire in the
// log this returns exactly ReadFires(limit).
func ReadFiresWithOpenTrails(limit int) []PaperFire {
	return withOpenTrails(readAllFires(), limit)
}

func withOpenTrails(all []PaperFire, limit int) []PaperFire {
	out := make([]PaperFire, 0, min(limit, len(all)))
	inWin := map[string]bool{}
	i := len(all) - 1
	for ; i >= 0 && len(out) < limit; i-- {
		out = append(out, all[i])
		inWin[all[i].Symbol+"|"+all[i].Strategy] = true
	}
	for ; i >= 0; i-- {
		f := all[i]
		k := f.Symbol + "|" + f.Strategy
		if inWin[k] {
			continue
		}
		inWin[k] = true // only the newest per rule: the slot is "newest fire per rule"
		if f.IsTrail() {
			out = append(out, f)
		}
	}
	return out
}

// NewestFire is the newest fire for (symbol, strategy) in the WHOLE log, or
// false when the rule never fired. paperBlocked uses it for trail rules,
// whose open position may be older than any fixed read depth (B8).
func NewestFire(symbol, strategy string) (PaperFire, bool) {
	all := readAllFires()
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Symbol == symbol && all[i].Strategy == strategy {
			return all[i], true
		}
	}
	return PaperFire{}, false
}
