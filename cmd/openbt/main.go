// Command openbt tests whether SESSION-OPEN BIAS predicts trade outcome.
// It generates sweep-reject fires (the shipped edge), dedups into one-position-
// per-rule trades, then BUCKETS each by open-alignment at fire time:
//   aligned  = long above BOTH daily+weekly open, or short below both
//   opposed  = long below both, or short above both
//   mixed    = between the two opens
// and reports netR / win% / count per bucket. If aligned >> opposed robustly
// across 60/90/120d, open-bias is a real +factor worth adding (Phase 2).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/config"
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/signal"
)

func alignRight(arr []float64, n int) []float64 {
	if len(arr) == n {
		return arr
	}
	out := make([]float64, n)
	off := n - len(arr)
	for i := range out {
		if i < off {
			if len(arr) > 0 {
				out[i] = arr[0]
			}
		} else {
			out[i] = arr[i-off]
		}
	}
	return out
}

// genSweepFires — copied from cmd/sweepbt (fixed params at the shipped sweet spot).
func genSweepFires(cs []market.Candle, atr []float64, short string, tolFrac, bufATR, rMult float64) []autotrade.PaperFire {
	var out []autotrade.PaperFire
	for i := 60; i < len(cs); i++ {
		pools := signal.FindLiquidity(cs[:i], 2, 20, tolFrac)
		bar := cs[i]
		a := atr[i]
		for _, p := range pools {
			if p.Kind == signal.EQH && bar.High > p.Hi && bar.Close < p.Lo {
				stop := bar.High + bufATR*a
				risk := stop - bar.Close
				if risk <= 0 {
					continue
				}
				out = append(out, autotrade.PaperFire{Time: bar.CloseTime, Symbol: short, TF: "1h", Strategy: "sweep-reject", Side: "short",
					Market: true, Entry: bar.Close, Stop: stop, TP: bar.Close - rMult*risk})
				break
			}
			if p.Kind == signal.EQL && bar.Low < p.Lo && bar.Close > p.Hi {
				stop := bar.Low - bufATR*a
				risk := bar.Close - stop
				if risk <= 0 {
					continue
				}
				out = append(out, autotrade.PaperFire{Time: bar.CloseTime, Symbol: short, TF: "1h", Strategy: "sweep-reject", Side: "long",
					Market: true, Entry: bar.Close, Stop: stop, TP: bar.Close + rMult*risk})
				break
			}
		}
	}
	return out
}

type bucket struct {
	n, tp, stop int
	netR        float64
}

func (b *bucket) add(o autotrade.Outcome) {
	switch o.Status {
	case autotrade.OutTP:
		b.tp++
		b.netR += o.NetR
	case autotrade.OutStop:
		b.stop++
		b.netR += o.NetR
	}
	if o.Status == autotrade.OutTP || o.Status == autotrade.OutStop {
		b.n++
	}
}
func (b bucket) line(name string) string {
	wr := 0.0
	if b.n > 0 {
		wr = float64(b.tp) / float64(b.n) * 100
	}
	return fmt.Sprintf("    %-9s n=%-3d win=%3.0f%%  netR %+7.2f", name, b.n, wr, b.netR)
}

func main() {
	config.LoadDotEnv()
	days := flag.Int("days", 90, "history window")
	flag.Parse()
	client := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	end := time.Now().UTC()
	start := end.AddDate(0, 0, -*days)
	syms := []struct {
		short string
		sym   market.Symbol
	}{{"BTC", market.BTCUSDT}, {"ETH", market.ETHUSDT}, {"SOL", market.SOLUSDT}, {"SUI", market.SUIUSDT}, {"NEAR", market.NEARUSDT}, {"LINK", market.LINKUSDT}}

	fmt.Printf("=== open-bias split · sweep-reject · %dd ===\n", *days)
	var agA, agO, agM bucket
	for _, s := range syms {
		cs, err := client.KlinesRange(context.Background(), s.sym, market.Timeframe("1h"), start, end)
		if err != nil || len(cs) < 200 {
			continue
		}
		atr := alignRight(indicator.ATR(cs, 14), len(cs))
		fires := genSweepFires(cs, atr, s.short, 0.0015, 0.15, 2.0)
		positions := autotrade.DedupFires(fires, 6, 6, time.Hour, func(f autotrade.PaperFire) autotrade.Outcome {
			return autotrade.EvaluateFire(f, cs, 6)
		})
		var al, op, mx bucket
		for _, p := range positions {
			o := signal.ComputeOpens(cs, p.Fire.Time)
			if o.Daily == 0 || o.Weekly == 0 {
				continue
			}
			px := p.Fire.Entry
			above := px > o.Daily && px > o.Weekly
			below := px < o.Daily && px < o.Weekly
			long := p.Fire.Side == "long"
			switch {
			case (long && above) || (!long && below):
				al.add(p.Outcome)
				agA.add(p.Outcome)
			case (long && below) || (!long && above):
				op.add(p.Outcome)
				agO.add(p.Outcome)
			default:
				mx.add(p.Outcome)
				agM.add(p.Outcome)
			}
		}
		fmt.Printf("%s:\n%s\n%s\n%s\n", s.short, al.line("aligned"), op.line("opposed"), mx.line("mixed"))
	}
	fmt.Printf("--- AGGREGATE ---\n%s\n%s\n%s\n", agA.line("aligned"), agO.line("opposed"), agM.line("mixed"))
}
