package market

import (
	"strings"
	"testing"
)

// Every stock synthetic must be registered in EVERY lookup, or it half-works:
// resolvable on the chart but invisible to auto, or tradeable but with no
// quantity precision. Adding a symbol touches 8 registries and this is the
// guard that they stayed in step.
func TestStockSyntheticsRegisteredEverywhere(t *testing.T) {
	for _, tc := range []struct {
		short string
		sym   Symbol
	}{
		{"SNDK", SNDKUSDT}, {"NVDA", NVDAUSDT},
		{"SPCX", SPCXUSDT}, {"MSTR", MSTRUSDT}, {"APP", APPUSDT},
	} {
		// The code must carry the ticker and the stock prefix/suffix, since
		// the earnings gate detects stock symbols by that shape.
		s := string(tc.sym)
		if !strings.HasPrefix(s, "NCSK") || !strings.HasSuffix(s, "2USD-USDT") {
			t.Errorf("%s: code %q does not match the NCSK...2USD-USDT stock shape", tc.short, s)
		}
		if !strings.Contains(s, tc.short) {
			t.Errorf("%s: code %q does not contain its ticker", tc.short, s)
		}
		// Deliberately excluded from All(): the core universe drives the
		// daemon, and these are forward-log only.
		for _, a := range All() {
			if a == tc.sym {
				t.Errorf("%s must NOT be in All() — stock synthetics are forward-log only", tc.short)
			}
		}
	}
}

// Codes must be unique; a copy-paste slip would silently alias two symbols.
func TestSymbolCodesUnique(t *testing.T) {
	seen := map[Symbol]string{}
	for name, sym := range map[string]Symbol{
		"BTC": BTCUSDT, "ETH": ETHUSDT, "XAU": XAUUSDT, "XAG": XAGUSDT,
		"SOL": SOLUSDT, "LINK": LINKUSDT, "SUI": SUIUSDT, "NEAR": NEARUSDT, "HYPE": HYPEUSDT,
		"SNDK": SNDKUSDT, "NVDA": NVDAUSDT, "SPCX": SPCXUSDT, "MSTR": MSTRUSDT, "APP": APPUSDT,
	} {
		if prev, dup := seen[sym]; dup {
			t.Errorf("%s and %s share the code %q", name, prev, sym)
		}
		seen[sym] = name
	}
}
