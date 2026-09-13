package ai

import (
	"strings"
	"testing"

	"github.com/henry190927/trading-bot/market"
)

func TestSymbolContext_FundamentalSection(t *testing.T) {
	in := SymbolAnalysisInputs{
		Symbol: market.Symbol("NCSKNVDA2USD-USDT"), Short: "NVDA", Timeframe: market.TF1h,
		FundLabel: "rich", FundQuality: 100, FundValuation: 27,
		FundNote: "strong quality but VERY rich — trim / sell into strength",
		FundSizeLabel: "small", FundSizeAligned: "conflict", FundSizeWhy: "a long is chasing",
	}
	msg := BuildSymbolAnalysisMessage(in)
	for _, want := range []string{
		"Fundamental layer (STOCK",
		"spot rating : rich",
		"sizing over.: small (conflict)",
		"PERMISSION + SIZE, not timing",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("context missing %q", want)
		}
	}
}

func TestSymbolContext_NoFundamentalForNonStock(t *testing.T) {
	in := SymbolAnalysisInputs{Symbol: market.BTCUSDT, Short: "BTC", Timeframe: market.TF1h}
	if strings.Contains(BuildSymbolAnalysisMessage(in), "Fundamental layer") {
		t.Error("non-stock (no FundLabel) should not render the fundamental section")
	}
}
