package main

// Account-exposure gate for manually-opened trades.
//
// autotrade.CheckCaps guards the paper executor's firing path and is genuinely
// enforced there. It never saw a manual order, and — more to the point — it
// would not have stopped the 2026-09-08 liquidation even if it had. Its caps
// were 4 positions / 140u TOTAL MARGIN / -3.0R daily, and the three unplanned
// trades committed 65.75u, 70.58u and 61.13u: every combination stayed under
// 140u. Yet two of them together were 16,463u of notional against 139.66u of
// equity — 117.9x, a 0.848% kill distance — and a 0.757% move closed the
// account.
//
// Margin is leverage-divided, so a margin cap cannot see exposure. This file
// prices the account in notional/equity instead, and it builds that from the
// EXCHANGE rather than from journal.csv: a position opened by hand in the
// BingX app counts against the book exactly like one this server placed.
//
// Ships warn-only. See package risk for why the limits are not chosen here.

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/risk"
)

// riskLimits reads the ceilings from the environment. Every one defaults to 0,
// which package risk treats as UNLIMITED — so an .env written before these
// existed leaves the order path exactly as it was.
//
//	RISK_MAX_ACCOUNT_LEV     hard cap on (notional/equity), e.g. 60
//	RISK_WARN_ACCOUNT_LEV    advisory threshold, e.g. 40
//	RISK_MAX_NOTIONAL_USDT   absolute notional ceiling
//	RISK_MAX_CONCURRENT      simultaneous positions
func riskLimits() risk.Limits {
	return risk.Limits{
		MaxAccountLev:   envFloat("RISK_MAX_ACCOUNT_LEV"),
		WarnAccountLev:  envFloat("RISK_WARN_ACCOUNT_LEV"),
		MaxNotionalUSDT: envFloat("RISK_MAX_NOTIONAL_USDT"),
		MaxConcurrent:   int(envFloat("RISK_MAX_CONCURRENT")),
	}
}

func envFloat(key string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(key)), 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}

// liveExposure prices the whole account from the exchange in two signed calls.
//
// Neither failure is fatal: package risk allows-and-warns on unusable input, so
// a balance hiccup degrades the gate to advisory rather than blocking a
// correctly-sized trade. The errors are folded into the Exposure's *Known
// flags instead of returned, because a partial read is still worth having —
// position count and notional bind the concurrency and notional caps even when
// equity is missing.
func (s *server) liveExposure(ctx context.Context) risk.Exposure {
	var e risk.Exposure
	if s.client == nil || s.client.APIKey == "" {
		return e
	}

	if b, err := s.client.AccountBalance(ctx); err == nil {
		if eq, ok := b.EquityOrZero(); ok {
			e.Equity, e.EquityKnown = eq, true
		}
		if b.UsedMargin != nil {
			e.UsedMargin, e.UsedMarginKnown = *b.UsedMargin, true
		}
	}

	poss, err := s.client.AllPositions(ctx)
	if err != nil {
		// Leave OpenCount at 0 but ALSO leave UsedMarginKnown as read: if the
		// exchange says margin is committed, risk.Check will spot the
		// contradiction and decline to enforce rather than approving against
		// an empty book.
		return e
	}
	for _, p := range poss {
		n := p.Notional()
		if n <= 0 {
			continue
		}
		e.OpenNotional += n
		e.OpenCount++
		e.Legs = append(e.Legs, fmt.Sprintf("%s %s %s", shortOrRaw(p.Symbol), p.Side, usdt(n)))
	}
	return e
}

// shortOrRaw prefers the journal's name for a symbol and falls back to the
// contract code, so a leg on a symbol with no short name still reads.
func shortOrRaw(sym market.Symbol) string {
	if s := market.Short(sym); s != "" {
		return s
	}
	return string(sym)
}

// usdt renders a notional with thousands separators — 16,463u is legible and
// 16463u is not, and this string ends up in a refusal a human has to read
// while deciding whether to argue with it.
func usdt(v float64) string {
	s := strconv.FormatFloat(v, 'f', 0, 64)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String() + "u"
	}
	return b.String() + "u"
}

// checkNewPosition prices a candidate order against the live account.
//
// notional is the ACTUAL notional of the order about to be sent (floored qty x
// entry), not margin x leverage — the two differ by the lot-precision floor
// and the number that goes on the exchange is the one that matters.
func (s *server) checkNewPosition(ctx context.Context, notional float64) (risk.Verdict, risk.Exposure) {
	e := s.liveExposure(ctx)
	return risk.Check(riskLimits(), e, notional), e
}

// equityAtEntry reads account equity for the journal's equity_usdt column.
// Returns 0 when unreadable; journal.AccountLeverage degrades to 0 in that
// case rather than reporting a wrong number.
func (s *server) equityAtEntry(ctx context.Context) float64 {
	if s.client == nil || s.client.APIKey == "" {
		return 0
	}
	b, err := s.client.AccountBalance(ctx)
	if err != nil {
		return 0
	}
	eq, ok := b.EquityOrZero()
	if !ok {
		return 0
	}
	return eq
}

// riskNote renders a verdict for the /journal flash banner.
func riskNote(v risk.Verdict) string {
	var parts []string
	if v.Enforced && v.LevAfter > 0 {
		parts = append(parts, fmt.Sprintf("帳戶槓桿 %.1fx · 歸零距離 %.3f%%", v.LevAfter, v.KillDistancePct))
	}
	parts = append(parts, v.Warnings...)
	return strings.Join(parts, " · ")
}
