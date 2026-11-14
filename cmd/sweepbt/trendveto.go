package main

import (
	"time"

	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/signal"
)

// structLookback matches the bar count live sweep-reject reads
// (autostrat.evalSweepReject: Klines(300)), on whichever TF the veto reads.
const structLookback = 300

// A vetoFn says whether the N 字 counter-trend veto kills a fire.
type vetoFn func(f autotrade.PaperFire) bool

func fireSide(f autotrade.PaperFire) signal.Side {
	if f.Side == "short" {
		return signal.Short
	}
	return signal.Long
}

// sameTFVeto: signal.StructureVeto (the XAU/XAG engine definition — an
// opposing HH-HL/LH-LL trend OR an opposing BOS/CHoCH) on the structure of
// the 300 bars of ref ending AT the fire bar, which is closed at fire time.
// ref is the base series plus warm-up bars before the window, so the first
// fires see a full 300-bar read as live does. A fire whose bar is not in ref
// is not vetoed.
func sameTFVeto(ref []market.Candle) vetoFn {
	idx := make(map[time.Time]int, len(ref))
	for i, c := range ref {
		idx[c.CloseTime] = i
	}
	return func(f autotrade.PaperFire) bool {
		i, ok := idx[f.Time]
		return ok && signal.StructureVetoAt(ref, i, structLookback, fireSide(f))
	}
}

// htfVeto: the same definition on the higher-TF series, reading only HTF
// bars closed at or before the fire bar's OpenTime — the alignment
// backtest.precomputeHTFStruct uses — and at most the last 300 of them.
func htfVeto(base, htf []market.Candle) vetoFn {
	open := make(map[time.Time]time.Time, len(base))
	for _, c := range base {
		open[c.CloseTime] = c.OpenTime
	}
	return func(f autotrade.PaperFire) bool {
		asOf, ok := open[f.Time]
		return ok && signal.StructureVetoAsOf(htf, asOf, structLookback, fireSide(f))
	}
}

// applyTrendVeto drops the fires veto kills. Applied BEFORE DedupFires, so
// a vetoed fire frees the one-position slot for a later one, as it would
// live. It only removes fires; survivors keep their stop and target, so an
// A/B against the unfiltered set isolates the selection effect.
func applyTrendVeto(fires []autotrade.PaperFire, veto vetoFn) []autotrade.PaperFire {
	out := fires[:0:0]
	for _, f := range fires {
		if !veto(f) {
			out = append(out, f)
		}
	}
	return out
}

// scoreFires resolves fires through the live primitives: DedupFires (one
// position per rule, the BOOK) or, with noDedup, each fire on its own (the
// SIGNAL, mirroring cmd/snrbt). feeBps is charged per resolved trade via
// autotrade.WithFee; 0 leaves EvaluateFire's gross R untouched.
func scoreFires(fires []autotrade.PaperFire, cs []market.Candle, noDedup bool, feeBps float64) []autotrade.Position {
	resolve := func(f autotrade.PaperFire) autotrade.Outcome {
		return autotrade.WithFee(f, autotrade.EvaluateFire(f, cs, 6), feeBps)
	}
	if noDedup {
		var ps []autotrade.Position
		for _, f := range fires {
			ps = append(ps, autotrade.Position{Fire: f, Outcome: resolve(f)})
		}
		return ps
	}
	return autotrade.DedupFires(fires, 6, 6, time.Hour, resolve)
}
