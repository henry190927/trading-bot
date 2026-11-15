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
	// newest first, cap
	out := make([]PaperFire, 0, limit)
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, all[i])
	}
	return out
}
