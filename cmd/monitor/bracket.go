package main

// Bracket guard — the server-side half of stop protection.
//
// THE BUG THIS EXISTS TO FIX
//
// cmd/web already knows how to attach a reduce-only stop to a filled trade.
// Its only callers are the dashboard and /today HTTP handlers, so it runs when
// a browser loads a page and at no other time. Every naked position in the
// journal traces to that: #60 (2026-08-31) went naked entirely, and
// #62/#63/#64 carried a planned stop that never reached BingX because the
// fills landed while nobody was at a desk. On 2026-09-05/06 both #63 and #64
// had weekend highs travel THROUGH their planned stops with no order resting.
//
// It also never re-checked: cmd/web skips placement once StopOrderID is set,
// so a stop that was cancelled, expired or silently rejected leaves the
// journal reading "protected" forever.
//
// WHAT THIS LOOP DOES NOT DO
//
// It never writes journal.csv. cmd/web rewrites that file on every dashboard
// render, and two processes doing read-modify-write on one CSV is how a row
// disappears. No local bookkeeping is needed because state comes from the
// exchange each cycle: after a stop is placed, the next cycle simply sees it.
//
// It also does not decide, by default, that the journal's stop should become a
// live order. journal.Stop is the ANALYSIS stop — the R baseline — kept
// deliberately distinct from whatever rests on the exchange (#61 recorded
// 77,380 as its R baseline while a 77,900 profit-lock rested live). So the
// default is ALERT: say loudly that a live position has no stop, and leave the
// send to /ops/verify, which is one tap away. Two things override that and
// place the order:
//
//   - a per-trade StopAuto flag, which has always meant "place this for me"
//     and until now only worked if a browser happened to be open, and
//   - MONITOR_BRACKET_MODE=place, for a trader who wants the analysis stop to
//     be the live order on every trade.
//
// Not gated by MONITOR_ZONE_ONLY, for the same reason macrowarn is not: a
// naked-position alarm is not part of the confluence noise that flag exists to
// silence, and its absence is invisible until the night it matters.

import (
	"context"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/bracket"
	"github.com/henry190927/trading-bot/journal"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/notify"
	"github.com/henry190927/trading-bot/protect"
)

const (
	// bracketPollEvery — a fill can happen at any second and the position is
	// unprotected from that second until this loop notices. 30s is the
	// compromise against two signed calls per symbol-with-an-open-trade;
	// when the journal has no open trades the cycle makes no API calls at all.
	bracketPollEvery = 30 * time.Second
	// bracketRenagEvery — a naked position gets re-alerted on this cadence
	// until it is protected or closed. One push that arrives while the phone
	// is face-down is not a safety net.
	bracketRenagEvery = 15 * time.Minute
)

// bracketState is the per-trade memory this loop keeps IN PROCESS ONLY —
// enough to avoid re-pushing the same alarm every 30s, and nothing that would
// survive a restart, because the exchange is the state.
type bracketState struct {
	lastNagged time.Time
	alerted    bool   // an unprotected alarm is currently outstanding
	placedID   string // the stop this loop last placed, for the log
	failures   int
}

