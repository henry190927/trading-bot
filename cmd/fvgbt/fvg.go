package main

import (
	"time"

	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/market"
)

// FVG is a three-bar fair-value gap: a price band that bar i-2 and bar i do
// not both trade through, so the move between them left an untraded pocket.
//
// Lo and Hi are the band edges with Lo < Hi regardless of direction. For a
// bullish gap price sits ABOVE the band and a retrace falls into it; for a
// bearish gap price sits BELOW and a retrace rises into it.
type FVG struct {
	Lo, Hi    float64
	Bullish   bool
	FormedIdx int       // index of the third bar — the one that completes the gap
	FormedAt  time.Time // that bar's CloseTime
}

// Height is the untraded span, in price.
func (g FVG) Height() float64 { return g.Hi - g.Lo }

// FindFVGs returns every three-bar gap in cs, in formation order.
//
// A gap is attributed to the bar that COMPLETES it (index i), not to the bar
// that opened it, because that is the first moment the gap is knowable. Callers
// relying on this for a no-look-ahead walk must still wait for bar i to close:
// see the FormedIdx <= i-1 activation test in genFVGFires.
func FindFVGs(cs []market.Candle) []FVG {
	var out []FVG
	for i := 2; i < len(cs); i++ {
		a, c := cs[i-2], cs[i]
		if c.Low > a.High { // gap up: nothing traded between a.High and c.Low
			out = append(out, FVG{Lo: a.High, Hi: c.Low, Bullish: true, FormedIdx: i, FormedAt: c.CloseTime})
			continue
		}
		if c.High < a.Low { // gap down
			out = append(out, FVG{Lo: c.High, Hi: a.Low, Bullish: false, FormedIdx: i, FormedAt: c.CloseTime})
		}
	}
	return out
}

// genFVGFires walks closed bars and fires where price retraces into a still-open
// gap and holds it.
//
// The trade is CONTINUATION, not reversal: a bullish gap is bought on the dip
// back into it. That direction is deliberate — the engine's mean-reversion arm
// is a counter-indicator in a trend, and the pivot-zone work found the edge in
// trend-ALIGNED fades of a retrace, not in fading the trend itself.
//
// Three filters, each one a thing that can be gated separately:
//   - minGapATR drops gaps too thin to be a level (quality, judged at formation)
//   - maxAge drops gaps price has ignored for too long (freshness)
//   - a close through the far edge kills the gap outright (invalidation)
//
// bias, when non-nil, is a per-base-bar higher-timeframe direction (+1 up, -1
// down, 0 unknown) and the gap must agree with it. It is the one filter with a
// track record in this repo — the NFE structure veto earned its keep purely by
// refusing counter-trend entries — so it is wired as a separate arm rather than
// folded into the others.
//
// NO LOOK-AHEAD: a gap becomes eligible only once its third bar has closed
// STRICTLY before the bar being evaluated (FormedIdx <= i-1), and every other
// input is bar i's own OHLC or bar i-1's close.
func genFVGFires(cs []market.Candle, atr []float64, short string,
	minGapATR, bufATR, rMult float64, maxAge int, side string, bias []int) []autotrade.PaperFire {

	gaps := FindFVGs(cs)
	wantShort := side == "short" || side == "auto"
	wantLong := side == "long" || side == "auto"

	var active []FVG
	var out []autotrade.PaperFire
	gi := 0

	for i := 30; i < len(cs); i++ {
		bar, prev, a := cs[i], cs[i-1], atr[i]

		// Activate every gap completed by a bar that closed before this one.
		for gi < len(gaps) && gaps[gi].FormedIdx <= i-1 {
			g := gaps[gi]
			gi++
			if fa := atr[g.FormedIdx]; fa <= 0 || g.Height() < minGapATR*fa {
				continue
			}
			active = append(active, g)
		}
		if a <= 0 {
			continue
		}

		// Age out, then pick the gap NEAREST to price. Several can be open at
		// once; the one price actually touches on the way down is the highest
		// bullish band below it, not the oldest.
		kept := active[:0]
		bullBest, bearBest := -1, -1
		for _, g := range active {
			if i-g.FormedIdx > maxAge {
				continue
			}
			kept = append(kept, g)
			idx := len(kept) - 1
			if bias != nil {
				if g.Bullish && bias[i] <= 0 {
					continue
				}
				if !g.Bullish && bias[i] >= 0 {
					continue
				}
			}
			switch {
			case g.Bullish && wantLong && prev.Close > g.Hi && bar.Low <= g.Hi && bar.Close > g.Lo:
				if bullBest < 0 || g.Hi > kept[bullBest].Hi {
					bullBest = idx
				}
			case !g.Bullish && wantShort && prev.Close < g.Lo && bar.High >= g.Lo && bar.Close < g.Hi:
				if bearBest < 0 || g.Lo < kept[bearBest].Lo {
					bearBest = idx
				}
			}
		}
		active = kept

		// An outside bar can reach into a bullish gap below AND a bearish gap
		// above on the same candle. That is two contradictory reads, not a
		// choice to make arbitrarily, so the bar produces nothing.
		best := -1
		switch {
		case bullBest >= 0 && bearBest >= 0:
		case bullBest >= 0:
			best = bullBest
		case bearBest >= 0:
			best = bearBest
		}

		if best >= 0 {
			g := active[best]
			var f autotrade.PaperFire
			ok := false
			if g.Bullish {
				stop := g.Lo - bufATR*a
				if risk := bar.Close - stop; risk > 0 {
					f = autotrade.PaperFire{
						Time: bar.CloseTime, Symbol: short, Strategy: "fvg", Side: "long",
						Market: true, Entry: bar.Close, Stop: stop, TP: bar.Close + rMult*risk,
					}
					ok = true
				}
			} else {
				stop := g.Hi + bufATR*a
				if risk := stop - bar.Close; risk > 0 {
					f = autotrade.PaperFire{
						Time: bar.CloseTime, Symbol: short, Strategy: "fvg", Side: "short",
						Market: true, Entry: bar.Close, Stop: stop, TP: bar.Close - rMult*risk,
					}
					ok = true
				}
			}
			if ok {
				out = append(out, f)
				active = append(active[:best], active[best+1:]...) // consumed
			}
		}

		// A close through the far edge means the pocket got traded through:
		// the gap is no longer an untested level and stops being one here.
		kept = active[:0]
		for _, g := range active {
			if g.Bullish && bar.Close < g.Lo {
				continue
			}
			if !g.Bullish && bar.Close > g.Hi {
				continue
			}
			kept = append(kept, g)
		}
		active = kept
	}
	return out
}
