package indicator

import (
	"sort"

	"myFirstGo/trading-bot/market"
)

// BodyWeight, if in (0,1), routes that fraction of each candle's volume into
// its body range [min(O,C), max(O,C)] and the remainder across the wicks
// proportional to wick length. Default 0 = legacy uniform-over-HL.
//
// Set by callers (e.g. the backtest CLI's --body-weight flag) before running
// Evaluate. The variable is package-level rather than a parameter because the
// volume profile is rebuilt deep inside signal.Evaluate and threading the
// option through every call site would be noisy for a prototype.
var BodyWeight float64

// VolumeBin is one slice of the volume profile.
type VolumeBin struct {
	PriceLow  float64
	PriceHigh float64
	PriceMid  float64
	Volume    float64
}

// VolumeProfile is the chip-concentration map (籌碼密集區) — accumulated traded
// volume binned by price level over a lookback window. The POC is the single
// price with the most volume; HVNs are the top-N volume bins, treated as
// strong S/R levels. VAH/VAL bracket the "value area" — the contiguous span
// around POC containing VAPercent of total volume (default 70%). Price
// behavior differs systematically inside vs outside VA: inside = mean-rev
// to POC, at edges = high-probability reversal candidates, outside =
// acceptance / trend mode.
type VolumeProfile struct {
	Bins      []VolumeBin
	POC       float64   // price of point of control
	HVN       []float64 // top-N HVN price midpoints, sorted by volume desc
	VAH       float64   // value area high (upper edge of 70% volume zone)
	VAL       float64   // value area low (lower edge of 70% volume zone)
	VAPercent float64   // fraction of volume captured by [VAL, VAH], e.g. 0.70
	PriceMin  float64
	PriceMax  float64
}

// BuildVolumeProfile distributes each candle's volume uniformly across its
// [low, high] range, into `bins` price buckets spanning the observed range.
// Returns the top `topN` HVN price levels by accumulated volume.
//
// Typical params: bins=80, topN=5, lookback=200 bars.
func BuildVolumeProfile(candles []market.Candle, bins, topN int) VolumeProfile {
	if len(candles) < 2 || bins < 2 {
		return VolumeProfile{}
	}
	minP, maxP := candles[0].Low, candles[0].High
	for _, c := range candles {
		if c.Low < minP {
			minP = c.Low
		}
		if c.High > maxP {
			maxP = c.High
		}
	}
	if maxP <= minP {
		return VolumeProfile{}
	}
	binWidth := (maxP - minP) / float64(bins)
	binVol := make([]float64, bins)

	addToBins := func(lo, hi, vol float64) {
		if vol <= 0 {
			return
		}
		sb := int((lo - minP) / binWidth)
		eb := int((hi - minP) / binWidth)
		if sb < 0 {
			sb = 0
		}
		if eb >= bins {
			eb = bins - 1
		}
		if eb < sb {
			eb = sb
		}
		n := eb - sb + 1
		perBin := vol / float64(n)
		for i := sb; i <= eb; i++ {
			binVol[i] += perBin
		}
	}

	bw := BodyWeight
	if bw < 0 || bw >= 1 {
		bw = 0 // out-of-range → legacy uniform path
	}

	for _, c := range candles {
		if c.Volume <= 0 || c.High <= c.Low {
			continue
		}
		if bw == 0 {
			addToBins(c.Low, c.High, c.Volume)
			continue
		}
		// Body-weighted: bw of volume goes to [bodyLo, bodyHi]; the rest
		// is split between the upper and lower wicks proportional to
		// wick length. Damps wick-hunt distortion of POC/HVN.
		bodyLo, bodyHi := c.Open, c.Close
		if bodyHi < bodyLo {
			bodyLo, bodyHi = bodyHi, bodyLo
		}
		bodyVol := c.Volume * bw
		wickVol := c.Volume * (1 - bw)
		if bodyHi > bodyLo {
			addToBins(bodyLo, bodyHi, bodyVol)
		} else {
			// doji body — collapse to single bin at that price
			b := int((bodyLo - minP) / binWidth)
			if b < 0 {
				b = 0
			}
			if b >= bins {
				b = bins - 1
			}
			binVol[b] += bodyVol
		}
		upper := c.High - bodyHi
		lower := bodyLo - c.Low
		totalWick := upper + lower
		if totalWick > 0 {
			if upper > 0 {
				addToBins(bodyHi, c.High, wickVol*(upper/totalWick))
			}
			if lower > 0 {
				addToBins(c.Low, bodyLo, wickVol*(lower/totalWick))
			}
		} else if bodyHi > bodyLo {
			// No wicks at all (rare): fold wickVol into body uniformly.
			addToBins(bodyLo, bodyHi, wickVol)
		} else {
			b := int((bodyLo - minP) / binWidth)
			if b < 0 {
				b = 0
			}
			if b >= bins {
				b = bins - 1
			}
			binVol[b] += wickVol
		}
	}

	vp := VolumeProfile{
		Bins:     make([]VolumeBin, bins),
		PriceMin: minP,
		PriceMax: maxP,
	}
	var maxV float64
	var pocIdx int
	for i := 0; i < bins; i++ {
		lo := minP + binWidth*float64(i)
		vp.Bins[i] = VolumeBin{
			PriceLow:  lo,
			PriceHigh: lo + binWidth,
			PriceMid:  lo + binWidth/2,
			Volume:    binVol[i],
		}
		if binVol[i] > maxV {
			maxV = binVol[i]
			pocIdx = i
		}
	}
	vp.POC = vp.Bins[pocIdx].PriceMid

	// Value Area: contiguous range around POC capturing ~70% of total volume.
	// Standard algo (Steidlmayer market profile): start at POC, expand by
	// adding whichever neighbor side has more volume each step, until the
	// running sum crosses the target. VAL = low edge of leftmost included
	// bin, VAH = high edge of rightmost included bin.
	const vaTargetFrac = 0.70
	var totalVol float64
	for _, v := range binVol {
		totalVol += v
	}
	if totalVol > 0 {
		target := totalVol * vaTargetFrac
		left, right := pocIdx, pocIdx
		covered := binVol[pocIdx]
		for covered < target && (left > 0 || right < bins-1) {
			var lv, rv float64
			if left > 0 {
				lv = binVol[left-1]
			}
			if right < bins-1 {
				rv = binVol[right+1]
			}
			// Tie-breaker: prefer expanding upward (right) — slight bias
			// matches how acceptance forms in trending markets.
			if rv >= lv && right < bins-1 {
				right++
				covered += rv
			} else if left > 0 {
				left--
				covered += lv
			} else {
				break
			}
		}
		vp.VAL = vp.Bins[left].PriceLow
		vp.VAH = vp.Bins[right].PriceHigh
		vp.VAPercent = covered / totalVol
	}

	// Pick top-N HVNs, but require non-adjacent (skip neighbours of already-
	// selected bins) to avoid clustering all picks around one peak.
	type bv struct {
		i int
		v float64
	}
	all := make([]bv, bins)
	for i, v := range binVol {
		all[i] = bv{i, v}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].v > all[j].v })

	taken := make(map[int]bool)
	for _, x := range all {
		if len(vp.HVN) >= topN {
			break
		}
		if x.v == 0 || taken[x.i] || taken[x.i-1] || taken[x.i+1] {
			continue
		}
		vp.HVN = append(vp.HVN, vp.Bins[x.i].PriceMid)
		taken[x.i] = true
	}
	return vp
}

