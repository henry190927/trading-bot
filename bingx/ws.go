package bingx

import (
	"context"
	"errors"

	"myFirstGo/trading/market"
)

// Stream is the WebSocket consumer. BingX uses gzip-compressed frames and
// expects Ping/Pong text frames (not protocol pings) to keep the connection
// alive — the implementation must reply "Pong" when it receives "Ping".
type Stream struct {
	URL string
}

func NewStream() *Stream {
	return &Stream{URL: HostWSSwap}
}

// SubscribeKlines pushes closed candles. Channel name: "<sym>@kline_<tf>".
func (s *Stream) SubscribeKlines(ctx context.Context, sym market.Symbol, tf market.Timeframe, out chan<- market.Candle) error {
	return errors.New("bingx.Stream.SubscribeKlines: not implemented")
}

// SubscribeTrades streams individual trade prints for CVD. Channel: "<sym>@trade".
func (s *Stream) SubscribeTrades(ctx context.Context, sym market.Symbol, out chan<- market.Trade) error {
	return errors.New("bingx.Stream.SubscribeTrades: not implemented")
}

// SubscribeDepth streams L2 snapshots/diffs for liquidity-zone detection.
func (s *Stream) SubscribeDepth(ctx context.Context, sym market.Symbol, out chan<- market.Depth) error {
	return errors.New("bingx.Stream.SubscribeDepth: not implemented")
}
