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

	"myFirstGo/trading-bot/market"
	"myFirstGo/trading-bot/oi"
	"myFirstGo/trading-bot/signal"
)

// oiDeltaThreshold mirrors the engine's own trigger (signal/engine.go:
// annotateContextWarnings uses ±0.02) so the card cannot disagree with the
// warning it is previewing.
const oiDeltaThreshold = 0.02

// oiStaticMinSamples is how much evidence it takes to call a symbol's open
// interest constant rather than merely quiet. Six samples is 30 minutes at
// the sampler's cadence.
const oiStaticMinSamples = 6

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
		} else if prev := oi.PrevFor(snaps, string(sym), market.BarDuration(market.TF1h), now); prev > 0 {
			d := (cur.OI - prev) / prev
			row["prev1h"] = prev
			row["delta"] = d
			// The label states which engine warning this delta would produce
			// and on which side, because the delta's sign alone does not say
			// it: the same -7% is a long unwind under a Long signal and
			// nothing at all under a Short one.
			switch {
			case d <= -oiDeltaThreshold:
				row["arms"] = "long-unwind"
			case d >= oiDeltaThreshold:
				row["arms"] = "shorts-crowding"
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
		"samples":        len(snaps),
		"spanMin":        spanMin,
		"hasHourOfDepth": spanMin >= 60,
		"deltaThreshold": oiDeltaThreshold,
		"path":           oi.Path(),
		"atTPE":          now.In(tpe).Format("2006-01-02 15:04:05"),
	})
}
