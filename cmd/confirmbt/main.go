// Command confirmbt A/B-tests the ENTRY half of the pivot-zone-fade queue item:
// take the fade the moment price reaches the 樞紐區, or wait for a bar to CLOSE
// back out of the zone in the trade direction first.
//
//	TOUCH    enter when price first reaches the zone's near edge
//	CONFIRM  enter at the close of the first later bar that closes back OUT of
//	         the zone in the trade direction (within --confirm-bars, else no trade)
//
// Both arms use the SAME structural stop (the zone's Invalidate) and the SAME
// target price (the zone's 1:1 Target), so ENTRY is the only variable. That
// control is not optional: cmd/shadowbt showed that letting each arm derive its
// target from its own risk hands the win to whichever arm enters closer to its
// stop, mechanically, regardless of entry quality
// (feedback-ab-hold-target-fixed).
//
// The STOP half of the same queue item is already settled and is NOT re-tested
// here: --stop-buffer-r, --slide-offset-pct and --sm-stop-buffer were each
// A/B'd and each was worse in every window. Stop-on-the-invalidate wins.
//
// The decomposition that actually answers the question is the third block of
// output: split the TOUCH arm by whether that event LATER confirmed. Confirming
// is only worth its worse entry price if the never-confirmed subset — the
// fakeouts it declines — is genuinely losing.
//
// ── RESULT 2026-09-04: confirmation is a large, robust improvement — and
//
//	   pivot-zone-fade is still not shippable ────────────────────────────────
//
//		config                      TOUCH R/trd   CONFIRM R/trd   delta
//		60d                            -0.320        -0.098       +0.222
//		90d                            -0.411        -0.119       +0.292
//		120d                           -0.401        -0.063       +0.338
//		90d, 2R cap (not zone 1:1)     -0.242        -0.129       +0.113
//		90d, confirm within 2 bars     -0.411        -0.080       +0.331
//		90d, confirm within 8 bars     -0.411        -0.158       +0.253
//
// CONFIRM is better in EVERY window, at both nearby confirm-bar values, and
// under a different target rule. Win rate 15% -> 29% on the same stop and the
// same target. 7 of 8 symbols individually (LINK the exception).
//
// BUT BOTH ARMS ARE NEGATIVE EVERYWHERE. The best CONFIRM figure is -0.063
// R/trade. Confirmation makes the fade much LESS BAD; it does not make it
// profitable, so NOTHING SHIPS — the gate is +R, not "less negative"
// (feedback-strategy-changes).
//
// WHY it improves, attributed. Splitting TOUCH's own R by whether the event
// later confirmed is remarkably stable across windows:
//
//	                    60d      90d     120d
//	later CONFIRMED   -0.070   -0.173   -0.162
//	never confirmed   -0.897   -0.945   -0.941
//
// The never-confirmed group is the fakeout group — price reaches the zone and
// keeps going — and it is reliably catastrophic. Confirmation declines exactly
// that group. Attribution on the 90d run:
//
//	selection (declining never-confirmed)   -0.411 -> -0.173   ~75% of the gain
//	entry / stop-distance                   -0.173 -> -0.119   ~25%
//
// That second component is the mechanism cmd/shadowbt isolated, running the
// other way: CONFIRM enters FURTHER from the invalidate, so it has more room
// and stops out less often. Holding stop and target fixed does not remove that
// effect — it only stops it being mistaken for entry quality. The
// decomposition is what separates the two, and selection does most of the
// work, which is what confirmation actually IS.
//
// USABLE CONCLUSION even though nothing ships: if a pivot-zone-fade is taken
// DISCRETIONARILY, wait for the close back out of the zone. Touching the zone
// and entering runs -0.41/trade; waiting runs -0.12/trade; and the events that
// never confirm run -0.94/trade, which is the group to avoid outright.
//
// Usage:
//
//	go run ./cmd/confirmbt --days 90        (also 60, 120)
//	go run ./cmd/confirmbt --days 90 --confirm-bars 2 --r-cap 2
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"myFirstGo/trading-bot/autotrade"
	"myFirstGo/trading-bot/bingx"
	"myFirstGo/trading-bot/config"
	"myFirstGo/trading-bot/market"
	"myFirstGo/trading-bot/signal"
)

