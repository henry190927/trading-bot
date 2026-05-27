package notify

import (
	"context"
	"fmt"
	"strings"

	"myFirstGo/trading/signal"
)

// Notifier is anything that can publish a signal alert.
type Notifier interface {
	Notify(ctx context.Context, sig signal.Signal, ctxInfo signal.Context) error
}

// Multi fans out to all configured notifiers; first error wins but doesn't
// stop the rest from being called.
type Multi struct {
	Sinks []Notifier
}

func (m Multi) Notify(ctx context.Context, sig signal.Signal, ctxInfo signal.Context) error {
	var firstErr error
	for _, s := range m.Sinks {
		if err := s.Notify(ctx, sig, ctxInfo); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// formatAlert builds the human-readable body. Plain text — used by stdout
// and any future text-based sink.
func formatAlert(sig signal.Signal, ctxInfo signal.Context) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s | %s (score %d) @ %.4f\n",
		sig.Symbol, sig.Timeframe, sig.Side, sig.Score, sig.Price)
	fmt.Fprintf(&b, "funding=%.4f%%   OI=%.0f\n", ctxInfo.FundingRate*100, ctxInfo.OpenInterest)
	for _, r := range sig.Reasons {
		fmt.Fprintf(&b, "  + %s\n", r)
	}
	for _, w := range sig.Warnings {
		fmt.Fprintf(&b, "  ! %s\n", w)
	}
	if sig.Plan.Entry != 0 {
		p := sig.Plan
		fmt.Fprintf(&b, "  %s %s\n  entry  %.4f\n  stop   %.4f  (risk %.4f)\n  TP1    %.4f  (1R)\n  TP2    %.4f  (2R)\n  anchor %s\n",
			p.OrderType, sig.Side, p.Entry, p.StopLoss, p.Risk(), p.TakeProfit[0], p.TakeProfit[1], p.Anchor)
	}
	return b.String()
}
