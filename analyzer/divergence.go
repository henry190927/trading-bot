package analyzer

type DivergenceKind int

const (
	NoDivergence DivergenceKind = iota
	BullishRegular               // price LL, oscillator HL → reversal up
	BearishRegular               // price HH, oscillator LH → reversal down
	BullishHidden                // price HL, oscillator LL → trend continuation up
	BearishHidden                // price LH, oscillator HH → trend continuation down
)

func (d DivergenceKind) String() string {
	switch d {
	case BullishRegular:
		return "bullish-regular"
	case BearishRegular:
		return "bearish-regular"
	case BullishHidden:
		return "bullish-hidden"
	case BearishHidden:
		return "bearish-hidden"
	default:
		return "none"
	}
}

type Divergence struct {
	Kind     DivergenceKind
	PivotA   int // earlier pivot index
	PivotB   int // later pivot index
	PriceA   float64
	PriceB   float64
	OscA     float64
	OscB     float64
}

// Detect scans for divergence between price and an oscillator (RSI, CVD, etc.)
// using the two most recent pivots within `lookback` bars. A pivot is a local
// extreme with `pivotWidth` bars on each side that don't exceed it.
func Detect(price, osc []float64, lookback, pivotWidth int) Divergence {
	if len(price) != len(osc) || len(price) < lookback || pivotWidth < 1 {
		return Divergence{}
	}
	start := len(price) - lookback
	if start < pivotWidth {
		start = pivotWidth
	}

	highs := findPivots(price, start, pivotWidth, true)
	lows := findPivots(price, start, pivotWidth, false)

	if len(highs) >= 2 {
		a, b := highs[len(highs)-2], highs[len(highs)-1]
		if price[b] > price[a] && osc[b] < osc[a] {
			return Divergence{BearishRegular, a, b, price[a], price[b], osc[a], osc[b]}
		}
		if price[b] < price[a] && osc[b] > osc[a] {
			return Divergence{BearishHidden, a, b, price[a], price[b], osc[a], osc[b]}
		}
	}
	if len(lows) >= 2 {
		a, b := lows[len(lows)-2], lows[len(lows)-1]
		if price[b] < price[a] && osc[b] > osc[a] {
			return Divergence{BullishRegular, a, b, price[a], price[b], osc[a], osc[b]}
		}
		if price[b] > price[a] && osc[b] < osc[a] {
			return Divergence{BullishHidden, a, b, price[a], price[b], osc[a], osc[b]}
		}
	}
	return Divergence{}
}

func findPivots(xs []float64, start, w int, high bool) []int {
	var out []int
	for i := start; i < len(xs)-w; i++ {
		isPivot := true
		for j := 1; j <= w; j++ {
			if high {
				if xs[i-j] >= xs[i] || xs[i+j] >= xs[i] {
					isPivot = false
					break
				}
			} else {
				if xs[i-j] <= xs[i] || xs[i+j] <= xs[i] {
					isPivot = false
					break
				}
			}
		}
		if isPivot {
			out = append(out, i)
		}
	}
	return out
}
