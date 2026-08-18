package signal

import (
	"testing"

	"myFirstGo/trading-bot/market"
)

// Locks in the forward-log stock treatment: SNDK→veto, NVDA→zone (their
// respective 2026-08-15 A/B survivors), and the cross-negatives.
func TestStockPerSymbolTreatment(t *testing.T) {
	if !isStructureVetoSymbol(market.SNDKUSDT) {
		t.Error("SNDK should get the structure veto")
	}
	if isStructureVetoSymbol(market.NVDAUSDT) {
		t.Error("NVDA should NOT get the veto (it gets the zone)")
	}
	if !isStructureZoneVoteSymbol(market.NVDAUSDT) {
		t.Error("NVDA should get the zone vote")
	}
	if isStructureZoneVoteSymbol(market.SNDKUSDT) {
		t.Error("SNDK should NOT get the zone vote (it gets the veto)")
	}
	// Core-4 treatment unchanged.
	if !isStructureVetoSymbol(market.XAUUSDT) || !isStructureZoneVoteSymbol(market.BTCUSDT) {
		t.Error("core-4 treatment regressed (XAU veto / BTC zone)")
	}
	if shortName(market.SNDKUSDT) != "SNDK" || shortName(market.NVDAUSDT) != "NVDA" {
		t.Error("shortName missing SNDK/NVDA")
	}
}

// Stocks must stay OUT of the daemon scan universe (forward-log discipline).
func TestStocksNotInAll(t *testing.T) {
	for _, s := range market.All() {
		if s == market.SNDKUSDT || s == market.NVDAUSDT {
			t.Errorf("%s must NOT be in market.All() (daemon scan) — forward-log only", s)
		}
	}
}
