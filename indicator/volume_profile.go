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
// strong S/R levels.
type VolumeProfile struct {
	Bins     []VolumeBin
	POC      float64   // price of point of control
	HVN      []float64 // top-N HVN price midpoints, sorted by volume desc
	PriceMin float64
	PriceMax float64
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