func runBracketGuard(ctx context.Context, client *bingx.Client) {
	if os.Getenv("MONITOR_BRACKET") == "0" {
		log.Printf("bracket: MONITOR_BRACKET=0 — naked-position guard disabled")
		return
	}
	if client == nil || client.APIKey == "" || client.APISecret == "" {
		log.Printf("bracket: BingX API key/secret not configured — naked-position guard disabled")
		return
	}

	mode := bracket.ModeAlert
	if strings.EqualFold(os.Getenv("MONITOR_BRACKET_MODE"), "place") {
		mode = bracket.ModePlace
	}
	var n *notify.Ntfy
	if topic := os.Getenv("NTFY_TOPIC"); topic != "" {
		n = notify.NewNtfy(os.Getenv("NTFY_SERVER"), topic)
	} else {
		// Still worth running: /ops/verify and the daemon log both surface
		// the finding. But the whole point is being told while asleep, so
		// this is a real degradation and it says so.
		log.Printf("bracket: NTFY_TOPIC unset — naked positions will be LOGGED ONLY, no push")
	}
	log.Printf("bracket: naked-position guard up — mode=%s poll=%s renag=%s (journal is read-only here)",
		mode, bracketPollEvery, bracketRenagEvery)

	state := map[int]*bracketState{}
	// Keyed "SYMBOL|side" because an orphan has no journal id to key on —
	// that is what makes it an orphan.
	orphan := map[string]*bracketState{}
	tick := time.NewTicker(bracketPollEvery)
	defer tick.Stop()
	for {
		bracketCycle(ctx, client, n, mode, state)
		orphanCycle(ctx, client, n, orphan)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// bracketCycle is one pass. Split out so a panic in one cycle (a nil position
// field from a changed payload, say) cannot take the loop down permanently.
func bracketCycle(ctx context.Context, client *bingx.Client, n *notify.Ntfy, mode bracket.Mode, state map[int]*bracketState) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("bracket: PANIC in cycle, loop continues: %v", r)
		}
	}()

	trades, err := journal.ReadAll("")
	if err != nil {
		log.Printf("bracket: read journal: %v", err)
		return
	}

	// Only open trades matter, and only their symbols get queried. A flat
	// journal costs zero API calls — this loop must be free when idle.
	open := make([]journal.Trade, 0, 4)
	syms := map[market.Symbol]bool{}
	live := map[int]bool{}
	for _, t := range trades {
		if !t.IsOpen() || t.IsNoFill() || t.Stop <= 0 {
			continue
		}
		sym, ok := market.Resolve(t.Symbol)
		if !ok {
			continue
		}
		open = append(open, t)
		syms[sym] = true
		live[t.ID] = true
	}
	// Forget trades that closed, so a reopened id cannot inherit a stale nag
	// timer.
	for id := range state {
		if !live[id] {
			delete(state, id)
		}
	}
	if len(open) == 0 {
		return
	}

	// One position + one open-orders call per symbol, shared by every trade
	// on it. A fetch failure is reported and that symbol is skipped — never
	// treated as "no position", which would read as stale, or as "no orders",
	// which would read as naked and place a duplicate stop.
	type snap struct {
		positions []bingx.Position
		orders    []bingx.OpenOrder
		err       error
	}
	snaps := map[market.Symbol]snap{}
	for sym := range syms {
		var s snap
		s.positions, s.err = client.OpenPositions(ctx, sym)
		if s.err != nil {
			s.err = fmt.Errorf("positions: %w", s.err)
			snaps[sym] = s
			continue
		}
		var oerr error
		s.orders, oerr = client.OpenOrders(ctx, sym)
		if oerr != nil {
			s.err = fmt.Errorf("open orders: %w", oerr)
		}
		snaps[sym] = s
	}

	// Deterministic order so the log reads the same way twice.
	sort.Slice(open, func(i, j int) bool { return open[i].ID < open[j].ID })

	for _, t := range open {
		sym, _ := market.Resolve(t.Symbol)
		s := snaps[sym]
		if s.err != nil {
			log.Printf("bracket: #%d %s — exchange read failed, skipping cycle: %v", t.ID, t.Symbol, s.err)
			continue
		}
		var pos *bingx.Position
		for i := range s.positions {
			if s.positions[i].Side == t.Side {
				pos = &s.positions[i]
				break
			}
		}

		d := bracket.Decide(t, pos, s.orders, mode)
		st := state[t.ID]
		if st == nil {
			st = &bracketState{}
			state[t.ID] = st
		}

		switch d.State {
		case bracket.StateSkip, bracket.StateWaiting:
			// Nothing to say. A pending limit is the normal resting state.
			continue

		case bracket.StateStale:
			// The position is gone and the CSV has not caught up. Not an
			// emergency and explicitly not actionable — placing a
			// reduce-only order with no position is what BingX rejects.
			if !st.alerted {
				log.Printf("bracket: #%d %s %s — %s (journal needs closing out)", t.ID, t.Symbol, t.Side, d.Reason)
				st.alerted = true
			}
			continue

		case bracket.StateProtected:
			if st.alerted {
				log.Printf("bracket: #%d %s %s — RESOLVED, %s", t.ID, t.Symbol, t.Side, d.Reason)
				push(ctx, n, fmt.Sprintf("🛡️ #%d %s %s 已保護", t.ID, t.Symbol, sideZH(t.Side)),
					fmt.Sprintf("交易所已有 reduce-only 停損 %g (orderId=%s)\n計畫停損 %g",
						d.LiveStopPrice, d.LiveStopID, d.PlannedStop), "shield")
				st.alerted = false
				st.failures = 0
			}
			if d.GhostStopID != "" {
				// Benign: covered, but by a different order than the CSV
				// names. Logged so a hand-replaced stop is traceable.
				log.Printf("bracket: #%d %s — journal names stop %s, exchange has %s at %g",
					t.ID, t.Symbol, d.GhostStopID, d.LiveStopID, d.LiveStopPrice)
			}
			continue

		case bracket.StateNaked:
			handleNaked(ctx, client, n, t, pos, d, st)
		}
	}
}

