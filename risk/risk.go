// Package risk caps total account exposure before a new position is opened.
//
// # WHY THE EXISTING CAPS WERE NOT ENOUGH
//
// autotrade.CheckCaps already caps concurrent positions, TOTAL MARGIN and
// daily realised R, and it is genuinely enforced at the firing path. It would
// not have prevented the liquidation that motivated this package, and the
// arithmetic says so plainly. Take its caps — 4 positions / 140u total margin
// / -3.0R daily — and three trades committing 65u, 70u and 62u of margin:
//
//	A alone       65u  < 140u   allowed
//	A + B        135u  < 140u   allowed
//	B + C        132u  < 140u   allowed
//
// Every combination passes. Yet B and C together, at 125x, are 16,500u of
// exposure; against an account whose equity is of the same order as the margin
// cap itself that is roughly 118x account leverage and a 0.85% kill distance.
// Both long, on correlated assets, with no stop. A 0.76% adverse move closes
// the account.
//
// The margin cap cannot see that, because MARGIN IS LEVERAGE-DIVIDED. 132u
// of margin at 125x is 16,500u of exposure and at 5x it is 660u; the cap
// values them identically. The same blindness runs through R (leverage-
// independent by definition) and through the journal, which records margin on
// 48% of filled rows and equity on none. So the one quantity that separates a
// survivable trade from a fatal one was computed nowhere in the system.
//
// # THE METRIC
//
// Account leverage = total notional / equity, and its reciprocal, the kill
// distance: the adverse move that takes equity to zero. Kill distance is the
// form worth reading, because it is denominated in the same unit as a chart.
// For scale, over the last 500 1h bars:
//
//	BTC 1h  median range 0.518%   p90 1.096%   19% of bars exceed 0.85%
//	ETH 1h  median range 0.662%   p90 1.494%   34% of bars exceed 0.85%
//
// A 0.85% kill distance is inside one ordinary hourly candle. That is the
// whole argument for capping this rather than margin.
//
// # WHAT THIS PACKAGE DELIBERATELY DOES NOT DO
//
// It does not pick the limits. A correlated BTC+ETH pair has been kept on at
// roughly 56x account leverage after the arithmetic was laid out — that is the
// desk's call to make, not this package's. A cap of zero means UNLIMITED here,
// matching autotrade's convention, so limits arrive from config and never from
// this file's opinion.
//
// It also does not model correlation. B and C were both long on BTC and ETH,
// which is closer to one position than two, and notional summed across
// correlated legs understates the real risk. Summing is still the right
// conservative floor — it can only under-report, never over-report — but do
// not read a passing verdict as "these positions are independent".
package risk

import (
	"fmt"
	"sort"
	"strings"
)

// Exposure is the account's state right now, assembled from the exchange.
//
// EquityKnown exists because "could not read equity" and "equity is zero" are
// different facts with opposite correct handling, and conflating them is how a
// transient API error becomes either a frozen order path or an infinite
// computed leverage. A live account really has read 0.00000000.
type Exposure struct {
	Equity      float64
	EquityKnown bool
	// UsedMargin as the exchange reports it, an INDEPENDENT witness that
	// positions exist. The positions list comes from a parameterless endpoint
	// whose all-symbols behaviour was only ever verified against a flat
	// account, so a list that says "no positions" while used margin says
	// otherwise is a contradiction, not a green light. See Check.
	UsedMargin      float64
	UsedMarginKnown bool

	OpenNotional float64
	OpenCount    int
	// Legs describe the existing positions for the verdict text, e.g.
	// "BTC long 8,822u". Order does not matter; Check sorts them.
	Legs []string
}

// Limits are the ceilings. Zero means UNLIMITED for every field — an
// autotrade.json or .env written before these existed unmarshals to 0, and a
// naive `>= cap` comparison would have frozen the order path the moment this
// shipped. That bug already happened once with autotrade's caps.
type Limits struct {
	// MaxAccountLev caps (openNotional + new) / equity.
	MaxAccountLev float64
	// WarnAccountLev warns without blocking. Set below MaxAccountLev to get a
	// heads-up before the wall; set alone (with MaxAccountLev 0) to run
	// advisory-only, which is how this ships.
	WarnAccountLev float64
	// MaxNotionalUSDT caps total notional in absolute terms, for when equity
	// is large enough that a leverage cap stops binding.
	MaxNotionalUSDT float64
	// MaxConcurrent caps the number of simultaneous positions.
	MaxConcurrent int
}

// Enabled reports whether any limit is set at all.
func (l Limits) Enabled() bool {
	return l.MaxAccountLev > 0 || l.WarnAccountLev > 0 || l.MaxNotionalUSDT > 0 || l.MaxConcurrent > 0
}

// Verdict is the decision plus the arithmetic behind it, so a refusal can be
// argued with and an approval can be sanity-checked.
type Verdict struct {
	Blocked  bool
	Reason   string   // "" when not blocked
	Warnings []string // advisory; present on allowed verdicts too

	// Enforced is false when the numbers could not be computed — equity
	// unreadable, or the exchange contradicting itself. The candidate is
	// ALLOWED in that case (see Check) and Warnings says why.
	Enforced bool

	NotionalBefore float64
	NotionalAfter  float64
	LevBefore      float64
	LevAfter       float64
	// KillDistancePct is the adverse move that would zero the account with
	// the new position on. 0 when not computable.
	KillDistancePct float64
}

