package signal

import (
	"myFirstGo/trading-bot/market"
	"myFirstGo/trading-bot/session"
)

// SessionVolBaseline switches the engine's three volume gates from a trailing
// 19/20-bar MEAN to a median over the same exchange-local time-of-day bucket.
//
// A/B FLAG, default off. The trailing mean on a 1h series spans nearly a whole
// day, so it always contains the cash-open bar; whether a given bar clears
// "vol > 1.5x baseline" is therefore decided substantially by its hour. The
// cash-open bar is 4.1% of bars but supplies 24-36% of every range-expansion
// fire on the stock synthetics (17% BTC, 21% XAG) — measured 2026-09-05.
//
// A/B RUN AND REJECTED 2026-09-05. Keep OFF. The bias is real; it is also
// LOAD-BEARING. The contaminated denominator works as an accidental session
// filter, and removing it costs money exactly where the volume votes actually
// fire. netR, baseline → variant:
//
//	1h    60d              90d              120d             ΣΔ      windows won
//	BTC   -3.11 → -3.65    +1.19 → -6.72    -13.80 → -12.36  -7.01   1/3
//	ETH   +1.24 → -3.23    -1.87 → -6.34    +16.12 → +14.61  -10.45  0/3
//	XAU   -6.38 → -1.32    -10.64 → -2.53   -3.98 → +1.05    +18.20  3/3
//	XAG   +1.07 → +0.20    +0.87 → +5.60    -0.76 → +4.98    +9.60   2/3
//
//	2h    60d              90d              120d
//	XAU   -8.23 → -2.59    +4.81 → +7.78    +2.77 → +7.47            3/3
//	XAG   +9.64 → +10.17   +17.74 → +11.31  +15.92 → +13.14          1/3
//
// Six cmd/gate runs, six FAILs:
//
//   - BTC/ETH 1h are simply worse. ETH loses in all three windows.
//   - XAU improves in EVERY window on both timeframes and still fails, because
//     it is improving a symbol that loses money anyway ("better than a
//     disaster is not an edge" — the absolute floor from package shipgate).
//   - XAG 1h turns positive in all three windows but beats the baseline in
//     only two, and its thinnest window is 10 trades.
//   - XAG 2h — the one metals edge actually being traded — gets WORSE:
//     90d +17.74 → +11.31, 120d +15.92 → +13.14.
//
// Reproduce with `go run ./cmd/backtest -tf=1h -days=N [-session-vol]`. The
// arms were confirmed non-inert before the numbers were trusted: over 880 bars
// the flag flips Side on 45 BTC / 89 XAG / 20 SNDK bars.
//
// What stays useful: session.SessionRelVol and TimeOfDayBucket as measurement
// substrate (the open-bar strategy candidate needs them), and the reason
// strings now naming which baseline produced a fire.
var SessionVolBaseline = false

// sessionVolMinSamples is the per-bucket history required before the session
// baseline is trusted. Below it the engine keeps the trailing mean, which
// makes the flag a deliberate no-op on short timeframes (5m has 288 buckets a
// day — a 300-bar window leaves about one sample each) instead of a silent
// randomiser.
const sessionVolMinSamples = 5

// relVolFor returns the volume ratio for candles[i] under whichever baseline
// is selected, along with a label for the reason string so a backtest log
// says which arm produced a fire.
//
// The fallback is the OLD behaviour, byte-for-byte, including its window
// arithmetic: with the flag off, or on a bucket too thin to measure, callers
// must see exactly what they saw before.
func relVolFor(candles []market.Candle, i int, trailing float64) (float64, string) {
	if !SessionVolBaseline {
		return trailing, "20-bar avg"
	}
	rv, ok := session.SessionRelVol(candles, i, sessionVolMinSamples)
	if !ok {
		return trailing, "20-bar avg"
	}
	return rv, "session median"
}