// handleNaked is the only path in this file that can send an order.
func handleNaked(ctx context.Context, client *bingx.Client, n *notify.Ntfy, t journal.Trade, pos *bingx.Position, d bracket.Decision, st *bracketState) {
	sym := d.Symbol
	notional := pos.Quantity * pos.EntryPrice

	if d.PlaceStop <= 0 {
		// Alert-only. Say what is exposed and where the trade said the stop
		// was, so the decision can be made from the push alone.
		if st.alerted && time.Since(st.lastNagged) < bracketRenagEvery {
			return
		}
		log.Printf("bracket: #%d %s %s NAKED — qty %g @ %g (notional %.0fu), planned stop %g, %s",
			t.ID, t.Symbol, t.Side, pos.Quantity, pos.EntryPrice, notional, d.PlannedStop, d.Reason)
		body := fmt.Sprintf("倉位 %g @ %g\n名目 %.0fu · %dx\n計畫停損 %g（未掛在交易所）\n\n到 /ops/verify 點 preview → SEND",
			pos.Quantity, pos.EntryPrice, notional, pos.Leverage, d.PlannedStop)
		if d.GhostStopID != "" {
			body = "journal 記錄 stop order " + d.GhostStopID + "，但交易所沒有這張掛單。\n\n" + body
		}
		push(ctx, n, fmt.Sprintf("🚨 #%d %s %s 無停損", t.ID, t.Symbol, sideZH(t.Side)), body, "rotating_light")
		st.alerted, st.lastNagged = true, time.Now()
		return
	}

	// Placement path. protect.BuildPlan re-derives the size from the live
	// position and sanity-checks against the MARK, not the entry — so a stop
	// that would trigger on arrival is refused rather than sent.
	fr, err := client.FundingRate(ctx, sym)
	if err != nil || fr.MarkPrice <= 0 {
		log.Printf("bracket: #%d %s — no mark price, refusing to place blind: %v", t.ID, t.Symbol, err)
		return
	}
	plan := protect.BuildPlan(pos, fr.MarkPrice, d.PlaceStop, 0)
	if !plan.OK() {
		// The common real case: price has already passed the planned stop, so
		// the order would fire the instant it lands and close at market. That
		// is a decision for a human, not for a daemon at 3am.
		if st.alerted && time.Since(st.lastNagged) < bracketRenagEvery {
			return
		}
		log.Printf("bracket: #%d %s — cannot place %g: %s", t.ID, t.Symbol, d.PlaceStop, strings.Join(plan.Faults, "; "))
		push(ctx, n, fmt.Sprintf("🚨 #%d %s %s 無停損且無法自動掛", t.ID, t.Symbol, sideZH(t.Side)),
			fmt.Sprintf("倉位 %g @ %g · 名目 %.0fu\nmark %g\n計畫停損 %g\n\n拒絕原因:\n%s\n\n需要人工決定 → /ops/verify",
				pos.Quantity, pos.EntryPrice, notional, fr.MarkPrice, d.PlaceStop, strings.Join(plan.Faults, "\n")),
			"rotating_light")
		st.alerted, st.lastNagged = true, time.Now()
		return
	}

	res := protect.Apply(ctx, client, plan)
	if len(res.Errors) > 0 || res.StopOrderID == "" {
		st.failures++
		log.Printf("bracket: #%d %s — PLACE FAILED (attempt %d): %v", t.ID, t.Symbol, st.failures, res.Errors)
		if st.failures == 1 || time.Since(st.lastNagged) >= bracketRenagEvery {
			push(ctx, n, fmt.Sprintf("🚨 #%d %s %s 停損掛單失敗", t.ID, t.Symbol, sideZH(t.Side)),
				fmt.Sprintf("倉位 %g @ %g · 名目 %.0fu\n嘗試掛 %g，第 %d 次失敗\n\n%s\n\n手動處理 → /ops/verify",
					pos.Quantity, pos.EntryPrice, notional, d.PlaceStop, st.failures, strings.Join(res.Errors, "\n")),
				"rotating_light")
			st.lastNagged = time.Now()
		}
		st.alerted = true
		return
	}

	st.placedID, st.failures = res.StopOrderID, 0
	log.Printf("bracket: #%d %s %s — PLACED reduce-only stop %g qty %g, orderId=%s",
		t.ID, t.Symbol, t.Side, d.PlaceStop, plan.Qty, res.StopOrderID)
	// The next cycle will read this back off the exchange and report
	// protected; this push is the record that the daemon acted.
	push(ctx, n, fmt.Sprintf("🛡️ #%d %s %s 自動掛上停損", t.ID, t.Symbol, sideZH(t.Side)),
		fmt.Sprintf("STOP_MARKET reduce-only\n觸發 %g · 數量 %g\norderId %s\n\n倉位 %g @ %g · 名目 %.0fu",
			d.PlaceStop, plan.Qty, res.StopOrderID, pos.Quantity, pos.EntryPrice, notional), "shield")
	st.alerted = false
}

