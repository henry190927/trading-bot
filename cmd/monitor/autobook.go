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
// maxCooldownBars is the most conservative post-stop absorb window across the
// rules — the same choice the /ops panel makes, so both compute one number from
// per-rule settings and stay in agreement.
func maxCooldownBars(cfg autotrade.Config) int {
	out := 6
	for i, r := range cfg.Rules {
		if r.CooldownBars <= 0 {
			continue
		}
		if i == 0 || r.CooldownBars > out {
			out = r.CooldownBars
		}
	}
	return out
}

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

// logTrace prints one line per fire behind AUTOEXEC_TRACE=1, so the book's
// open= and todayR= numbers can be reconciled against the /ops row list.
//
// The two views answer different questions and are SUPPOSED to differ: /ops
// lists the outcome of every fire, while the breaker sums DEDUPED positions —
// a re-fire inside a still-held (or cooling-down) rule is absorbed and books
// no R of its own. Without this trace that difference is indistinguishable
// from a miscount, which is exactly the confusion it was added to end.
func logTrace(trace []autotrade.FireTrace, cooldownBars int) {
	log.Printf("autoexec: trace — %d fire(s), cooldown=%d bars (newest first)", len(trace), cooldownBars)
	for _, t := range trace {
		flags := ""
		if t.NewestForRule {
			flags += " newest"
		}
		if t.CountedOpen {
			flags += " OPEN-SLOT"
		}
		if t.CountedToday {
			flags += " →todayR"
		}
		if t.Unscoreable {
			flags += " UNSCOREABLE"
		}
		exit := "—"
		if !t.ExitAt.IsZero() {
			exit = t.ExitAt.UTC().Format("01-02 15:04")
		}
		log.Printf("autoexec:   %s %s/%s fired=%s bars=%d status=%s exit=%s netR=%+.2f%s",
			t.Symbol, t.TF, t.Strategy, t.FireTime.UTC().Format("01-02 15:04"),
			t.Bars, t.Status, exit, t.NetR, flags)
	}
}
