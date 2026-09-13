// shelfbt — C10. A/B the SHELF RETEST as an entry.
//
// The claim under test, from the desk: "方向的拐點基本上高機率發生在這個帶裡面"
// — turns happen inside the shelf band with high probability.
//
// HALF OF THAT NEEDS NO TEST. A shelf is BY DEFINITION a band where swing
// highs and swing lows have both already clustered, so "turns have happened
// here" is true by construction and predicts nothing. That descriptive fact
// alone justifies putting a stop BEYOND the band — the band is the observed
// turn-scatter, so a stop inside it sits where price is known to travel. This
// harness tests only the FORWARD claim: does a retest produce a tradeable turn?
//
// ARMS. There is deliberately no "arm A" here. A baseline of "don't look at
// shelves" takes no trades, so its netR is 0 by construction and a relative
// comparison against it is meaningless. The A-versus-rest question IS the
// absolute floor in package shipgate: netR > 0 in every window, plus a median
// R/trade floor. That is what "better than not trading" means, and cmd/gate
// already enforces it.
//
//	B  touch    — enter when price ENTERS the band. Direction from the side it
//	              came from: arriving from above = long the band as support,
//	              from below = short it as resistance.
//	C  close-in — enter only when a bar CLOSES inside the band. Same question
//	              cmd/confirmbt already answered for pivot zones (touch
//	              -0.41R/trade vs close-in -0.12R/trade), so C is expected to
//	              beat B; what matters is whether either clears zero.
//	D  strong   — as C, but only bands with min(highs, lows) >= 3, the tier the
//	              chart draws solid. This is the arm that tests whether the
//	              layer's OWN strength metric carries information.
//
// STOP AND TARGET ARE HELD FIXED ACROSS ARMS, anchored to the BAND EDGES and
// never to the entry. Per-arm R targets are how a tighter-entry arm wins
// mechanically — a +319R result once flipped to worse-everywhere as soon as
// the target was pinned. Here the only thing that varies between B, C and D is
// WHEN you get in.
//
// PRIOR, stated up front: proximity votes, EQH/EQL-as-vote, polarity-flip and
// liquidity-TP have ALL been A/B-rejected in this system. A band-derived entry
// starts from a losing record, and one live counterexample already exists —
// 2026-09-07 10:14, BTC turned 17 points ABOVE a band's top edge, inside a
// 6-touch EQH pool the band had nothing to do with.
//
// ---------------------------------------------------------------------------
// RUN AND REJECTED 2026-09-07. Do not re-run; the surface has no peak.
//
//	arm           60d              90d              120d             R/trade
//	B-touch       -102.92  n=502   -136.34  n=733   -174.07  n=931   -0.187
//	C-close-in     -53.04  n=345    -57.46  n=481    -52.30  n=597   -0.119
//	D-strong        -7.20  n= 49     -2.11  n= 60     +4.80  n= 79   -0.035
//
// Three cmd/gate runs, three FAILs on absolute grounds. Nine arm-window cells,
// exactly one positive (D at 120d, +4.80R over 79 trades).
//
// The mechanism ORDERING came out exactly as the prior predicted — touch is
// worse than close-in (matching cmd/confirmbt's -0.41 vs -0.12 on pivot zones)
// and the two-sidedness filter improves it further, so the layer's own
// strength metric does carry SOME information. It just carries it from -0.19
// to -0.04, not to positive.
//
// A parameter sweep is what settles it. The stop distance is the only lever
// that could plausibly rescue this, and it has no peak (90d, C arm):
//
//	stop-atr   0.2     0.3     0.4     0.5     0.6     0.8     1.0     1.5
//	R/trade  -0.011  +0.004  -0.115  -0.120  -0.118  -0.068  -0.027  -0.019
//
// The single positive point is +0.0044 R/trade (netR +2.28 over 514 trades —
// noise), and its neighbour at 0.4 is -0.115. The surface is <= 0 EVERYWHERE.
// That is a stronger rejection than a param cliff: a cliff implies a peak
// worth investigating, and here there is none.
//
// And the shape explains itself. At 0.4-0.6 ATR the stop sits inside the
// band's own noise and gets picked off. By 1.0-1.5 the stop is far enough to
// survive, but the entry is then so far from it that R cannot pay the fees.
// No distance is simultaneously outside the noise and close enough to matter.
//
// WHICH IS THE DESCRIPTIVE CLAIM, CONFIRMED BY THE THING THAT KILLS THE
// PREDICTIVE ONE. The band IS the observed turn-scatter, so a stop must go
// beyond it — and once it does, the trade has no edge left. The shelf layer
// stays what the glossary says it is: a stop-placement and context tool, not
// an entry signal.
//
// One more tell, visible before any of the above: 3.5-4.0 events per symbol
// per day. A "structural band retest" happening four times a day is not a
// special location — with six bands inside 5% of price, contacting one is
// ordinary intraday movement. That fire rate alone should lower the prior.
// ---------------------------------------------------------------------------
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"time"

	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/config"
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/signal"
)