// push is a no-op when ntfy is unconfigured, so every call site can stay
// unconditional.
func push(ctx context.Context, n *notify.Ntfy, title, body, tags string) {
	if n == nil {
		return
	}
	if err := n.Push(ctx, title, body, tags); err != nil {
		log.Printf("bracket: ntfy push failed: %v", err)
	}
}

func sideZH(side string) string {
	if side == "short" {
		return "空"
	}
	return "多"
}

// orphanCycle finds positions the EXCHANGE holds that no open journal trade
// claims, and alerts when they carry no protective stop.
//
// WHY THIS EXISTS SEPARATELY FROM bracketCycle. That function enumerates from
// the journal — `syms` is built from open trades and it returns early on
// `len(open) == 0`, which the comment there defends as "a flat journal costs
// zero API calls". On 2026-09-10 that cost real money instead: a BTC long,
// 0.1202 @ 78,264.5, 125x cross, 9,407u notional on 258.52u of equity, was
// placed outside the journal. With zero open journal rows the guard returned
// before making a single exchange call, and /ops/verify — whose own subtitle
// reads "trust the exchange, not local bookkeeping" — showed a GREEN "all
// trades protected & matched" because it had zero rows to iterate. The
// position was naked for five hours and both safety surfaces said fine.
//
// The inversion is the fix: the EXCHANGE enumerates what exists, the journal
// only EXCLUDES what another path already covers. Read-only is not the same
// as not-authoritative-for-what-exists, and that is the distinction both
// surfaces got wrong.
//
// Deliberately additive rather than a rewrite of bracketCycle: this is a live
// safety daemon and the per-trade path works for journaled trades. Cost is one
// AllPositions call per poll — the thing the old early return was avoiding,
// and worth it.
func orphanCycle(ctx context.Context, client *bingx.Client, n *notify.Ntfy, state map[string]*bracketState) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("bracket/orphan: PANIC in cycle, loop continues: %v", r)
		}
	}()

	poss, err := client.AllPositions(ctx)
	if err != nil {
		log.Printf("bracket/orphan: AllPositions failed, skipping cycle: %v", err)
		return
	}

	// Journal used ONLY to exclude. A read failure must not silence the scan:
	// with no exclusions every position looks like an orphan, which
	// over-reports rather than under-reports — the safe direction here.
	claimed := map[string]bool{}
	if trades, jerr := journal.ReadAll(""); jerr == nil {
		for _, t := range trades {
			if t.IsOpen() && !t.IsNoFill() {
				claimed[strings.ToUpper(t.Symbol)+"|"+strings.ToLower(t.Side)] = true
			}
		}
	} else {
		log.Printf("bracket/orphan: journal unreadable, treating every position as unclaimed: %v", jerr)
	}

	live := map[string]bool{}
	for _, p := range poss {
		if p.Notional() <= 0 {
			continue
		}
		// market.Short returns "" for a contract with no short name (BRENT);
		// fall back to the raw code so an unnamed symbol still gets reported
		// rather than alerting about "".
		short := market.Short(p.Symbol)
		if short == "" {
			short = string(p.Symbol)
		}
		key := strings.ToUpper(short) + "|" + strings.ToLower(p.Side)
		if claimed[key] {
			continue // bracketCycle owns this one
		}
		live[key] = true

		ords, oerr := client.OpenOrders(ctx, p.Symbol)
		if oerr != nil {
			// Never treat a failed order read as "no orders": that would read
			// as naked and cry wolf every 30s.
			log.Printf("bracket/orphan: %s open orders failed, skipping: %v", short, oerr)
			continue
		}
		protected := false
		for _, o := range ords {
			if bracket.Protects(o, &p) {
				protected = true
				break
			}
		}
		st := state[key]
		if st == nil {
			st = &bracketState{}
			state[key] = st
		}
		if protected {
			if st.alerted {
				log.Printf("bracket/orphan: %s %s — RESOLVED, a protective stop is now live", short, p.Side)
				st.alerted = false
			}
			continue
		}
		if st.alerted && time.Since(st.lastNagged) < bracketRenagEvery {
			continue
		}
		notional := p.Notional()
		log.Printf("bracket/orphan: %s %s NAKED and NOT IN JOURNAL — qty %g @ %g (notional %.0fu, %dx %s)",
			short, p.Side, p.Quantity, p.EntryPrice, notional, p.Leverage, p.MarginMode)
		push(ctx, n, fmt.Sprintf("🚨 %s %s 裸倉(未記錄在 journal)", short, sideZH(p.Side)),
			fmt.Sprintf("倉位 %g @ %g\n名目 %.0fu · %dx · %s\n交易所沒有任何保護性停損\n\n這張單不在 journal 裡,所以 /ops/verify 的逐筆檢查看不到它",
				p.Quantity, p.EntryPrice, notional, p.Leverage, p.MarginMode), "rotating_light")
		st.alerted, st.lastNagged = true, time.Now()
	}

	// Drop state for positions that no longer exist, so a reopened one cannot
	// inherit a stale nag timer.
	for k := range state {
		if !live[k] {
			delete(state, k)
		}
	}
}
