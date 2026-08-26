package autotrade

import (
	"time"

	"myFirstGo/trading-bot/market"
)

// OutcomeStatus classifies what happened to a paper fire once its closed candles
// are replayed forward.
type OutcomeStatus string

const (
	OutPending OutcomeStatus = "pending" // resting limit, entry not yet touched, expiry not reached
	OutNoFill  OutcomeStatus = "no-fill" // limit expired without ever being touched
	OutOpen    OutcomeStatus = "open"    // filled but neither stop nor tp hit yet
	OutTP      OutcomeStatus = "tp"      // take-profit reached
	OutStop    OutcomeStatus = "stop"    // stop-loss reached
)

// Outcome is the replayed result of one PaperFire.
type Outcome struct {
	Status     OutcomeStatus
	NetR       float64   // realized R: tp = reward:risk (+), stop = -1, no-fill/open = 0
	UnrealR    float64   // unrealized R for an OPEN trade, marked to the last close (0 otherwise)
	FillPrice  float64   // = entry when filled
	ExitPrice  float64   // stop/tp level, or last close if still open
	FilledAt   time.Time
	ExitAt     time.Time
	BarsToFill int // closed bars from fire → fill
	BarsHeld   int // closed bars from fill → exit (or → last bar if open)
}

// EvaluateFire replays the closed candles that occur after the fire to classify
// the paper trade. Fill depends on f.Market:
//
//   - Marketable entry (f.Market: range-edge box @ spot, or an engine plan already
//     on the fillable side of price) fills AT the fire — it is OPEN immediately,
//     even before the first bar closes.
//   - Resting limit (!f.Market: an engine sweep level away from spot) fills only
//     when a later bar trades to entry — Low<=entry for a long, High>=entry for a
//     short. Until touched it is PENDING while fewer than expiryBars have closed
//     since the fire, and NO-FILL once expiryBars elapse without a touch (the order
//     is treated as cancelled/stale). This separates "still waiting to fill" from
//     "the setup came and went".
//
// After fill it scans forward for stop/tp. Same-bar ambiguity resolves to the
// stop (conservative). candles must be ascending by time; bars at/before the fire
// are ignored so there is no look-ahead into the bar the order was placed on.
func EvaluateFire(f PaperFire, candles []market.Candle, expiryBars int) Outcome {
	long := f.Side == "long"
	var risk float64
	if long {
		risk = f.Entry - f.Stop
	} else {
		risk = f.Stop - f.Entry
	}
	if risk <= 0 {
		return Outcome{Status: OutOpen} // malformed bracket — can't score
	}

	// Only bars that closed after the order existed.
	var fwd []market.Candle
	for _, c := range candles {
		if c.CloseTime.After(f.Time) {
			fwd = append(fwd, c)
		}
	}

	// --- fill ---
	// A marketable entry (range-edge box, or an engine plan already on the fillable
	// side of spot) fills AT the fire — no waiting for a bar to touch it. A resting
	// limit (engine sweep level away from spot) fills only when a bar trades to it,
	// and is pending until then.
	fillIdx := 0
	filledAt := f.Time
	barsToFill := 0
	if !f.Market {
		if len(fwd) == 0 {
			return Outcome{Status: OutPending} // resting limit, no closed bar yet
		}
		fillIdx = -1
		limit := min(expiryBars, len(fwd))
		for i := range limit {
			c := fwd[i]
			if long && c.Low <= f.Entry {
				fillIdx = i
				break
			}
			if !long && c.High >= f.Entry {
				fillIdx = i
				break
			}
		}
		if fillIdx < 0 {
			// Not touched yet: still resting if the expiry window hasn't elapsed,
			// otherwise treat the order as expired/cancelled (no-fill).
			if len(fwd) < expiryBars {
				return Outcome{Status: OutPending}
			}
			return Outcome{Status: OutNoFill}
		}
		filledAt = fwd[fillIdx].CloseTime
		barsToFill = fillIdx + 1
	}

	// Marketable entry but no closed bar yet → filled and running, nothing to mark.
	if len(fwd) == 0 {
		return Outcome{Status: OutOpen, FillPrice: f.Entry, ExitPrice: f.Entry, FilledAt: filledAt}
	}

	out := Outcome{
		Status:     OutOpen,
		FillPrice:  f.Entry,
		FilledAt:   filledAt,
		BarsToFill: barsToFill,
		ExitPrice:  fwd[len(fwd)-1].Close,
	}

	// --- exit scan (from the fill bar onward, stop checked before tp) ---
	for i := fillIdx; i < len(fwd); i++ {
		c := fwd[i]
		if long {
			if c.Low <= f.Stop {
				out.Status, out.ExitPrice, out.NetR = OutStop, f.Stop, (f.Stop-f.Entry)/risk
				out.ExitAt, out.BarsHeld = c.CloseTime, i-fillIdx
				return out
			}
			if c.High >= f.TP {
				out.Status, out.ExitPrice, out.NetR = OutTP, f.TP, (f.TP-f.Entry)/risk
				out.ExitAt, out.BarsHeld = c.CloseTime, i-fillIdx
				return out
			}
		} else {
			if c.High >= f.Stop {
				out.Status, out.ExitPrice, out.NetR = OutStop, f.Stop, (f.Entry-f.Stop)/risk
				out.ExitAt, out.BarsHeld = c.CloseTime, i-fillIdx
				return out
			}
			if c.Low <= f.TP {
				out.Status, out.ExitPrice, out.NetR = OutTP, f.TP, (f.Entry-f.TP)/risk
				out.ExitAt, out.BarsHeld = c.CloseTime, i-fillIdx
				return out
			}
		}
	}
	// still open: unrealized R marked-to-last-close, but NetR stays 0 (not realized).
	out.BarsHeld = len(fwd) - 1 - fillIdx
	mark := out.ExitPrice // = last close
	if long {
		out.UnrealR = (mark - f.Entry) / risk
	} else {
		out.UnrealR = (f.Entry - mark) / risk
	}
	return out
}

