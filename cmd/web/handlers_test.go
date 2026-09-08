package main

import (
	"testing"

	"myFirstGo/trading-bot/market"
)

// TestUISymbolsMatchMarketResolve keeps the dropdown and the resolver from
// drifting. A name in uiSymbols that market.Resolve rejects is a form the user
// can submit and the server then refuses; a name market.Resolve accepts that
// is missing from uiSymbols is a symbol whose journal rows work but which
// cannot be selected. zone.ShortToSym drifted exactly this way and the
// symptom was cmd/protect silently declining to protect five symbols.
func TestUISymbolsMatchMarketResolve(t *testing.T) {
	for _, short := range uiSymbols {
		if _, err := resolveWebSymbol(short); err != nil {
			t.Errorf("uiSymbols has %q but resolveWebSymbol rejects it: %v", short, err)
		}
	}
	inUI := map[string]bool{}
	for _, s := range uiSymbols {
		inUI[s] = true
	}
	for _, short := range market.Shorts() {
		if !inUI[short] {
			t.Errorf("market.Resolve accepts %q but it is absent from uiSymbols — journal rows would work while the form cannot offer it", short)
		}
	}
}

// Case and whitespace tolerance is a deliberate consequence of delegating to
// market.Resolve: a symbol arriving from a query string or a hand-edited CSV
// should not fail on casing.
func TestResolveWebSymbolIsLenient(t *testing.T) {
	for _, in := range []string{"btc", " BTC ", "Btc"} {
		got, err := resolveWebSymbol(in)
		if err != nil {
			t.Errorf("resolveWebSymbol(%q) errored: %v", in, err)
			continue
		}
		if got != market.BTCUSDT {
			t.Errorf("resolveWebSymbol(%q) = %q, want %q", in, got, market.BTCUSDT)
		}
	}
	if _, err := resolveWebSymbol("DOGE"); err == nil {
		t.Error("resolveWebSymbol(\"DOGE\") should error")
	}
}
