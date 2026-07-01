package main

import (
	"context"
	"fmt"
	"math"

	"myFirstGo/trading-bot/ai"
	"myFirstGo/trading-bot/indicator"
	"myFirstGo/trading-bot/market"
	"myFirstGo/trading-bot/signal"
)

// parentTimeframes returns the parent TFs to fetch for multi-TF context
// on a given current TF. Chosen to give one "medium jump up" + one "big
// jump up" so the LLM sees both a near-parent regime read and a macro
// bias check. All symbols use the same ladder.
func parentTimeframes(tf market.Timeframe) []market.Timeframe {
	switch tf {
	case market.TF1m:
		return []market.Timeframe{market.TF15m, market.TF1h}
	case market.TF5m:
		return []market.Timeframe{market.TF30m, market.TF1h}
	case market.TF15m:
		return []market.Timeframe{market.TF1h, market.TF4h}
	case market.TF30m:
		return []market.Timeframe{market.TF1h, market.TF4h}
	case market.TF1h:
		return []market.Timeframe{market.TF4h, market.TF1d}
	case market.TF2h:
		return []market.Timeframe{market.TF4h, market.TF1d}
	case market.TF4h:
		return []market.Timeframe{market.TF1d}
	default:
		return nil
	}
}

// buildHigherTFSummaries fetches candles + evaluates the engine on each
// parent TF, returning compact snapshots for the AI context. Best-effort:
// any failed TF is silently skipped so the LLM sees only the TFs we
// could resolve rather than "TF X unavailable" filler.
func (s *server) buildHigherTFSummaries(ctx context.Context, sym market.Symbol, currentTF market.Timeframe) []ai.HigherTFSummary {
	parents := parentTimeframes(currentTF)
	if len(parents) == 0 || s.client == nil {
		return nil
	}
	out := make([]ai.HigherTFSummary, 0, len(parents))
	for _, ptf := range parents {
		candles, err := s.client.Klines(ctx, sym, ptf, 200)
		if err != nil || len(candles) < 60 {
			continue
		}
		sig := signal.Evaluate(signal.Inputs{
			Symbol: sym, Timeframe: ptf, Candles: candles,
		})
		trend, _, _ := signal.ClassifyTrendStructure(candles)
		summary := ai.HigherTFSummary{
			Timeframe:     ptf,
			Side:          sig.Side,
			Score:         sig.Score,
			MRScore:       sig.MRScore,
			MomentumScore: sig.MomentumScore,
			POCDriftPct:   sig.POCMig.DriftPct,
			POCTrend:      pocTrendStr(sig.POCMig.Trend),
			StructureNote: trend.String(),
		}
		out = append(out, summary)
	}
	return out
}

func pocTrendStr(t indicator.POCTrend) string {
	switch t {
	case indicator.POCRising:
		return "rising"
	case indicator.POCFalling:
		return "falling"
	}
	return "flat"
}

// topHVNList formats the top-5 HVN chip zones for LLM context, tagging
// which one is the POC and rendering each as "<price> (<%> above/below
// mark)". Skips entries beyond the 5th to keep context tight.
func topHVNList(hvns []float64, poc, mark float64) []string {
	if len(hvns) == 0 {
		return nil
	}
	out := make([]string, 0, 5)
	for i, h := range hvns {
		if i >= 5 {
			break
		}
		rel := ""
		if mark > 0 {
			pct := (h - mark) / mark * 100
			switch {
			case pct > 0.05:
				rel = fmt.Sprintf(" — %.2f%% above mark", pct)
			case pct < -0.05:
				rel = fmt.Sprintf(" — %.2f%% below mark", -pct)
			default:
				rel = " — at mark"
			}
		}
		pocMark := ""
		if math.Abs(h-poc) < 1e-9 {
			pocMark = " ← POC"
		}
		out = append(out, fmt.Sprintf("%.4f%s%s", h, rel, pocMark))
	}
	return out
}

// structureNoteFor returns the LH-LL / HH-HL classifier string for the
// candles at the current TF. Empty on insufficient data.
func structureNoteFor(candles []market.Candle) string {
	if len(candles) < 20 {
		return ""
	}
	trend, _, _ := signal.ClassifyTrendStructure(candles)
	return trend.String()
}