// arm identifies which entry rule a run uses.
type arm string

const (
	armTouch   arm = "B-touch"
	armCloseIn arm = "C-close-in"
	armStrong  arm = "D-strong"
)

// shelfEvent is one retest of one band, detected on closed bars only.
type shelfEvent struct {
	Bar  int    // bar that made contact (touch semantics)
	Side string // "long" | "short" — from the side price arrived from
	Lo   float64
	Hi   float64
	Bal  int // min(highs, lows) — the two-sidedness the strong arm filters on

	// TouchPx is the fill for arm B: the band edge price actually reached,
	// which is what a resting limit at that edge would have got.
	TouchPx float64
	// CloseInBar/Px is the first bar at or after Bar whose CLOSE is inside
	// the band. -1 when that never happened within the confirm window.
	CloseInBar int
	CloseInPx  float64

	// Stop and Target are anchored to the band edges and to ATR, so both are
	// identical for every arm that trades this event.
	Stop   float64
	Target float64
}

func main() {
	config.LoadDotEnv()
	days := flag.Int("days", 90, "history window in days")
	tfStr := flag.String("tf", "1h", "timeframe")
	lookback := flag.Int("lookback", 300, "candles handed to FindShelves per bar — 300 matches what /chart fetches, so the harness sees the bands the desk sees")
	tolPct := flag.Float64("tol", 0.0015, "shelf clustering tolerance, fractional (0.0015 = 15bps, same as the chart)")
	minTouches := flag.Int("min-touches", 3, "minimum pivots per band (chart uses 3)")
	strongBal := flag.Int("strong-bal", 3, "min(highs,lows) required by the D arm")
	stopATR := flag.Float64("stop-atr", 0.5, "stop this many ATR(14) BEYOND the far band edge — identical for every arm")
	tgtATR := flag.Float64("tgt-atr", 2.0, "target this many ATR(14) beyond the near band edge — identical for every arm")
	confirmBars := flag.Int("confirm-bars", 4, "bars the close-in arms wait for a close inside the band before abandoning the event")
	holdBars := flag.Int("hold", 24, "max bars to hold (EvaluateFire expiry)")
	flag.Parse()

	client := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	tf := market.Timeframe(*tfStr)
	end := time.Now().UTC()
	start := end.AddDate(0, 0, -*days)

	syms := []struct {
		short string
		sym   market.Symbol
	}{
		{"BTC", market.BTCUSDT}, {"ETH", market.ETHUSDT},
		{"XAU", market.XAUUSDT}, {"XAG", market.XAGUSDT},
		{"SOL", market.SOLUSDT}, {"LINK", market.LINKUSDT},
		{"SUI", market.SUIUSDT}, {"NEAR", market.NEARUSDT},
	}

	fmt.Printf("=== shelfbt · C10 shelf-retest entry A/B · %dd · %s ===\n", *days, *tfStr)
	fmt.Printf("    bands: tol %.4f minTouches %d lookback %d | stop %.2fATR beyond far edge, target %.2fATR beyond near edge (SAME for all arms)\n",
		*tolPct, *minTouches, *lookback, *stopATR, *tgtATR)
	fmt.Printf("    no baseline arm by design — 'do not trade' is netR 0, so the comparison is cmd/gate's ABSOLUTE floor\n\n")
	fmt.Printf("%-5s %7s %7s | %6s %6s %6s %9s | %6s %6s %6s %9s | %6s %6s %6s %9s\n",
		"sym", "events", "strong",
		"B trd", "B noF", "B WR%", "B R/trd",
		"C trd", "C noF", "C WR%", "C R/trd",
		"D trd", "D noF", "D WR%", "D R/trd")

	type agg struct {
		netR   float64
		trades int
	}
	tot := map[arm]*agg{armTouch: {}, armCloseIn: {}, armStrong: {}}
	var totEv, totStrong int

	for _, s := range syms {
		cs, err := client.KlinesRange(context.Background(), s.sym, tf, start, end)
		if err != nil || len(cs) < 150 {
			log.Printf("%s: klines %v (len %d)", s.short, err, len(cs))
			continue
		}
		evs := detectShelfEvents(cs, *lookback, *tolPct, *minTouches, *stopATR, *tgtATR, *confirmBars)
		strong := 0
		for _, e := range evs {
			if e.Bal >= *strongBal {
				strong++
			}
		}
		totEv += len(evs)
		totStrong += strong
		if len(evs) == 0 {
			fmt.Printf("%-5s %7d %7s | %6s %6s %6s %9s | %6s %6s %6s %9s | %6s %6s %6s %9s\n",
				s.short, 0, "—", "—", "—", "—", "—", "—", "—", "—", "—", "—", "—", "—", "—")
			continue
		}

		row := fmt.Sprintf("%-5s %7d %7d", s.short, len(evs), strong)
		for _, a := range []arm{armTouch, armCloseIn, armStrong} {
			netR, trades, noFill, won := score(cs, evs, s.short, a, *strongBal, *holdBars)
			tot[a].netR += netR
			tot[a].trades += trades
			wr, rpt := "—", "—"
			if trades > 0 {
				wr = fmt.Sprintf("%.0f", float64(won)/float64(trades)*100)
				rpt = fmt.Sprintf("%+.3f", netR/float64(trades))
			}
			row += fmt.Sprintf(" | %6d %6d %6s %9s", trades, noFill, wr, rpt)
		}
		fmt.Println(row)
	}

	fmt.Printf("\n--- aggregate (%dd, %s) · events %d, of which strong %d ---\n", *days, *tfStr, totEv, totStrong)
	for _, a := range []arm{armTouch, armCloseIn, armStrong} {
		g := tot[a]
		if g.trades == 0 {
			fmt.Printf("  %-12s no trades\n", a)
			continue
		}
		fmt.Printf("  %-12s netR %+8.2f over %4d trades = %+.4f R/trade\n", a, g.netR, g.trades, g.netR/float64(g.trades))
	}
	fmt.Printf("\nFeed the per-window numbers to cmd/gate. A positive aggregate here is NOT a pass:\n")
	fmt.Printf("the gate needs positive netR in EVERY window plus the absolute R/trade floor.\n")
}

