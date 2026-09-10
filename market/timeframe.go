package market

import "time"

// BarDuration returns the wall-clock length of one bar on tf, or 0 for a
// timeframe this package does not define.
//
// Zero rather than a guessed fallback: callers use this to size time windows,
// and silently treating an unknown timeframe as "1h" produces a window that
// is wrong without saying so. A caller that wants a default can write one.
//
// This is the canonical mapping — it belongs next to the Timeframe constants.
// Three private copies predate it (bingx.tfDuration, cmd/serve.tfInterval,
// cmd/web.tfDurationSeconds); two of them fall back to 1h, which is why they
// are not simply swapped for this without checking each call site.
func BarDuration(tf Timeframe) time.Duration {
	switch tf {
	case TF1m:
		return time.Minute
	case TF5m:
		return 5 * time.Minute
	case TF15m:
		return 15 * time.Minute
	case TF30m:
		return 30 * time.Minute
	case TF1h:
		return time.Hour
	case TF2h:
		return 2 * time.Hour
	case TF4h:
		return 4 * time.Hour
	case TF1d:
		return 24 * time.Hour
	}
	return 0
}
