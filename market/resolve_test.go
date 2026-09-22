package market

import "testing"

// Both were traded before they existed here: AKE on 2026-09-19, and its
// journal row #88 could not resolve to a contract, so it could not be verified
// or protected. Resolution has to round-trip or the row stays half-supported.
func TestResolveNewAltSymbols(t *testing.T) {
	for short, want := range map[string]Symbol{"AKE": AKEUSDT, "UNI": UNIUSDT, "XRP": XRPUSDT} {
		got, ok := Resolve(short)
		if !ok || got != want {
			t.Errorf("Resolve(%q) = %v,%v — want %v", short, got, ok, want)
		}
		if back := Short(want); back != short {
			t.Errorf("Short(%v) = %q, want %q", want, back, short)
		}
	}
	// Still deliberately out of the daemon universe.
	for _, s := range All() {
		if s == AKEUSDT || s == UNIUSDT {
			t.Errorf("%v must not be in All() — forward-log only", s)
		}
	}
}