// detectShelfEvents walks closed bars and records each fresh contact with a
// shelf band.
//
// No look-ahead: bands for bar i are computed from cs[:i+1] only, and the
// confirm scan reads bars strictly after i. The lookback slice keeps the cost
// bounded AND makes the harness see the same band set the chart does, which
// fetches 300 candles — a harness that sees more history than production
// would be testing a different feature.
func detectShelfEvents(cs []market.Candle, lookback int, tolPct float64, minTouches int, stopATR, tgtATR float64, confirmBars int) []shelfEvent {
	if len(cs) < 60 {
		return nil
	}
	atr := indicator.ATR(cs, 14)
	var out []shelfEvent
	// Remember the last bar each band was traded, keyed by its rounded edges,
	// so a band price sits in for several bars does not emit an event per bar.
	lastFire := map[string]int{}

	for i := 60; i < len(cs); i++ {
		lo := i - lookback + 1
		if lo < 0 {
			lo = 0
		}
		shelves := signal.FindShelves(cs[lo:i+1], 2, 0, tolPct, minTouches)
		if len(shelves) == 0 || atr[i] <= 0 {
			continue
		}
		prev, cur := cs[i-1], cs[i]
		for _, sh := range shelves {
			// Fresh contact only: the PREVIOUS bar must have been entirely
			// clear of the band, or every bar of a slow grind through it
			// would fire.
			prevAbove := prev.Low > sh.Hi
			prevBelow := prev.High < sh.Lo
			if !prevAbove && !prevBelow {
				continue
			}
			touched := cur.Low <= sh.Hi && cur.High >= sh.Lo
			if !touched {
				continue
			}
			key := fmt.Sprintf("%.4f|%.4f", sh.Lo, sh.Hi)
			if last, ok := lastFire[key]; ok && i-last < confirmBars+1 {
				continue
			}
			lastFire[key] = i

			ev := shelfEvent{
				Bar: i, Lo: sh.Lo, Hi: sh.Hi,
				Bal: minInt(sh.Highs, sh.Lows), CloseInBar: -1,
			}
			if prevAbove {
				// Arrived from above: trade the band as support.
				ev.Side = "long"
				ev.TouchPx = sh.Hi
				ev.Stop = sh.Lo - stopATR*atr[i]
				ev.Target = sh.Hi + tgtATR*atr[i]
			} else {
				ev.Side = "short"
				ev.TouchPx = sh.Lo
				ev.Stop = sh.Hi + stopATR*atr[i]
				ev.Target = sh.Lo - tgtATR*atr[i]
			}
			// First close inside the band, at or after contact.
			for j := i; j < len(cs) && j <= i+confirmBars; j++ {
				if cs[j].Close >= sh.Lo && cs[j].Close <= sh.Hi {
					ev.CloseInBar, ev.CloseInPx = j, cs[j].Close
					break
				}
			}
			out = append(out, ev)
		}
	}
	return out
}

