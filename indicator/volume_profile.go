package indicator

import (
	"sort"

	"myFirstGo/trading/market"
)

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

	for _, c := range candles {
		if c.Volume <= 0 || c.High <= c.Low {
			continue
		}
		startBin := int((c.Low - minP) / binWidth)
		endBin := int((c.High - minP) / binWidth)
		if startBin < 0 {
			startBin = 0
		}
		if endBin >= bins {
			endBin = bins - 1
		}
		nBins := endBin - startBin + 1
		if nBins < 1 {
			nBins = 1
		}
		perBin := c.Volume / float64(nBins)
		for i := startBin; i <= endBin; i++ {
			binVol[i] += perBin
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