// Check decides whether a new position of newNotional USDT may be opened.
//
// FAILURE POSTURE: when the inputs are unusable this ALLOWS and warns, it does
// not block. That is deliberate and it is the opposite of package bracket's
// posture, for a reason worth stating: bracket's ambiguity costs a duplicate
// reduce-only order, whereas blocking here on a transient balance read would
// stop a legitimate, correctly-sized trade at the moment it needed to go on.
// A risk gate that fires on its own plumbing gets switched off, and then it
// protects nothing. Every non-enforced path is loud instead.
func Check(l Limits, e Exposure, newNotional float64) Verdict {
	v := Verdict{
		NotionalBefore: e.OpenNotional,
		NotionalAfter:  e.OpenNotional + newNotional,
	}
	if !l.Enabled() {
		v.Warnings = append(v.Warnings, "no risk limits configured — set RISK_MAX_ACCOUNT_LEV / RISK_WARN_ACCOUNT_LEV / RISK_MAX_NOTIONAL_USDT / RISK_MAX_CONCURRENT")
		return v
	}

	// Concurrency does not need equity, so it is checked first and is the one
	// limit that still binds when the balance read fails.
	if l.MaxConcurrent > 0 && e.OpenCount >= l.MaxConcurrent {
		v.Enforced = true
		v.Blocked = true
		v.Reason = fmt.Sprintf("max-concurrent: %d already open >= cap %d%s",
			e.OpenCount, l.MaxConcurrent, legsSuffix(e.Legs))
		return v
	}
	if l.MaxNotionalUSDT > 0 && v.NotionalAfter > l.MaxNotionalUSDT {
		v.Enforced = true
		v.Blocked = true
		v.Reason = fmt.Sprintf("max-notional: %.0fu open + %.0fu new = %.0fu > cap %.0fu%s",
			e.OpenNotional, newNotional, v.NotionalAfter, l.MaxNotionalUSDT, legsSuffix(e.Legs))
		return v
	}

	needsEquity := l.MaxAccountLev > 0 || l.WarnAccountLev > 0
	if !needsEquity {
		v.Enforced = true
		return v
	}

	switch {
	case !e.EquityKnown:
		v.Warnings = append(v.Warnings,
			"account equity could not be read — leverage limits NOT enforced for this order; check /ops/balance")
		return v
	case e.Equity <= 0:
		// A zero-equity account cannot open anything; BingX rejects it for
		// insufficient margin. Duplicating that here would only replace a
		// clear exchange error with a local one.
		v.Warnings = append(v.Warnings,
			fmt.Sprintf("account equity reads %.2fu — leverage limits not applicable; the exchange will reject for insufficient margin", e.Equity))
		return v
	}

	// The contradiction check. A positions list that reports nothing while the
	// exchange reports committed margin means the list is not to be trusted,
	// and the exposure sum is therefore a floor, not a total. Refusing to
	// enforce is right: the alternative is approving an order against an
	// understated book.
	if e.UsedMarginKnown && e.UsedMargin > 0 && e.OpenCount == 0 {
		v.Warnings = append(v.Warnings, fmt.Sprintf(
			"exchange reports %.2fu of used margin but returned NO positions — exposure is understated, leverage limits NOT enforced. Check /ops/verify.",
			e.UsedMargin))
		return v
	}

	v.Enforced = true
	v.LevBefore = e.OpenNotional / e.Equity
	v.LevAfter = v.NotionalAfter / e.Equity
	if v.LevAfter > 0 {
		v.KillDistancePct = 100 / v.LevAfter
	}

	if l.MaxAccountLev > 0 && v.LevAfter > l.MaxAccountLev {
		v.Blocked = true
		v.Reason = fmt.Sprintf(
			"max-account-leverage: %.0fu open + %.0fu new = %.0fu on %.2fu equity = %.1fx > cap %.1fx (kill distance %.3f%%)%s",
			e.OpenNotional, newNotional, v.NotionalAfter, e.Equity, v.LevAfter, l.MaxAccountLev, v.KillDistancePct, legsSuffix(e.Legs))
		return v
	}
	if l.WarnAccountLev > 0 && v.LevAfter > l.WarnAccountLev {
		v.Warnings = append(v.Warnings, fmt.Sprintf(
			"account leverage would be %.1fx (%.0fu on %.2fu equity) — kill distance %.3f%%, warn threshold %.1fx",
			v.LevAfter, v.NotionalAfter, e.Equity, v.KillDistancePct, l.WarnAccountLev))
	}
	return v
}

// legsSuffix renders the existing positions so a refusal names what is already
// on, rather than only the number it exceeded.
func legsSuffix(legs []string) string {
	if len(legs) == 0 {
		return ""
	}
	s := append([]string(nil), legs...)
	sort.Strings(s)
	return " [open: " + strings.Join(s, ", ") + "]"
}
