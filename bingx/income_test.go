package bingx

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/henry190927/trading-bot/market"
)

// Verbatim from BingX on 2026-09-21, trimmed to the fields parseIncome reads.
// It contains the two symbols this whole change exists for: XRP and AKE, both
// real trades that /ops/fills could not see because neither was in uiSymbols.
const incomeReal = `[
 {"symbol":"XRP-USDT","incomeType":"REALIZED_PNL","income":"29.33550000","asset":"USDT","info":"Close Long","time":1789747714000},
 {"symbol":"XRP-USDT","incomeType":"TRADING_FEE","income":"-3.82330125","asset":"USDT","info":"Position closing fee","time":1789747714000},
 {"symbol":"AKE-USDT","incomeType":"REALIZED_PNL","income":"32.74761600","asset":"USDT","info":"Close Long","time":1789839387000},
 {"symbol":"AKE-USDT","incomeType":"FUNDING_FEE","income":"3.34340000","asset":"USDT","info":"Funding Fee","time":1789830000000},
 {"symbol":"NEAR-USDT","incomeType":"INSURANCE_CLEAR","income":"-189.57000000","asset":"USDT","info":"Liquidation","time":1789738050000},
 {"symbol":"ETH-USDT","incomeType":"REALIZED_PNL","income":"70.93080000","asset":"USDT","info":"Close Long","time":1789747714000}
]`

func TestParseIncomeReadsTheLedger(t *testing.T) {
	got, err := parseIncome(json.RawMessage(incomeReal))
	if err != nil {
		t.Fatalf("real income payload rejected: %v", err)
	}
	if len(got) != 6 {
		t.Fatalf("got %d records, want 6", len(got))
	}
	// Oldest first, so a caller can scan forward through a session.
	for i := 1; i < len(got); i++ {
		if got[i].Time.Before(got[i-1].Time) {
			t.Fatalf("records not sorted oldest-first at %d", i)
		}
	}
	// Numbers arrive as strings; a silent 0 here would understate every total.
	var pnl float64
	for _, r := range got {
		if r.Type == "REALIZED_PNL" {
			pnl += r.Income
		}
	}
	if want := 29.3355 + 32.747616 + 70.9308; math.Abs(pnl-want) > 1e-6 {
		t.Errorf("summed REALIZED_PNL = %v, want %v", pnl, want)
	}
	// A liquidation is its own ledger type and must survive as one — it is how
	// a forced close is told apart from a stop after the fact.
	var liq bool
	for _, r := range got {
		if r.Type == "INSURANCE_CLEAR" && r.Symbol == "NEAR-USDT" && r.Income == -189.57 {
			liq = true
		}
	}
	if !liq {
		t.Error("INSURANCE_CLEAR row lost")
	}
}

// The point of the whole change: discovery must surface symbols nobody put on
// a list. XRP and AKE were both invisible to /ops/fills within three days.
func TestDistinctSymbolsFindsUnlistedContracts(t *testing.T) {
	recs, err := parseIncome(json.RawMessage(incomeReal))
	if err != nil {
		t.Fatal(err)
	}
	got := distinctSymbols(recs)
	want := []market.Symbol{"AKE-USDT", "ETH-USDT", "NEAR-USDT", "XRP-USDT"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (sorted, deduped)", got, want)
		}
	}
	// AKE has no short name in market.Resolve and must still come through —
	// dropping unnamed contracts would reintroduce the bug one level down.
	if market.Short("AKE-USDT") != "" {
		t.Skip("AKE has since been given a short name; the unnamed case needs a new example")
	}
}

// A funding payment with no trade still means a position existed in the
// window, so the symbol has to be queried.
func TestDistinctSymbolsCountsFundingOnlySymbols(t *testing.T) {
	recs := []IncomeRecord{{Symbol: "HYPE-USDT", Type: "FUNDING_FEE", Income: -0.12}}
	if got := distinctSymbols(recs); len(got) != 1 || got[0] != "HYPE-USDT" {
		t.Errorf("got %v, want [HYPE-USDT]", got)
	}
	if got := distinctSymbols([]IncomeRecord{{Symbol: "", Type: "X"}}); len(got) != 0 {
		t.Errorf("a blank symbol must not become a query target, got %v", got)
	}
}

func TestParseIncomeRejectsGarbage(t *testing.T) {
	if _, err := parseIncome(json.RawMessage(`{"not":"an array"}`)); err == nil {
		t.Error("an unexpected shape must be reported, not silently read as zero trades")
	}
}
