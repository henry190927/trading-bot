package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"myFirstGo/trading-bot/bingx"
	"myFirstGo/trading-bot/config"
	"myFirstGo/trading-bot/market"
	"myFirstGo/trading-bot/notify"
	sig "myFirstGo/trading-bot/signal"
)

// serve runs an infinite loop that wakes shortly after every timeframe
// boundary (e.g. every 5 minutes for 5m), scans all symbols, and emits
// alerts via stdout + optional Slack. Dedup ensures we only alert once
// per (symbol, candle).
func main() {
	config.LoadDotEnv()
	tf := flag.String("tf", "1h", "timeframe to monitor")
	biasTfFlag := flag.String("bias-tf", "", "higher timeframe for MTF bias filter (empty = auto)")
	useBias := flag.Bool("bias", false, "enable MTF bias filter (off by default — backtest shows it hurts mean-reversion edge)")
	minScore := flag.Int("min-score", 3, "minimum confluence to alert on")
	sweepOnly := flag.Bool("sweep-only", false, "only alert on sweep-anchored entries")
	noMac := flag.Bool("no-mac", false, "disable macOS Notification Center banner")
	stopRefine := flag.Bool("stop-refine", false, "widen stops past HVN/equal-level obstacles (opt-in)")
	flag.Parse()

	if *stopRefine {
		sig.StopRefineEnabled = true
	}

	timeframe := market.Timeframe(*tf)
	interval := tfInterval(timeframe)
	if interval == 0 {
		log.Fatalf("unsupported timeframe %s", *tf)
	}
	biasTF := sig.DefaultBiasTF(timeframe)
	if *biasTfFlag != "" {
		biasTF = market.Timeframe(*biasTfFlag)
	}

	client := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	notifier := notify.Multi{Sinks: []notify.Notifier{notify.Stdout{}}}
	if !*noMac {
		notifier.Sinks = append(notifier.Sinks, notify.Mac{})
		log.Printf("macOS Notification Center enabled")
	}
	if topic := os.Getenv("NTFY_TOPIC"); topic != "" {
		notifier.Sinks = append(notifier.Sinks, notify.NewNtfy(os.Getenv("NTFY_SERVER"), topic))
		log.Printf("ntfy push configured for topic %q", topic)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go waitForShutdown(cancel)

	dedup := &dedupSet{seen: map[string]int64{}}

	log.Printf("serve: tf=%s bias=%v bias-tf=%s min-score=%d sweep-only=%v symbols=%v",
		*tf, *useBias, biasTF, *minScore, *sweepOnly, market.All())
	scan(ctx, client, timeframe, biasTF, *useBias, *minScore, *sweepOnly, notifier, dedup)
	for {
		next := nextBoundary(time.Now(), interval).Add(2 * time.Second)
		select {
		case <-ctx.Done():
			log.Println("shutting down")
			return
		case <-time.After(time.Until(next)):
		}
		scan(ctx, client, timeframe, biasTF, *useBias, *minScore, *sweepOnly, notifier, dedup)
	}
}

// scanResult is the per-symbol outcome of one scan iteration. Used to print
// a single heartbeat line per scan covering all symbols.
type scanResult struct {
	Symbol market.Symbol
	Side   sig.Side
	Score  int
	Anchor string // empty if no plan
	Status string // "FIRED" | "skipped: <reason>" | "error: <...>"
}

func (r scanResult) format() string {
	side := r.Side.String()
	anchor := r.Anchor
	if anchor == "" {
		anchor = "-"
	}
	return fmt.Sprintf("%s=%s(%d) %s [%s]",
		shortSymbol(r.Symbol), side, r.Score, anchor, r.Status)
}

// shortSymbol trims the long NCCO* gold/silver names down to XAU/XAG for
// log readability. BTC/ETH already short.
func shortSymbol(s market.Symbol) string {
	switch s {
	case market.XAUUSDT:
		return "XAU"
	case market.XAGUSDT:
		return "XAG"
	case market.BTCUSDT:
		return "BTC"
	case market.ETHUSDT:
		return "ETH"
	}
	return string(s)
}

func scan(ctx context.Context, client *bingx.Client, tf, biasTF market.Timeframe, useBias bool, minScore int, sweepOnly bool, n notify.Notifier, dedup *dedupSet) {
	var wg sync.WaitGroup
	results := make([]scanResult, len(market.All()))
	for i, s := range market.All() {
		wg.Add(1)
		go func(idx int, sym market.Symbol) {
			defer wg.Done()
			r := scanResult{Symbol: sym, Status: "skipped"}
			defer func() { results[idx] = r }()

			candles, err := client.Klines(ctx, sym, tf, 300)
			if err != nil {
				r.Status = "error: klines " + err.Error()
				return
			}
			bias := sig.Flat
			if useBias {
				if bc, err := client.Klines(ctx, sym, biasTF, 100); err == nil {
					bias = sig.Bias(bc)
				} else {
					log.Printf("%s: bias klines: %v", sym, err)
				}
			}
			sigCtx := sig.Context{}
			if fr, err := client.FundingRate(ctx, sym); err == nil {
				sigCtx.FundingRate = fr.Rate
			}
			if oi, err := client.OpenInterest(ctx, sym); err == nil {
				sigCtx.OpenInterest = oi
			}
			s := sig.Evaluate(sig.Inputs{
				Symbol: sym, Timeframe: tf, Candles: candles, Ctx: sigCtx, Bias: bias,
			})
			r.Side = s.Side
			r.Score = s.Score
			r.Anchor = s.Plan.Anchor

			switch {
			case s.Side == sig.Flat:
				r.Status = "skipped: flat"
				return
			case s.Score < minScore:
				r.Status = fmt.Sprintf("skipped: score %d < %d", s.Score, minScore)
				return
			case s.Plan.Entry == 0:
				r.Status = "skipped: no plan"
				return
			case sweepOnly && !s.Plan.IsSweepAnchored():
				r.Status = "skipped: not sweep-anchored"
				return
			}

			candleKey := candles[len(candles)-1].OpenTime.UnixMilli()
			if !dedup.markIfNew(string(sym), candleKey) {
				r.Status = "skipped: dedup (already alerted this bar)"
				return
			}
			if err := n.Notify(ctx, s, sigCtx); err != nil {
				r.Status = "FIRED but notify err: " + err.Error()
				return
			}
			r.Status = "FIRED ✓"
		}(i, s)
	}
	wg.Wait()

	parts := make([]string, len(results))
	for i, r := range results {
		parts[i] = r.format()
	}
	log.Printf("scan tf=%s  %s", tf, strings.Join(parts, "  "))
}

type dedupSet struct {
	mu   sync.Mutex
	seen map[string]int64
}

func (d *dedupSet) markIfNew(key string, candleTS int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seen[key] == candleTS {
		return false
	}
	d.seen[key] = candleTS
	return true
}

func nextBoundary(now time.Time, interval time.Duration) time.Time {
	t := now.Truncate(interval).Add(interval)
	return t
}

func tfInterval(tf market.Timeframe) time.Duration {
	switch tf {
	case market.TF1m:
		return time.Minute
	case market.TF5m:
		return 5 * time.Minute
	case market.TF15m:
		return 15 * time.Minute
	case market.TF1h:
		return time.Hour
	case market.TF4h:
		return 4 * time.Hour
	}
	return 0
}

func waitForShutdown(cancel context.CancelFunc) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	<-c
	cancel()
}
