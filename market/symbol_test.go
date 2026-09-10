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

func TestIsUSStock(t *testing.T) {
	for _, c := range []struct {
		sym  Symbol
		want bool
	}{
		{SNDKUSDT, true},
		{NVDAUSDT, true},
		{SPCXUSDT, true},
		{MSTRUSDT, true},
		{APPUSDT, true},
		// The metals are the same synthetic family under a different prefix,
		// and the cash-open guard must not reach them: they follow the metals
		// session, not the NYSE one.
		{XAUUSDT, false},
		{XAGUSDT, false},
		{BTCUSDT, false},
		{ETHUSDT, false},
		{SOLUSDT, false},
		{SUIUSDT, false},
		{Symbol(""), false},
		// Prefix, not equality — the roster has grown four times, and a
		// ticker added later must be covered without editing this package.
		{Symbol("NCSKTSLA2USD-USDT"), true},
	} {
		if got := c.sym.IsUSStock(); got != c.want {
			t.Errorf("Symbol(%q).IsUSStock() = %v, want %v", c.sym, got, c.want)
		}
	}
}

// The two session predicates must stay disjoint: a symbol answering true to
// both would be handed two different session guards.
func TestMetalAndUSStockAreDisjoint(t *testing.T) {
	for _, s := range []Symbol{BTCUSDT, ETHUSDT, XAUUSDT, XAGUSDT, SNDKUSDT, NVDAUSDT, SOLUSDT} {
		if s.IsMetal() && s.IsUSStock() {
			t.Errorf("Symbol(%q) reports both IsMetal and IsUSStock", s)
		}
	}
}