// score replays one arm over the detected events. Only the entry price and
// the fire bar differ per arm; Stop and Target come straight off the event.
func score(cs []market.Candle, evs []shelfEvent, short string, a arm, strongBal, hold int) (netR float64, trades, noFill, won int) {
	var fires []autotrade.PaperFire
	for _, e := range evs {
		entry, bar := e.TouchPx, e.Bar
		if a == armCloseIn || a == armStrong {
			if e.CloseInBar < 0 {
				continue // never closed inside — this arm takes no trade
			}
			entry, bar = e.CloseInPx, e.CloseInBar
		}
		if a == armStrong && e.Bal < strongBal {
			continue
		}
		risk := math.Abs(entry - e.Stop)
		if risk <= 0 {
			continue
		}
		// A target the entry has already passed is not a trade. This can
		// happen on the close-in arms when the close lands beyond the near
		// edge, and silently keeping it would score a free win.
		if (e.Side == "long" && e.Target <= entry) || (e.Side == "short" && e.Target >= entry) {
			continue
		}
		// And a stop the entry is already through is not a trade either.
		if (e.Side == "long" && e.Stop >= entry) || (e.Side == "short" && e.Stop <= entry) {
			continue
		}
		fires = append(fires, autotrade.PaperFire{
			Time: cs[bar].CloseTime, Symbol: short, TF: "1h",
			Strategy: "shelf-retest", Side: e.Side,
			Entry: entry, Stop: e.Stop, TP: e.Target, Margin: 35, Lev: 125,
			Why: fmt.Sprintf("shelf %s %.4f-%.4f bal%d", e.Side, e.Lo, e.Hi, e.Bal),
		})
	}
	positions := autotrade.DedupFires(fires, 6, 6, time.Hour, func(f autotrade.PaperFire) autotrade.Outcome {
		return autotrade.EvaluateFire(f, cs, hold)
	})
	for _, p := range positions {
		switch p.Outcome.Status {
		case autotrade.OutTP, autotrade.OutStop:
			netR += p.Outcome.NetR
			trades++
			if p.Outcome.Status == autotrade.OutTP {
				won++
			}
		case autotrade.OutNoFill, autotrade.OutPending:
			noFill++
		}
	}
	return netR, trades, noFill, won
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
