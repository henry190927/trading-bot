package autostrat

import (
	"testing"
	"time"

	"github.com/henry190927/trading-bot/market"
)

// twoNodes is 100 bars trading 100–102 then 100 bars trading 110–112: a
// lower node, an empty gap, an upper node. The last two bars are prev and the
// signal bar; prev is part of the profile but its single-price volume is far
// below the prominence floor, so it is not a node.
func twoNodes(prevClose, barClose float64) []market.Candle {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	c := func(i int, lo, hi, cl float64) market.Candle {
		ot := t0.Add(time.Duration(i) * time.Hour)
		return market.Candle{OpenTime: ot, CloseTime: ot.Add(time.Hour - time.Millisecond), Open: cl, High: hi, Low: lo, Close: cl, Volume: 1}
	}
	var cs []market.Candle
	for i := 0; i < 100; i++ {
		cs = append(cs, c(i, 100, 102, 101))
	}
	for i := 100; i < 199; i++ {
		cs = append(cs, c(i, 110, 112, 111))
	}
	cs = append(cs, c(199, prevClose, prevClose, prevClose))
	cs = append(cs, c(200, barClose, barClose, barClose))
	return cs
}

func TestVPNodeTriggerLongIntoLowerNode(t *testing.T) {
	cs := twoNodes(104, 102.8) // prev above the 1-ATR band (103), now inside it
	tr := VPNodeTrigger(cs, 1.0, 200, true, true, DefaultVPNodeParams())
	if !tr.Fire || tr.Side != "long" || tr.Market {
		t.Fatalf("want a resting long limit, got %+v", tr)
	}
	if tr.Entry < 101 || tr.Entry > 102 {
		t.Errorf("entry %.3f: want the upper part of the 100–102 node", tr.Entry)
	}
	if tr.Stop >= 100 {
		t.Errorf("stop %.3f: want below the node's far foot (100)", tr.Stop)
	}
	// BuildVolumeProfile spreads a bar's volume evenly over every bin it
	// touches, so the bin straddling 110 (109.90–110.05) ties the full bins
	// and, as the lowest, is the POC: allow one bin below 110.
	if tr.TP < 109.85 || tr.TP > 112 {
		t.Errorf("tp %.3f: want the upper node's POC (110–112, one bin of slack)", tr.TP)
	}
}

func TestVPNodeTriggerShortIntoUpperNode(t *testing.T) {
	cs := twoNodes(108, 109.5) // prev below the band (109), now inside it
	tr := VPNodeTrigger(cs, 1.0, 200, true, true, DefaultVPNodeParams())
	if !tr.Fire || tr.Side != "short" || tr.Market {
		t.Fatalf("want a resting short limit, got %+v", tr)
	}
	if tr.Entry < 110 || tr.Entry > 111 || tr.Stop <= 112 || tr.TP < 100 || tr.TP > 102.15 {
		t.Errorf("short bracket entry %.3f stop %.3f tp %.3f: want entry in the lower part of 110–112, stop above 112, tp in 100–102", tr.Entry, tr.Stop, tr.TP)
	}
}

func TestVPNodeTriggerNeedsTheApproach(t *testing.T) {
	p := DefaultVPNodeParams()
	// prev already inside the band: not an arrival, no fire.
	if tr := VPNodeTrigger(twoNodes(102.9, 102.5), 1.0, 200, true, true, p); tr.Fire {
		t.Errorf("prev already in the band fired: %+v", tr)
	}
	// broke through the far foot: not a hold, no fire.
	if tr := VPNodeTrigger(twoNodes(104, 99), 1.0, 200, true, true, p); tr.Fire {
		t.Errorf("close below the node fired: %+v", tr)
	}
	// longs disabled by the rule's side.
	if tr := VPNodeTrigger(twoNodes(104, 102.8), 1.0, 200, false, true, p); tr.Fire {
		t.Errorf("side short-only fired: %+v", tr)
	}
}
