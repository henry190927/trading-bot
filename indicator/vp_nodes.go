package indicator

import "math"

// Node is one "mountain" in a volume profile: a contiguous price range whose
// volume stands clearly above its surroundings. See docs/vpbt_design.md §2.
type Node struct {
	Low    float64 // PriceLow of the first bin in the node
	High   float64 // PriceHigh of the last bin in the node
	POC    float64 // PriceMid of the bin with the most RAW volume in the node
	Volume float64 // sum of RAW bin volumes in the node
}

// NodeOpts tunes FindNodes. The first backtest round runs DefaultNodeOpts only.
type NodeOpts struct {
	Smooth        int     // centered moving-average window in bins; 1 = no smoothing
	ProminencePct float64 // P: a peak must be >= P × the highest smoothed bin
	EdgePct       float64 // E: a node ends where volume drops below E × its peak
	ValleyDepth   float64 // D: two peaks are separate nodes only if valley / lower peak < D
}

// DefaultNodeOpts are the design-doc defaults (Smooth 3, P 25%, E 40%, D 0.5).
func DefaultNodeOpts() NodeOpts {
	return NodeOpts{Smooth: 3, ProminencePct: 0.25, EdgePct: 0.40, ValleyDepth: 0.5}
}

// FindNodes splits a volume profile into nodes ("mountains"), sorted by Low
// ascending. It returns nil when the profile has no bins or no volume.
//
// Contract (vp_nodes_test.go checks each step):
//
//  1. Smooth. s[i] = mean of raw volume over bins [i-k, i+k], k = Smooth/2,
//     using only the bins that exist (edge bins average fewer values).
//     Smooth <= 1 means s = raw volume. Steps 2-4 use s; POC and Volume use
//     the raw volumes.
//
//  2. Peaks. Bin i is a peak when s[i] >= s[i-1] and s[i] > s[i+1], treating
//     bins outside the profile as -Inf (so the first and last bin can be
//     peaks), and s[i] >= ProminencePct × max(s). On a flat top the rule
//     picks the rightmost bin of the plateau.
//
//  3. Valleys and merging. For each pair of ADJACENT peaks a < b, the valley
//     is the bin with the lowest s strictly between them (leftmost on a tie).
//     If s[valley] / min(s[a], s[b]) >= ValleyDepth, a and b belong to the
//     same node. Decide every adjacent pair this way; chains merge
//     transitively (a~b and b~c make one node a..c).
//
//  4. Edges. A node runs from its leftmost peak to its rightmost peak, then
//     extends outward one bin at a time while s[j] >= EdgePct × s[peak],
//     where peak is the end peak it is extending from. It stops before
//     a bin that fails the test, before the edge of the profile, and before
//     the valley bin it shares with a neighbouring node (a valley belongs to
//     neither node).
//
//  5. Output. Low = Bins[first].PriceLow, High = Bins[last].PriceHigh,
//     POC = PriceMid of the bin with the highest raw volume in [first, last]
//     (lowest index on a tie), Volume = sum of raw volumes in [first, last].
func FindNodes(vp VolumeProfile, opts NodeOpts) []Node {
	// Step 1. Smooth
	k, binSize := opts.Smooth/2, len(vp.Bins)
	s := make([]float64, binSize)
	prefix := make([]float64, binSize+1)

	for i := range binSize {
		prefix[i+1] = prefix[i] + vp.Bins[i].Volume
	}

	for i := range binSize {
		l, r := max(0, i-k), min(binSize-1, i+k)
		s[i] = (prefix[r+1] - prefix[l]) / float64(r-l+1)
	}

	// Step 2. Peaks
	maxVol := 0.0
	for _, vol := range s {
		maxVol = max(maxVol, vol)
	}
	if maxVol == 0 {
		return nil
	}

	peaks := make([]int, 0, len(s))

	at := func(j int) float64 {
		if j < 0 || j >= binSize {
			return math.Inf(-1)
		}
		return s[j]
	}

	for i := range s {
		if s[i] >= at(i-1) && s[i] > at(i+1) && s[i] >= opts.ProminencePct*maxVol {
			peaks = append(peaks, i)
		}
	}

	// Step 3. Valleys and merging
	type mountain struct {
		leftPeak    int
		rightPeak   int
		leftValley  int
		rightValley int
	}

	// peaks is never empty here: maxVol > 0, and the rightmost bin holding
	// maxVol passes all three peak tests (>= its left, > its right or -Inf,
	// >= ProminencePct×maxVol for any ProminencePct <= 1).
	//
	// leftValley -1 and rightValley binSize are sentinels one bin outside
	// the profile, so the step 4 loops stop at the profile edge with the same
	// "next bin is not the valley" test they use between nodes.
	mountains := []mountain{}
	curr := mountain{leftPeak: peaks[0], rightPeak: peaks[0], leftValley: -1, rightValley: binSize}

	for i := 0; i < len(peaks)-1; i++ {
		a, b := peaks[i], peaks[i+1]

		// Two peaks are never adjacent: s[a] > s[a+1] rules out a+1 being a
		// peak (that needs s[a+1] >= s[a]). So a+1 <= b-1 and the valley
		// search below always has at least one bin.
		valley := a + 1
		for j := a + 1; j <= b-1; j++ {
			if s[j] < s[valley] {
				valley = j
			}
		}

		if s[valley]/min(s[a], s[b]) >= opts.ValleyDepth {
			curr.rightPeak = b
		} else {
			curr.rightValley = valley
			mountains = append(mountains, curr)
			curr = mountain{leftPeak: b, rightPeak: b, leftValley: valley}
		}
	}

	curr.rightValley = binSize
	mountains = append(mountains, curr)

	// Step 4. Edges
	// Step 5. Output
	var nodes []Node
	for _, m := range mountains {
		l, r := m.leftPeak, m.rightPeak
		// Each side grows against its own end peak, checking the NEXT bin so
		// that l and r stay on the last bin that belongs to the node.
		// traverse left
		for l-1 > m.leftValley && s[l-1] >= opts.EdgePct*s[m.leftPeak] {
			l--
		}
		// traverse right
		for r+1 < m.rightValley && s[r+1] >= opts.EdgePct*s[m.rightPeak] {
			r++
		}

		// POC and Volume come from RAW volume: smoothing only shapes the node.
		// Strict > keeps the lowest index on a tie.
		volume, pocIdx := 0.0, l
		for i := l; i <= r; i++ {
			volume += vp.Bins[i].Volume
			if vp.Bins[i].Volume > vp.Bins[pocIdx].Volume {
				pocIdx = i
			}
		}
		nodes = append(nodes, Node{
			Low:    vp.Bins[l].PriceLow,
			High:   vp.Bins[r].PriceHigh,
			POC:    vp.Bins[pocIdx].PriceMid,
			Volume: volume,
		})
	}

	return nodes
}