// POCTrend classifies whether volume-weighted equilibrium is migrating
// upward, downward, or flat across nested lookback windows.
type POCTrend int

const (
	POCFlat POCTrend = iota
	POCRising
	POCFalling
)

func (t POCTrend) String() string {
	switch t {
	case POCRising:
		return "rising"
	case POCFalling:
		return "falling"
	default:
		return "flat"
	}
}

// POCMigration tracks POC across short / medium / long windows so the
// validator can read regime — trending vs ranging — from a volume-weighted
// reference rather than a price-only filter (SMA, ADX). The drift fraction
// is (POC_short − POC_long) / POC_long; magnitude < ~0.005 → flat.
type POCMigration struct {
	POCShort  float64 // typically last 50 bars
	POCMed    float64 // last 100 bars
	POCLong   float64 // last 200 bars
	DriftPct  float64 // (POCShort - POCLong) / POCLong; sign = direction
	Trend     POCTrend
	Stacked   bool // strict POCShort > POCMed > POCLong (or reverse for falling)
}

// ComputePOCMigration runs three nested BuildVolumeProfile passes and
// classifies the stack. Requires at least `long` candles; if fewer are
// available it uses what it can and degrades gracefully (zero values).
//
// Threshold for "flat" is 0.5% drift across the full window — enough to
// shrug off micro-noise on 1h+ TFs without missing genuine regime shifts.
func ComputePOCMigration(candles []market.Candle, short, med, long int) POCMigration {
	var m POCMigration
	if len(candles) < short || short <= 0 || med <= 0 || long <= 0 {
		return m
	}
	pocFor := func(n int) float64 {
		if len(candles) < n {
			return 0
		}
		vp := BuildVolumeProfile(candles[len(candles)-n:], 80, 1)
		return vp.POC
	}
	m.POCShort = pocFor(short)
	m.POCMed = pocFor(med)
	m.POCLong = pocFor(long)
	if m.POCLong == 0 || m.POCShort == 0 {
		return m
	}
	m.DriftPct = (m.POCShort - m.POCLong) / m.POCLong
	const flatBand = 0.005
	switch {
	case m.DriftPct > flatBand:
		m.Trend = POCRising
	case m.DriftPct < -flatBand:
		m.Trend = POCFalling
	default:
		m.Trend = POCFlat
	}
	// Strict stack adds confirmation strength; some validators may want
	// it as a separate confidence dial.
	if m.POCMed > 0 {
		switch m.Trend {
		case POCRising:
			m.Stacked = m.POCShort > m.POCMed && m.POCMed > m.POCLong
		case POCFalling:
			m.Stacked = m.POCShort < m.POCMed && m.POCMed < m.POCLong
		}
	}
	return m
}

// NearestHVN returns the HVN closest to `price` along with its index in HVN.
// Returns 0, -1 if no HVNs.
func (vp VolumeProfile) NearestHVN(price float64) (level float64, idx int) {
	idx = -1
	best := -1.0
	for i, h := range vp.HVN {
		d := h - price
		if d < 0 {
			d = -d
		}
		if best < 0 || d < best {
			best = d
			level = h
			idx = i
		}
	}
	return
}
