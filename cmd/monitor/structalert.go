package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/notify"
	sig "github.com/henry190927/trading-bot/signal"
)

// runStructureAlerts pushes an ntfy when a symbol's N-字 structure flips on a
// regime TF (1h/4h) — a fresh BOS (continuation) or CHoCH (reversal warning).
// This is the gap 8/19 exposed: the confluence scan alerts on engine SCORE, not
// on the structure turning. Closed-bar (client.Klines) to avoid repaint; fires
// once per distinct event; the first pass SEEDS current state without alerting
// so a restart doesn't spam.
func runStructureAlerts(ctx context.Context, client interface {
	Klines(context.Context, market.Symbol, market.Timeframe, int) ([]market.Candle, error)
}) {
	topic := os.Getenv("NTFY_TOPIC")
	if topic == "" {
		log.Printf("structalert: NTFY_TOPIC unset — structure alerts disabled")
		return
	}
	n := notify.NewNtfy(os.Getenv("NTFY_SERVER"), topic)
	tfs := []market.Timeframe{market.TF1h, market.TF4h}
	last := map[string]string{} // "sym|tf" -> last event signature
	seeded := false
	shortOf := map[market.Symbol]string{market.BTCUSDT: "BTC", market.ETHUSDT: "ETH", market.XAUUSDT: "XAU", market.XAGUSDT: "XAG"}

	desc := func(e sig.StructEventKind) (string, string) {
		switch e {
		case sig.EvBOSUp:
			return "BOS-up", "續勢向上"
		case sig.EvBOSDown:
			return "BOS-down", "續勢向下"
		case sig.EvCHoCHUp:
			return "CHoCH-up", "轉多警訊"
		case sig.EvCHoCHDown:
			return "CHoCH-down", "轉空警訊"
		}
		return "", ""
	}

	check := func() {
		for _, symbol := range market.All() {
			short := shortOf[symbol]
			if short == "" {
				short = string(symbol)
			}
			for _, tf := range tfs {
				candles, err := client.Klines(ctx, symbol, tf, 200)
				if err != nil || len(candles) < 60 {
					continue
				}
				st := sig.AnalyzeStructure(candles, 2)
				if st.Event == sig.EvNone {
					continue
				}
				key := short + "|" + string(tf)
				sigStr := fmt.Sprintf("%s@%.4f", st.Event.String(), st.EventPrice)
				if last[key] == sigStr {
					continue
				}
				last[key] = sigStr
				if !seeded {
					continue // first pass: seed current state, don't alert
				}
				label, zh := desc(st.Event)
				if label == "" {
					continue
				}
				title := fmt.Sprintf("🔀 %s %s %s", short, tf, label)
				body := fmt.Sprintf("%s %s 結構轉變:%s(%s)@ %.4f — regime flip, 重看方向", short, tf, label, zh, st.EventPrice)
				tags := "twisted_rightwards_arrows"
				if st.Event == sig.EvCHoCHDown || st.Event == sig.EvBOSDown {
					tags = "twisted_rightwards_arrows,red_circle"
				} else {
					tags = "twisted_rightwards_arrows,green_circle"
				}
				if err := n.Push(ctx, title, body, tags); err != nil {
					log.Printf("structalert: push failed: %v", err)
				} else {
					log.Printf("structalert: fired %s %s %s @ %.4f", short, tf, label, st.EventPrice)
				}
			}
		}
		seeded = true
	}

	log.Printf("structalert: up — core symbols on 1h/4h (BOS/CHoCH regime flips)")
	check()
	tick := time.NewTicker(3 * time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			check()
		}
	}
}