// zoneEvent is one entry of price into a directional 樞紐區.
type zoneEvent struct {
	Bar        int
	Side       string  // "long" (uptrend pullback) | "short" (downtrend pullback)
	Near       float64 // the zone edge price reaches first — the TOUCH entry
	Invalidate float64
	Target     float64
	// ConfirmBar/ConfirmPx are set when a later bar closed back out of the zone
	// in the trade direction; -1 / 0 when it never confirmed.
	ConfirmBar int
	ConfirmPx  float64
}

func main() {
	config.LoadDotEnv()
	days := flag.Int("days", 90, "history window in days")
	tfStr := flag.String("tf", "1h", "timeframe")
	confirmBars := flag.Int("confirm-bars", 4, "how many bars the CONFIRM arm waits for a close back out of the zone before giving up")
	holdBars := flag.Int("hold", 24, "max bars to hold (EvaluateFire expiry window)")
	rCap := flag.Float64("r-cap", 0, "if >0, cap the target at this R multiple instead of using the zone's 1:1 Target (both arms alike)")
	flag.Parse()

	client := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	tf := market.Timeframe(*tfStr)
	end := time.Now().UTC()
	start := end.AddDate(0, 0, -*days)
	syms := []struct {
		short string
		sym   market.Symbol
	}{{"BTC", market.BTCUSDT}, {"ETH", market.ETHUSDT}, {"XAU", market.XAUUSDT}, {"XAG", market.XAGUSDT},
		{"SOL", market.SOLUSDT}, {"LINK", market.LINKUSDT}, {"SUI", market.SUIUSDT}, {"NEAR", market.NEARUSDT}}

	tgt := "zone 1:1 target"
	if *rCap > 0 {
		tgt = fmt.Sprintf("%.1fR cap", *rCap)
	}
	fmt.Printf("=== confirmbt · pivot-zone-fade entry A/B · %dd · %s · stop=invalidate · TP %s · confirm within %d bars ===\n",
		*days, *tfStr, tgt, *confirmBars)
	fmt.Printf("%-5s %6s %6s | %5s %5s %5s %8s | %5s %5s %5s %8s\n",
		"sym", "events", "conf%", "tFill", "tNoF", "tWR%", "T R/trd", "cFill", "cNoF", "cWR%", "C R/trd")

	var aggT, aggC float64
	var aggTN, aggCN, aggEv, aggConf int
	// The decomposition: TOUCH's R split by whether the event later confirmed.
	var touchOnConf, touchOnNever float64
	var nConf, nNever int

	for _, s := range syms {
		cs, err := client.KlinesRange(context.Background(), s.sym, tf, start, end)
		if err != nil || len(cs) < 120 {
			log.Printf("%s: klines %v (len %d)", s.short, err, len(cs))
			continue
		}
		evs := detectZoneEvents(cs, *confirmBars)
		if len(evs) == 0 {
			fmt.Printf("%-5s %6d %6s | %5s %5s %5s %8s | %5s %5s %5s %8s\n", s.short, 0, "—", "—", "—", "—", "—", "—", "—", "—", "—")
			continue
		}
		tR, tN, tUF, tW, perEv := score(cs, evs, s.short, false, *rCap, *holdBars)
		cR, cN, cUF, cW, _ := score(cs, evs, s.short, true, *rCap, *holdBars)

		conf := 0
		for _, e := range evs {
			if e.ConfirmBar >= 0 {
				conf++
			}
		}
		for i, e := range evs {
			r, ok := perEv[i]
			if !ok {
				continue
			}
			if e.ConfirmBar >= 0 {
				touchOnConf += r
				nConf++
			} else {
				touchOnNever += r
				nNever++
			}
		}

		tRPT, cRPT := 0.0, 0.0
		if tN > 0 {
			tRPT = tR / float64(tN)
		}
		if cN > 0 {
			cRPT = cR / float64(cN)
		}
		tWR, cWR := 0.0, 0.0
		if tN > 0 {
			tWR = float64(tW) / float64(tN) * 100
		}
		if cN > 0 {
			cWR = float64(cW) / float64(cN) * 100
		}
		fmt.Printf("%-5s %6d %5.0f%% | %5d %5d %5.0f %+8.3f | %5d %5d %5.0f %+8.3f\n",
			s.short, len(evs), float64(conf)/float64(len(evs))*100,
			tN, tUF, tWR, tRPT, cN, cUF, cWR, cRPT)

		aggT += tR
		aggC += cR
		aggTN += tN
		aggCN += cN
		aggEv += len(evs)
		aggConf += conf
	}

	aggTRPT, aggCRPT := 0.0, 0.0
	if aggTN > 0 {
		aggTRPT = aggT / float64(aggTN)
	}
	if aggCN > 0 {
		aggCRPT = aggC / float64(aggCN)
	}
	fmt.Printf("--- aggregate: %d zone events, %.0f%% later confirmed\n", aggEv, float64(aggConf)/float64(max(aggEv, 1))*100)
	fmt.Printf("    TOUCH    resolved %4d  netR %+8.2f  R/trade %+.3f  trd/day %.2f\n", aggTN, aggT, aggTRPT, float64(aggTN)/float64(*days))
	fmt.Printf("    CONFIRM  resolved %4d  netR %+8.2f  R/trade %+.3f  trd/day %.2f\n", aggCN, aggC, aggCRPT, float64(aggCN)/float64(*days))

	// The question confirmation actually has to answer.
	fmt.Printf("\n    TOUCH's own R, split by whether that event LATER confirmed:\n")
	cAvg, nAvg := 0.0, 0.0
	if nConf > 0 {
		cAvg = touchOnConf / float64(nConf)
	}
	if nNever > 0 {
		nAvg = touchOnNever / float64(nNever)
	}
	fmt.Printf("      later CONFIRMED  n=%4d  netR %+8.2f  R/trade %+.3f\n", nConf, touchOnConf, cAvg)
	fmt.Printf("      never confirmed  n=%4d  netR %+8.2f  R/trade %+.3f\n", nNever, touchOnNever, nAvg)
	fmt.Printf("      -> confirmation declines the second group. It pays only if that group is losing,\n")
	fmt.Printf("         AND by more than the worse entry price costs on the first.\n")
	fmt.Printf("    NOTE: this split is DIAGNOSTIC, not a gate — a post-hoc subgroup does not transfer\n")
	fmt.Printf("          to a strategy change under one-position dedup (feedback-bucket-split-not-ab).\n")
}

