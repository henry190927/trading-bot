package bingx

import (
	"time"

	"myFirstGo/trading-bot/market"
)

// dropForming returns candles minus a trailing still-forming bar.
//
// BingX (like most exchanges) includes the current open bar in
// /openApi/swap/v3/quote/klines responses. The engine assumes every candle
// it sees is closed: range-expansion vote reads bar.High - bar.Low and
// bar.Volume on `last`; MACD cross compares [last] vs [last-1]; sweep
// detection requires SweepIdx == last. With a partial bar at the tail,
// these mechanics fire (or fail to fire) on incomplete data — live behavior
// diverges from the backtest, which operates on historical fully-closed
// candles.
//
// A candle is "forming" if its CloseTime is strictly after `now`. At the
// exact close instant we keep it (the bar has just closed). Trims at most
// one trailing bar — an upstream bug producing multiple un-closed bars
// won't silently eat real data.
func dropForming(candles []market.Candle, now time.Time) []market.Candle {
	if len(candles) == 0 {
		return candles
	}
	if candles[len(candles)-1].CloseTime.After(now) {
		return candles[:len(candles)-1]
	}
	return candles
}
