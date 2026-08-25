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
// the paper trade. It models the entry as a resting limit at f.Entry: for a long
// it fills when a later bar trades down to entry (Low <= entry), for a short when
// a bar trades up to it (High >= entry). Because range-edge entries sit at the
// spot price at fire time, the fill is usually immediate; if no bar touches entry
// within fillWindow bars the trade is a no-fill (price ran away).
//
// A resting limit that hasn't been touched yet is PENDING while fewer than
// expiryBars have closed since the fire, and a NO-FILL once expiryBars elapse
// without a touch (the order is treated as cancelled/stale). This separates
// "still waiting to fill" (e.g. an engine sweep-high short resting above spot)
// from "the setup came and went".
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
	if len(fwd) == 0 {
		return Outcome{Status: OutPending} // just fired, no closed bar yet — limit is resting
	}

	// --- fill ---
	fillIdx := -1
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

	out := Outcome{
		Status:     OutOpen,
		FillPrice:  f.Entry,
		FilledAt:   fwd[fillIdx].CloseTime,
		BarsToFill: fillIdx + 1,
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
