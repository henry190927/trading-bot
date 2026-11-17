package main

import (
	"context"
	"log"
	"time"

	"github.com/henry190927/trading-bot/autostrat"
	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/market"
)

// runCandidates evaluates the candidate_only rules once per tick
// (autotrade.Rule.CandidateOnly). They are the rules that failed the fee
// re-gate, kept as a feed of take/skip candidates for the decision log.
//
// Everything the book loop does to decide WHETHER a rule would trade is done
// here the same way — trigger, one placement per bar, blackout, one position
// per rule (in the candidate log), sizing, the cash-open guard — so a
// candidate is exactly the fire the rule would have produced. Everything that
// concerns the BOOK is not: no caps, no breaker, no paper log, no ntfy, never
// live. It runs before the halt check on purpose: a halted book is no reason
// to stop measuring the trader's selection.
func runCandidates(ctx context.Context, client *bingx.Client, cfg autotrade.Config, state map[string]*autoState) {
	var rules []autotrade.Rule
	needEquity := false
	for _, r := range cfg.Rules {
		if r.Enabled && r.CandidateOnly {
			rules = append(rules, r)
			needEquity = needEquity || cfg.EffectiveRiskPct(r) > 0
		}
	}
	if len(rules) == 0 {
		return
	}
	var equity float64
	var equityOK bool
	if needEquity {
		if b, err := client.AccountBalance(ctx); err == nil {
			equity, equityOK = b.EquityOrZero()
		}
	}
	kc := newKlineCache(ctx, client)
	for _, r := range rules {
		sym, ok := autoSymbols[r.Symbol]
		if !ok {
			continue
		}
		trig := autostrat.EvalAutoTrigger(ctx, client, sym, r)
		if !trig.Fire {
			continue
		}
		cs, err := client.Klines(ctx, sym, market.Timeframe(r.TF), 3)
		if err != nil || len(cs) == 0 {
			continue
		}
		barTime := cs[len(cs)-1].CloseTime
		if !trig.BarTime.IsZero() {
			barTime = trig.BarTime
		}
		key := "cand|" + r.Symbol + "|" + r.TF + "|" + r.Strategy
		st := state[key]
		if st == nil {
			st = &autoState{}
			state[key] = st
		}
		if st.lastFireBar.Equal(barTime) {
			continue
		}
		if autoInBlackout(sym, barTime) {
			continue
		}
		if candidateBlocked(kc, r, barTime, trig.TrailM > 0) {
			continue
		}
		if trig.TrailM > 0 && trailStale(barTime, time.Now().UTC()) != "" {
			continue
		}
		sz := autotrade.Size(cfg, r, trig.Entry, trig.Stop, equity, equityOK)
		if sz.Blocked {
			continue
		}
		if why := autoSessionBlock(ctx, client, sym, r.TF, trig, time.Now().UTC()); why != "" {
			continue
		}
		st.lastFireBar = barTime
		score := autostrat.FireScore100(ctx, client, sym, r.TF, trig.Side, trig.Entry)
		autotrade.AppendCandidate(fireRecord(r, trig, sz, barTime, trig.Why, score, false))
		log.Printf("[AUTO-CANDIDATE] %s %s/%s %s @%.4f stop %.4f tp %.4f (risk %.2fu) — %s",
			r.Symbol, r.TF, r.Strategy, trig.Side, trig.Entry, trig.Stop, trig.TP, sz.RiskUSDT, trig.Why)
	}
}

// candidateBlocked is paperBlocked against the candidate log: one position
// per candidate rule, cooldown after a stop, from the same SlotBlocked rule.
func candidateBlocked(kc *klineCache, r autotrade.Rule, barTime time.Time, _ bool) bool {
	latest, ok := autotrade.NewestCandidate(r.Symbol, r.Strategy)
	if !ok {
		return false
	}
	return autotrade.SlotBlocked(latest, kc.forFire(r.Symbol, r.TF), barTime, r.CooldownBars, barDurationTF(r.TF))
}
