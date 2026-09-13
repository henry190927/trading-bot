package main

import (
	"testing"

	"github.com/henry190927/trading-bot/market"
)

// The guard's scope, pinned. The measurement behind it (package session) was
// taken on the NCSK* stock synthetics; every other symbol on the board trades
// continuously, so "be flat before the cash open" is not advice that applies
// to them. A silent widening of this predicate would put a yellow warning on
// every BTC stop on /ops/verify and tell the desk to close before an open that
// means nothing for a 24/7 pair.
func TestSessionGuardScopeIsStockSyntheticsOnly(t *testing.T) {
	for _, sym := range []market.Symbol{
		market.SNDKUSDT, market.NVDAUSDT, market.SPCXUSDT, market.MSTRUSDT, market.APPUSDT,
	} {
		if !sessionGuardApplies(sym) {
			t.Errorf("%s is a stock synthetic — the cash-open guard must apply", sym)
		}
	}

	for _, sym := range []market.Symbol{
		market.BTCUSDT, market.ETHUSDT, market.SOLUSDT, market.LINKUSDT,
		market.SUIUSDT, market.NEARUSDT, market.HYPEUSDT,
		// Metals are synthetics too, but NCCO/NCSI-family, not NCSK — they
		// have their own sessions and were never measured here.
		market.XAUUSDT, market.XAGUSDT,
	} {
		if sessionGuardApplies(sym) {
			t.Errorf("%s is not a US-stock synthetic — the guard must stay silent", sym)
		}
	}
}

// A symbol that resolves to nothing must not accidentally read as in-scope.
func TestSessionGuardIgnoresEmptySymbol(t *testing.T) {
	if sessionGuardApplies("") {
		t.Error("empty symbol must not be in scope")
	}
	if sessionGuardApplies("NCSK2USD-USDT") {
		t.Error("NCSK prefix with no ticker between prefix and suffix must not be in scope")
	}
}
