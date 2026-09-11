package main

// GET /ops/oi — open-interest change and funding for the whole roster.
//
// The engine already turns a prior OI reading into a warning ("OI up X% —
// shorts crowding, squeeze risk" / "OI down X% — long unwind driving the
// move"), but those are annotations on a Long or Short signal. When every
// symbol scores FLAT — which is most of the time — the measurement exists and
// nothing shows it. This is the standalone readout.
//
// Zero external calls. Every number comes from /opt/trading/oi.jsonl, which
// the monitor's sampler writes every 5 minutes, so opening /ops costs no API
// requests and cannot be rate-limited. The cost is that "now" is as old as
// the last sample, so the response carries each row's timestamp and its age
// rather than implying the reading is live.

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"myFirstGo/trading-bot/binfut"
	"myFirstGo/trading-bot/market"
	"myFirstGo/trading-bot/oi"
	"myFirstGo/trading-bot/signal"
)

// oiDeltaThreshold mirrors the engine's own trigger (signal/engine.go:
// annotateContextWarnings uses ±0.02) so the card cannot disagree with the
// warning it is previewing.
const oiDeltaThreshold = 0.02

// bnFetched renders the Binance cache age, or "" when nothing is cached —
// so a blank cross-reference column is distinguishable from a stale one.
func bnFetched(s binfut.Store, loc *time.Location) string {
	if s.FetchedAt.IsZero() {
		return ""
	}
	return s.FetchedAt.In(loc).Format("15:04:05")
}

// oiStaticMinSamples is how much evidence it takes to call a symbol's open
// interest constant rather than merely quiet. Six samples is 30 minutes at
// the sampler's cadence.
const oiStaticMinSamples = 6

// oiPriceNoiseFloor is the smallest price move over the window that this will
// treat as a direction. Below it the quadrant is reported as indeterminate
// rather than guessed: a +1.9% OI change against a -0.06% drift is not
// evidence about which side opened those contracts.
//
// A noise guard, not a measurement — deliberately loose, and the actual price
// delta is returned alongside so the reader can judge it.
const oiPriceNoiseFloor = 0.001 // 0.1%

// oiQuadrant names what an OI change plus a price change over the same window
// says about who opened or closed the contracts. This is the standard
// four-way reading, and it is the correction to a first version of this card
// that labelled OI-up as "shorts crowding" full stop.
//
// That label was wrong in a way the live data showed within two hours: ETH ran
// OI +1.82% while price rose 0.5%, which is longs being added, not shorts.
// signal.annotateContextWarnings gets away with reading OI alone because its
// warnings only fire under an existing Long or Short signal — the signal
// supplies the direction. Dropping that condition while keeping the conclusion
// is how a readout ends up asserting the opposite of what happened.
func oiQuadrant(oiDelta, priceDelta float64) (key, label string) {
	if oiDelta >= oiDeltaThreshold {
		switch {
		case priceDelta <= -oiPriceNoiseFloor:
			return "shorts-building", "跌勢中新倉 — 空單堆積"
		case priceDelta >= oiPriceNoiseFloor:
			return "longs-building", "漲勢中新倉 — 多單堆積"
		}
		return "new-positions", "新倉進場 — 價格無方向,分不出邊"
	}
	if oiDelta <= -oiDeltaThreshold {
		switch {
		case priceDelta <= -oiPriceNoiseFloor:
			return "longs-unwinding", "跌勢中減倉 — 多單解除"
		case priceDelta >= oiPriceNoiseFloor:
			return "shorts-covering", "漲勢中減倉 — 空單回補"
		}
		return "closing", "倉位減少 — 價格無方向,分不出邊"
	}
	return "", ""
}

// oiIsStatic reports whether every stored reading for sym is the same number.
//
// BingX publishes a fixed open interest for its CFD-style synthetics — the
// NCCO* metals and NCSK* US-stock contracts. Measured over 132 minutes and 30
// samples, all seven returned exactly one distinct value while all seven
// native perps returned 15-17. So their delta is structurally 0.00%, forever.
//
// Rendering that as a 0.00% row is the display-vs-reality failure again: it
// reads as "nothing is happening" when the truth is "this number cannot
// happen". Detected from the data rather than an allowlist, so it corrects
// itself if the venue ever starts publishing real figures — and so it flags
// any OTHER symbol whose feed silently freezes.
//
// Funding is unaffected and still shown: it moves on the synthetics (XAG was
// +0.0230%, MSTR +0.0193%), it is just open interest that does not.
func oiIsStatic(snaps []oi.Snapshot, sym string) bool {
	n := 0
	var first float64
	for _, s := range snaps {
		if s.Symbol != sym {
			continue
		}
		if n == 0 {
			first = s.OI
		} else if s.OI != first {
			return false
		}
		n++
	}
	return n >= oiStaticMinSamples
}