// detectZoneEvents finds each entry of price into a directional 樞紐區, using
// structure computed from cs[:i] only so nothing is read from the future.
//
// One event per zone occupancy, not one per bar: re-firing every bar price sits
// in the zone would inflate the sample with the same trade.
func detectZoneEvents(cs []market.Candle, confirmBars int) []zoneEvent {
	var out []zoneEvent
	inZone := false
	for i := 100; i < len(cs); i++ {
		st := signal.AnalyzeStructure(cs[:i], 2)
		if st.Zone == nil {
			inZone = false
			continue
		}
		z := st.Zone
		lo, hi := z.Lo, z.Hi
		if hi < lo {
			lo, hi = hi, lo
		}
		bar := cs[i]
		touched := bar.Low <= hi && bar.High >= lo
		if !touched {
			inZone = false
			continue
		}
		if inZone {
			continue // already counted this occupancy
		}
		inZone = true

		var side string
		var near float64
		switch z.Dir {
		case signal.StructUptrend:
			// Bullish pullback: price falls INTO the zone from above, so the
			// upper edge is what it reaches first.
			side, near = "long", hi
		case signal.StructDowntrend:
			side, near = "short", lo
		default:
			continue // no directional trend -> not a fade setup
		}
		e := zoneEvent{
			Bar: i, Side: side, Near: near,
			Invalidate: z.Invalidate, Target: z.Target,
			ConfirmBar: -1,
		}
		// Confirmation: a later bar CLOSES back out of the zone in the trade
		// direction. For a long fade that means closing back above the zone.
		for j := i; j < len(cs) && j <= i+confirmBars; j++ {
			c := cs[j].Close
			if (side == "long" && c > hi) || (side == "short" && c < lo) {
				e.ConfirmBar, e.ConfirmPx = j, c
				break
			}
		}
		out = append(out, e)
	}
	return out
}

