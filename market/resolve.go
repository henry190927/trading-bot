package market

import (
	"fmt"
	"sort"
	"strings"
)

// Short-name resolution for the journal ↔ exchange boundary.
//
// This is the canonical table. Three near-copies existed before: the web's
// resolveWebSymbol (complete), zone.ShortToSym (missing the five crypto alts,
// which is why cmd/protect could not protect a SOL/SUI/HYPE/NEAR/LINK
// position while the web could), and the ad-hoc shortOf map in structalert
// (majors only). A journal row names a symbol as "BTC"; every process that
// has to turn that into a contract code should agree, and disagreement here
// is invisible until the one night it matters.
//
// zone.ShortToSym is deliberately NOT replaced by this: it gates what a
// MANUAL zone may name, which is a different question from what a journal row
// may name, and conflating them once already caused a bug.

var shortToSym = map[string]Symbol{
	"BTC": BTCUSDT, "ETH": ETHUSDT, "XAU": XAUUSDT, "XAG": XAGUSDT,
	"SNDK": SNDKUSDT, "NVDA": NVDAUSDT, "SPCX": SPCXUSDT,
	"MSTR": MSTRUSDT, "APP": APPUSDT,
	"SOL": SOLUSDT, "LINK": LINKUSDT, "SUI": SUIUSDT,
	"HYPE": HYPEUSDT, "NEAR": NEARUSDT, "XRP": XRPUSDT,
}

var symToShort = func() map[Symbol]string {
	m := make(map[Symbol]string, len(shortToSym))
	for k, v := range shortToSym {
		m[v] = k
	}
	return m
}()

// Resolve turns a journal/UI short name into a contract symbol.
func Resolve(short string) (Symbol, bool) {
	s, ok := shortToSym[strings.ToUpper(strings.TrimSpace(short))]
	return s, ok
}

// ResolveErr is Resolve with an error that lists the accepted names, so a
// typo reports what WOULD have worked instead of just failing.
func ResolveErr(short string) (Symbol, error) {
	if s, ok := Resolve(short); ok {
		return s, nil
	}
	return "", fmt.Errorf("unknown symbol %q (use %s)", short, strings.Join(Shorts(), " / "))
}

// Short is the reverse: contract symbol to the name a journal row uses.
// Returns "" for a symbol with no short name (e.g. BRENTUSDT).
func Short(sym Symbol) string { return symToShort[sym] }

// Shorts lists every accepted short name, sorted, for error messages and
// tests. Not the UI dropdown order — cmd/web owns that.
func Shorts() []string {
	out := make([]string, 0, len(shortToSym))
	for k := range shortToSym {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
