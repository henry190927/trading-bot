package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/market"
)

func TestTrailStale(t *testing.T) {
	bar := time.Date(2026, 11, 15, 3, 59, 59, 999e6, time.UTC)
	if why := trailStale(bar, bar.Add(70*time.Second)); why != "" {
		t.Errorf("70s after the close: %q, want allowed", why)
	}
	if why := trailStale(bar, bar.Add(trailFireGrace)); why != "" {
		t.Errorf("exactly at the grace: %q, want allowed", why)
	}
	if why := trailStale(bar, bar.Add(3*time.Hour)); !strings.HasPrefix(why, "stale-signal") {
		t.Errorf("3h after the close: %q, want stale-signal", why)
	}
}

// 4h bars, M=2, a long fired on bar 4 (entry 11.5, stop 10.5) that is still
// open at bar 7 — and 300+ unrelated rows written after it.
func trailCandles() []market.Candle {
	rows := [][4]float64{{10, 11, 9, 10}, {10, 11, 9, 10}, {10, 11, 9, 10}, {10, 11, 9.5, 10},
		{10, 12, 10, 11.5}, {11.5, 13, 11, 12.8}, {12.8, 14, 12, 13.5}, {13.5, 15, 13, 14.5}}
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	cs := make([]market.Candle, len(rows))
	for i, r := range rows {
		ot := t0.Add(time.Duration(i) * 4 * time.Hour)
		cs[i] = market.Candle{OpenTime: ot, CloseTime: ot.Add(4*time.Hour - time.Millisecond), Open: r[0], High: r[1], Low: r[2], Close: r[3]}
	}
	return cs
}

func TestPaperBlockedTrailReadsWholeLog(t *testing.T) {
	t.Setenv("AUTOTRADE_PAPER_LOG", filepath.Join(t.TempDir(), "paper.jsonl"))
	cs := trailCandles()
	f := autotrade.PaperFire{Time: cs[4].CloseTime.Add(time.Minute), BarTime: cs[4].CloseTime, Symbol: "BTC", TF: "4h",
		Strategy: "htf-trend", Side: "long", Market: true, Entry: 11.5, Stop: 10.5, TrailM: 2}
	autotrade.AppendFire(f)
	for i := 0; i < 320; i++ { // other rules keep writing; the trail fire scrolls past 300
		autotrade.AppendFire(autotrade.PaperFire{Time: f.Time.Add(time.Duration(i) * time.Minute), Symbol: "ETH", Strategy: "engine"})
	}
	kc := &klineCache{ctx: context.Background(), data: map[string][]market.Candle{string(market.BTCUSDT) + "|4h": cs}}
	r := autotrade.Rule{Symbol: "BTC", Strategy: "htf-trend", TF: "4h"}
	if !paperBlocked(kc, r, cs[7].CloseTime, true) {
		t.Error("an open trail position older than 300 rows must still block its rule")
	}
	// The fixed-bracket path keeps its 300-row read: the same fire is invisible to it.
	if paperBlocked(kc, r, cs[7].CloseTime, false) {
		t.Error("the fixed-bracket path changed its read depth")
	}
	// Unscoreable (the window misses the lookback) holds the slot: fail-safe.
	kc.data[string(market.BTCUSDT)+"|4h"] = cs[4:]
	if !paperBlocked(kc, r, cs[7].CloseTime, true) {
		t.Error("an unscoreable trail fire must hold its slot, not free it")
	}
}

func TestKlineCacheTrailDepth(t *testing.T) {
	cfg := autotrade.Config{Rules: []autotrade.Rule{
		{Enabled: true, Symbol: "BTC", Strategy: "htf-trend", TF: "4h"},
		{Enabled: false, Symbol: "ETH", Strategy: "htf-trend-1d", TF: "1d"},
		{Enabled: true, Symbol: "SOL", Strategy: "engine", TF: "1h"},
	}}
	fires := []autotrade.PaperFire{{Symbol: "XRP", TF: "1d", Strategy: "htf-trend-1d", TrailM: 10}, {Symbol: "SUI", TF: "1h", Strategy: "engine"}}
	kc := newKlineCache(context.Background(), nil).withTrailDepth(cfg, fires)
	for key, want := range map[string]bool{
		string(market.BTCUSDT) + "|4h": true,  // enabled trail rule
		string(market.ETHUSDT) + "|1d": false, // disabled
		string(market.SOLUSDT) + "|1h": false, // fixed bracket keeps 300
		string(market.XRPUSDT) + "|1d": true,  // a trail fire in the book
		string(market.SUIUSDT) + "|1h": false,
	} {
		if kc.deep[key] != want {
			t.Errorf("deep[%s] = %v, want %v", key, kc.deep[key], want)
		}
	}
}
