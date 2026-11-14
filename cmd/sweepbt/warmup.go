package main

import (
	"strconv"
	"strings"
	"time"

	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/market"
)

// tfDur is a timeframe's bar length ("15m", "1h", "4h", "1d").
func tfDur(tf market.Timeframe) time.Duration {
	s := string(tf)
	if strings.HasSuffix(s, "d") {
		n, _ := strconv.Atoi(strings.TrimSuffix(s, "d"))
		return time.Duration(max(n, 1)) * 24 * time.Hour
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return time.Hour
	}
	return d
}

// inWindow keeps fires placed on bars that OPENED at or after start (the
// bar's close is after start), so -warmup bars inform pools and ATR but never
// trade. Fire generation is per bar with no carried state beyond the series
// itself, so filtering afterwards equals not generating them.
func inWindow(fires []autotrade.PaperFire, start time.Time) []autotrade.PaperFire {
	out := fires[:0:0]
	for _, f := range fires {
		if f.Time.After(start) {
			out = append(out, f)
		}
	}
	return out
}