// EvaluateFireLive runs the closed-bar EvaluateFire and then overlays the LIVE
// mark price so the panel doesn't lag a full bar. Closed bars stay authoritative
// for history (they catch a wick between fill and now); the live price refines
// only the CURRENT state, exactly like the dashboard's open-position R cards:
//
//   - open trade → unrealized R is marked to the live price (not last close), and
//     if live has already crossed stop/tp the trade is resolved now (a real bracket
//     tp/stop triggers intrabar, so this is more faithful than waiting for close);
//   - pending limit → if live has reached entry it fills now and becomes open.
//
// Resolved (tp/stop/no-fill) outcomes are left untouched — live can't un-settle
// them. livePrice<=0 falls back to the pure closed-bar result.
func EvaluateFireLive(f PaperFire, candles []market.Candle, expiryBars int, livePrice float64) Outcome {
	out := EvaluateFire(f, candles, expiryBars)
	if livePrice <= 0 {
		return out
	}
	long := f.Side == "long"
	var risk float64
	if long {
		risk = f.Entry - f.Stop
	} else {
		risk = f.Stop - f.Entry
	}
	if risk <= 0 {
		return out
	}

	// A resting limit the live price has since reached fills now.
	if out.Status == OutPending {
		filled := (long && livePrice <= f.Entry) || (!long && livePrice >= f.Entry)
		if !filled {
			return out
		}
		out.Status, out.FillPrice = OutOpen, f.Entry
	}
	if out.Status != OutOpen {
		return out
	}

	// Mark to live: resolve if live has crossed stop/tp, else unrealized R.
	if long {
		switch {
		case livePrice <= f.Stop:
			out.Status, out.ExitPrice, out.NetR, out.UnrealR = OutStop, f.Stop, (f.Stop-f.Entry)/risk, 0
		case livePrice >= f.TP:
			out.Status, out.ExitPrice, out.NetR, out.UnrealR = OutTP, f.TP, (f.TP-f.Entry)/risk, 0
		default:
			out.ExitPrice, out.UnrealR = livePrice, (livePrice-f.Entry)/risk
		}
	} else {
		switch {
		case livePrice >= f.Stop:
			out.Status, out.ExitPrice, out.NetR, out.UnrealR = OutStop, f.Stop, (f.Entry-f.Stop)/risk, 0
		case livePrice <= f.TP:
			out.Status, out.ExitPrice, out.NetR, out.UnrealR = OutTP, f.TP, (f.Entry-f.TP)/risk, 0
		default:
			out.ExitPrice, out.UnrealR = livePrice, (f.Entry-livePrice)/risk
		}
	}
	return out
}

