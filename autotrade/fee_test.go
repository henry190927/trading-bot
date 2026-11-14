package autotrade

import (
	"math"
	"testing"
)

func TestWithFee(t *testing.T) {
	f := PaperFire{Side: "long", Entry: 100, Stop: 99, TP: 102, Market: true}
	// 6 bps of 100 notional = 0.06 per unit; risk 1 per unit -> 0.06R.
	tp := WithFee(f, Outcome{Status: OutTP, NetR: 2, FillPrice: 100}, 6)
	if math.Abs(tp.NetR-1.94) > 1e-12 {
		t.Errorf("tp net = %v, want 1.94", tp.NetR)
	}
	st := WithFee(f, Outcome{Status: OutStop, NetR: -1, FillPrice: 100}, 6)
	if math.Abs(st.NetR+1.06) > 1e-12 {
		t.Errorf("stop net = %v, want -1.06", st.NetR)
	}
	for _, s := range []OutcomeStatus{OutOpen, OutNoFill, OutPending, OutUnscored} {
		if got := WithFee(f, Outcome{Status: s}, 6); got.NetR != 0 {
			t.Errorf("%s paid a fee: %v", s, got.NetR)
		}
	}
	if got := WithFee(f, Outcome{Status: OutTP, NetR: 2}, 0); got.NetR != 2 {
		t.Errorf("0 bps changed NetR: %v", got.NetR)
	}
}
