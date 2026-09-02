package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"myFirstGo/trading-bot/autotrade"
	"myFirstGo/trading-bot/bingx"
	"myFirstGo/trading-bot/market"
)

// Cross-rule book assembly for the global risk caps (autotrade.CheckCaps).
//
// The per-rule guards only ever needed to know about their own rule, so nothing
// here existed. The global caps need a view of the WHOLE book once per tick.
//
// Both paper and live fires are appended to the same log (the live branch
// records Live:true), so one source serves both modes: replaying the latest
// fire per rule against real candles gives the open book, and replaying today's
// settled fires gives realized R for the circuit breaker.
//
// Deriving the book from the log rather than from the exchange is deliberate
// even for live: the exchange knows positions but not which rule owns them, nor
// anything about a rule whose limit is still resting. A live position the log
// doesn't know about would be a human's trade, and the auto-executor should not
// spend its budget on it.

// klineCache memoises Klines per (symbol, TF) for the duration of one tick.
// Without it the book would refetch what paperBlocked already fetched.
type klineCache struct {
	ctx    context.Context
	client *bingx.Client
	data   map[string][]market.Candle
}

func newKlineCache(ctx context.Context, client *bingx.Client) *klineCache {
	return &klineCache{ctx: ctx, client: client, data: map[string][]market.Candle{}}
}

// forFire resolves candles by the fire's short symbol name, returning nil for
// a symbol the executor doesn't know.
func (k *klineCache) forFire(symbol, tf string) []market.Candle {
	sym, ok := autoSymbols[symbol]
	if !ok {
		return nil
	}
	return k.get(sym, tf)
}

// get returns candles for (sym, tf), fetching at most once per tick. A failed
// fetch is cached as nil so a broken symbol doesn't retry on every lookup.
func (k *klineCache) get(sym market.Symbol, tf string) []market.Candle {
	key := string(sym) + "|" + tf
	if cs, ok := k.data[key]; ok {
		return cs
	}
	cs, err := k.client.Klines(k.ctx, sym, market.Timeframe(tf), 300)
	if err != nil {
		cs = nil
	}
	k.data[key] = cs
	return cs
}

// candleFn resolves candles for a fire's (symbol, TF). Injected so buildBook is
// testable without an exchange — the production caller passes the tick's cache.
// haltState remembers which UTC day the breaker has already been announced for,
// so a tripped halt logs and pushes once rather than on every 60s tick.
// Re-arming is implicit: a new date has no record, and buildBook's realized-R
// window resets with it.
type haltState struct{ announcedDate string }

// announce reports whether this trip should be surfaced (first time today).
func (h *haltState) announce(now time.Time) bool {
	d := now.UTC().Format("2006-01-02")
	if h.announcedDate == d {
		return false
	}
	h.announcedDate = d
	return true
}

// logBook emits the book once per tick — the journal trail that explains why a
// fire was or wasn't taken.
func logBook(bk autotrade.Book, cfg autotrade.Config, unscored int) {
	warn := ""
	if unscored > 0 {
		warn = fmt.Sprintf(" ⚠ %d unscoreable fire(s) counted as OPEN (fail-safe)", unscored)
	}
	log.Printf("autoexec: book — open=%d/%s margin=%.0fu/%s todayR=%+.2f/%s%s",
		bk.OpenCount, capStr(float64(cfg.MaxConcurrentTotal)),
		bk.OpenMargin, capStr(cfg.MaxMarginTotalUSDT),
		bk.RealizedRToday, capStr(cfg.DailyLossHaltR), warn)
}

// capStr renders a cap, or "∞" when it is disabled (zero/unset).
func capStr(v float64) string {
	if v == 0 {
		return "∞"
	}
	return fmt.Sprintf("%g", v)
}