func (s *server) handleOpsOI(c *gin.Context) {
	snaps := oi.Load()
	now := time.Now().UTC()
	tpe := time.FixedZone("Asia/Taipei", 8*3600)

	bar := market.BarDuration(market.TF1h)
	bfStore := binfut.Load()
	rows := make([]gin.H, 0, len(uiSymbols))
	for _, short := range uiSymbols {
		sym, err := resolveWebSymbol(short)
		if err != nil {
			continue
		}
		cur, ok := oi.Latest(snaps, string(sym))
		if !ok {
			// No sample yet — a symbol the sampler has never reached, or a
			// store that has just been created. Reported as a row so the
			// gap is visible instead of the symbol silently vanishing.
			rows = append(rows, gin.H{"short": short, "state": "no-sample"})
			continue
		}

		row := gin.H{
			"short":   short,
			"state":   "ok",
			"oi":      cur.OI,
			"atTPE":   cur.Time.In(tpe).Format("15:04:05"),
			"ageMin":  int(now.Sub(cur.Time).Minutes()),
			"funding": cur.Funding,
		}
		// Funding thresholds are the engine's (signal.FundingCrowdedLong /
		// FundingCrowdedShort), so "crowded" here means what it means there.
		switch {
		case cur.Funding > signal.FundingCrowdedLong:
			row["fundingSide"] = "longs-crowded"
		case cur.Funding < signal.FundingCrowdedShort:
			row["fundingSide"] = "shorts-crowded"
		}

		if oiIsStatic(snaps, string(sym)) {
			row["oiStatic"] = true
		} else if prev := oi.PrevFor(snaps, string(sym), bar, now); prev > 0 {
			d := (cur.OI - prev) / prev
			row["prev1h"] = prev
			row["delta"] = d

			// The quadrant needs the price move over the SAME window. Without
			// it there is no honest label — see oiQuadrant.
			if pd, ok := oi.PriceChangeOver(snaps, string(sym), bar, now); ok {
				row["priceDelta"] = pd
				if key, label := oiQuadrant(d, pd); key != "" {
					row["quadrant"] = key
					row["reading"] = label
				}
			} else if d <= -oiDeltaThreshold || d >= oiDeltaThreshold {
				// Past the threshold but no price history to interpret it —
				// say so instead of falling back to an OI-only verdict.
				row["quadrant"] = "no-price"
				row["reading"] = "OI 已過門檻,但缺同窗價格,無法判邊"
			}
		}
		// Binance cross-reference: a real 5-minute OI series where BingX has
		// only a 10-minute republish, and the whale-vs-retail split BingX
		// cannot answer at all. Kept in separate keys, never merged into the
		// BingX figures — they are different books, and an entry fills
		// against BingX's.
		if b, ok := bfStore.LatestOI(short); ok {
			row["bnOI"] = b.Value
			if d, ok := bfStore.OIChange(short, time.Hour); ok {
				row["bnDelta1h"] = d
			}
			// 15 minutes is possible HERE and nowhere else: Binance buckets
			// are genuinely 5m, so three of them is a real reading rather
			// than a re-read of one published value.
			if d, ok := bfStore.OIChange(short, 15*time.Minute); ok {
				row["bnDelta15m"] = d
			}
		}
		if w, ok := bfStore.Whales(short); ok {
			row["whaleTop"] = w.Top
			row["whaleAll"] = w.All
			row["whaleGap"] = w.Gap
			row["whaleTopSize"] = w.TopSz
			if c := w.Crowd(); c != "" {
				row["crowd"] = c
			}
		}
		rows = append(rows, row)
	}

	// Depth is what tells the reader whether an empty delta column means
	// "nothing is moving" or "the sampler has not been up an hour yet".
	var oldest, newest time.Time
	for _, sn := range snaps {
		if oldest.IsZero() || sn.Time.Before(oldest) {
			oldest = sn.Time
		}
		if sn.Time.After(newest) {
			newest = sn.Time
		}
	}
	var spanMin int
	if !oldest.IsZero() {
		spanMin = int(newest.Sub(oldest).Minutes())
	}

	c.JSON(http.StatusOK, gin.H{
		"rows":           rows,
		"bnFetchedTPE":   bnFetched(bfStore, tpe),
		"samples":        len(snaps),
		"spanMin":        spanMin,
		"hasHourOfDepth": spanMin >= 60,
		"deltaThreshold": oiDeltaThreshold,
		"path":           oi.Path(),
		"atTPE":          now.In(tpe).Format("2006-01-02 15:04:05"),
	})
}