// score runs one arm. perEv maps event index -> realized R for the TOUCH arm,
// which the caller uses for the confirmed/never-confirmed decomposition.
func score(cs []market.Candle, evs []zoneEvent, short string, confirmArm bool, rCap float64, hold int) (netR float64, resolved, unfilled, won int, perEv map[int]float64) {
	perEv = map[int]float64{}
	type tagged struct {
		fire autotrade.PaperFire
		idx  int
	}
	var items []tagged
	for i, e := range evs {
		entry, fireBar := e.Near, e.Bar
		if confirmArm {
			if e.ConfirmBar < 0 {
				continue // never confirmed -> this arm takes no trade
			}
			entry, fireBar = e.ConfirmPx, e.ConfirmBar
		}
		stop := e.Invalidate
		// Same structural stop and the same target price for both arms — the
		// entry is the only thing that differs.
		risk := entry - stop
		if e.Side == "short" {
			risk = stop - entry
		}
		if risk <= 0 {
			continue
		}
		tp := e.Target
		if rCap > 0 {
			if e.Side == "long" {
				tp = entry + rCap*risk
			} else {
				tp = entry - rCap*risk
			}
		}
		// A target on the wrong side of entry is not a trade.
		if (e.Side == "long" && tp <= entry) || (e.Side == "short" && tp >= entry) {
			continue
		}
		items = append(items, tagged{autotrade.PaperFire{
			Time: cs[fireBar].CloseTime, Symbol: short, TF: "1h",
			Strategy: "zonefade", Side: e.Side,
			Entry: entry, Stop: stop, TP: tp, Margin: 35, Lev: 125,
			Why: fmt.Sprintf("zone-fade %s near %.4f", e.Side, e.Near),
		}, i})
	}
	fires := make([]autotrade.PaperFire, len(items))
	for i, it := range items {
		fires[i] = it.fire
	}
	positions := autotrade.DedupFires(fires, 6, 6, time.Hour, func(f autotrade.PaperFire) autotrade.Outcome {
		return autotrade.EvaluateFire(f, cs, hold)
	})
	// Map positions back to event indices by fire identity, so the split below
	// describes the trades that were actually TAKEN after dedup.
	idxOf := map[string]int{}
	for _, it := range items {
		idxOf[it.fire.Time.String()+it.fire.Side] = it.idx
	}
	for _, p := range positions {
		switch p.Outcome.Status {
		case autotrade.OutTP, autotrade.OutStop:
			netR += p.Outcome.NetR
			resolved++
			if p.Outcome.Status == autotrade.OutTP {
				won++
			}
			if !confirmArm {
				if idx, ok := idxOf[p.Fire.Time.String()+p.Fire.Side]; ok {
					perEv[idx] = p.Outcome.NetR
				}
			}
		case autotrade.OutNoFill, autotrade.OutPending:
			unfilled++
		}
	}
	return netR, resolved, unfilled, won, perEv
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