// Position is one deduped position: the fire that opened it, its outcome, and how
// many raw re-fires of the same still-live setup were absorbed into it.
type Position struct {
	Fire    PaperFire
	Outcome Outcome
	Absorbed int
}

// DedupFires collapses the raw fire log into realistic positions: one position per
// (symbol, strategy) at a time. A later fire for the same rule is a NEW position
// only once the prior one has left the book — resolved (tp/stop, then a stop adds
// cooldownBars), or a limit that expired (no-fill after expiryBars). While the
// prior fire is still open/pending, subsequent fires are absorbed (they are the
// same setup re-evaluated each bar, not new trades). `fires` must be oldest-first;
// resolve(f) returns the outcome for a fire (the caller supplies it because it
// needs klines).
func DedupFires(fires []PaperFire, expiryBars, cooldownBars int, barDur time.Duration, resolve func(PaperFire) Outcome) []Position {
	openUntil := map[string]time.Time{}
	curIdx := map[string]int{}
	var out []Position
	for _, f := range fires {
		key := f.Symbol + "|" + f.Strategy
		if u, ok := openUntil[key]; ok && f.Time.Before(u) {
			if i, ok := curIdx[key]; ok {
				out[i].Absorbed++
			}
			continue
		}
		oc := resolve(f)
		out = append(out, Position{Fire: f, Outcome: oc})
		curIdx[key] = len(out) - 1

		var hold time.Time
		switch oc.Status {
		case OutTP:
			hold = oc.ExitAt
		case OutStop:
			hold = oc.ExitAt.Add(time.Duration(cooldownBars) * barDur)
		case OutNoFill:
			hold = f.Time.Add(time.Duration(expiryBars) * barDur)
		default: // open / pending — still in the position or waiting to fill
			hold = f.Time.Add(1_000_000 * time.Hour)
		}
		openUntil[key] = hold
	}
	return out
}

// Summary aggregates outcomes across many fires for the panel header.
type Summary struct {
	Total   int
	Filled  int
	Pending int
	NoFill  int
	Open    int
	TP      int
	Stop    int
	NetR    float64 // sum of realized R (tp + stop)
	UnrealR float64 // sum of unrealized R across open trades (mark-to-last-close)
	WinRate float64 // tp / (tp + stop), 0 if none resolved
	FillPct float64 // filled / (filled + no-fill); pending excluded (undetermined)
}

// Summarize folds a slice of outcomes into a Summary.
func Summarize(outs []Outcome) Summary {
	var s Summary
	s.Total = len(outs)
	for _, o := range outs {
		switch o.Status {
		case OutPending:
			s.Pending++
		case OutNoFill:
			s.NoFill++
		case OutOpen:
			s.Filled++
			s.Open++
			s.UnrealR += o.UnrealR
		case OutTP:
			s.Filled++
			s.TP++
			s.NetR += o.NetR
		case OutStop:
			s.Filled++
			s.Stop++
			s.NetR += o.NetR
		}
	}
	if resolved := s.TP + s.Stop; resolved > 0 {
		s.WinRate = float64(s.TP) / float64(resolved)
	}
	if det := s.Filled + s.NoFill; det > 0 {
		s.FillPct = float64(s.Filled) / float64(det)
	}
	return s
}
