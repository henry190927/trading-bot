package main

// AMD (吸籌 / 操縱 / 派發) as a VARIANT of sweep-reject, not a new strategy.
//
// Roughly seventy percent of the framework is already here under other names:
// the accumulation range is what computeRange and range-edge work on, and the
// manipulation leg — a sweep of resting liquidity that closes back through the
// level — is precisely what sweep-reject detects. The one genuinely new
// ingredient is TIME ANCHORING: classic PO3 accumulates in the Asian session,
// manipulates at the London open and expands into New York, whereas
// sweep-reject is price-anchored and fires at any hour.
//
// So this swaps exactly two things and leaves everything else — stop placement,
// R multiple, DedupFires, EvaluateFire — identical to the baseline:
//
//   1. the level swept is the ASIAN RANGE boundary, not any EQH/EQL pool
//   2. the sweep must happen inside the London→NY window
//
// That makes the A/B isolate the session anchoring itself. It also means the
// comparison has a real baseline (sweep-reject, +4.00R live over 55 positions
// and positive in all three backtest windows) rather than being a new idea
// scored against nothing.
//
// Session boundaries in UTC, matching the chart's own open lines
// (cmd/web/handlers.go computes the London open as day+7h):
//   Asia   00:00–07:00   accumulation, the range
//   London 07:00–13:00   manipulation, where the sweep must land
//   NY     13:00–20:00   expansion, still eligible so a late sweep is not lost
//
// RESULT 2026-09-30 — FAILS, 0 of 3 windows, both TP variants. Kept behind a
// default-off flag because the negative result is the useful part.
//
//	BTC+ETH+SOL+SUI 1h   60d      90d      120d
//	baseline           -1.00   +30.00   +50.00   (n 242 / 340 / 455)
//	AMD               -19.00   -13.00   -42.00   (n 195 / 312 / 422)
//	AMD + edge TP     -11.68    -9.28   -43.10
//
// Trade counts are close, so this is not a sample-size effect — it is worse per
// trade. The likely reason, and it is a useful one: what makes sweep-reject
// work is not WHEN the sweep happens but WHAT is swept. An EQH/EQL pool is a
// level price has tested repeatedly, so resting orders have actually
// accumulated there. An Asian-session boundary is a seven-hour extreme — one
// touch, no pool. Anchoring to the session trades a weaker level and the timing
// does not compensate.
//
// Consistent with the open-gate A/B (2026-09-24), which also made things worse
// in all three windows: clock- and open-price-based gates keep failing here
// while level-quality keeps mattering.

import (
	"time"

	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/market"
)

const (
	asiaStartUTC = 0
	asiaEndUTC   = 7  // exclusive: the Asian range is [00:00, 07:00)
	sweepEndUTC  = 20 // last hour a manipulation sweep is accepted
)

// asianRange returns the high and low of the current UTC day's Asian session,
// using only bars at or before i.
//
// ok is false until the session has CLOSED (bar i is at or past 07:00 UTC) and
// the day actually has Asian bars. Reading a partial range would be
// look-ahead's quieter cousin: the boundary would still be moving when the
// sweep is judged against it, so a "sweep" could be a bar that simply extended
// a range nobody had finished drawing.
func asianRange(cs []market.Candle, i int) (hi, lo float64, ok bool) {
	bar := cs[i].OpenTime.UTC()
	if bar.Hour() < asiaEndUTC {
		return 0, 0, false // still inside the Asian session
	}
	y, m, d := bar.Date()
	for j := i; j >= 0; j-- {
		t := cs[j].OpenTime.UTC()
		if yy, mm, dd := t.Date(); yy != y || mm != m || dd != d {
			break // walked off the start of the day
		}
		if t.Hour() < asiaStartUTC || t.Hour() >= asiaEndUTC {
			continue
		}
		if !ok {
			hi, lo, ok = cs[j].High, cs[j].Low, true
			continue
		}
		if cs[j].High > hi {
			hi = cs[j].High
		}
		if cs[j].Low < lo {
			lo = cs[j].Low
		}
	}
	return hi, lo, ok && hi > lo
}

// inSweepWindow reports whether a manipulation sweep on this bar counts.
func inSweepWindow(c market.Candle) bool {
	h := c.OpenTime.UTC().Hour()
	return h >= asiaEndUTC && h < sweepEndUTC
}

// genAMDFires mirrors genSweepFires bar for bar; only the level source and the
// time gate differ.
func genAMDFires(cs []market.Candle, atr []float64, short string, bufATR, rMult float64, rangeTP bool) []autotrade.PaperFire {
	var out []autotrade.PaperFire
	for i := 60; i < len(cs); i++ {
		bar, a := cs[i], atr[i]
		if a <= 0 || !inSweepWindow(bar) {
			continue
		}
		hi, lo, ok := asianRange(cs, i)
		if !ok {
			continue
		}
		// Sweep the range HIGH and close back inside → the trap was upward.
		if bar.High > hi && bar.Close < hi {
			stop := bar.High + bufATR*a
			risk := stop - bar.Close
			if risk <= 0 {
				continue
			}
			tp := bar.Close - rMult*risk
			if rangeTP && lo < bar.Close-risk { // the opposite edge, if it is at least 1R away
				tp = lo
			}
			out = append(out, autotrade.PaperFire{
				Time: bar.CloseTime, Symbol: short, TF: "1h", Strategy: "sweep-reject", Side: "short",
				Market: true, Entry: bar.Close, Stop: stop, TP: tp,
			})
			continue
		}
		if bar.Low < lo && bar.Close > lo {
			stop := bar.Low - bufATR*a
			risk := bar.Close - stop
			if risk <= 0 {
				continue
			}
			tp := bar.Close + rMult*risk
			if rangeTP && hi > bar.Close+risk {
				tp = hi
			}
			out = append(out, autotrade.PaperFire{
				Time: bar.CloseTime, Symbol: short, TF: "1h", Strategy: "sweep-reject", Side: "long",
				Market: true, Entry: bar.Close, Stop: stop, TP: tp,
			})
		}
	}
	return out
}

var _ = time.Hour
