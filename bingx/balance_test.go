package bingx

import "testing"

// The payload the live account actually returned on 2026-09-08 at 14:14, when
// the balance was genuinely zero. A parser that cannot tell this from a failed
// read is worse than no parser: the post-mortem turned on being able to say
// "this is a real zero".
//
// userId / shortUid are SCRUBBED placeholders — the real ones were committed
// here by accident on 2026-09-08 and removed on 2026-09-09. Nothing asserts on
// them; what this fixture has to preserve is the payload SHAPE (equity present
// and equal to "0.00000000"), not the account it came from. Keep them fake.
const realZeroPayload = `{"balance":{"userId":"1000000000000000000","asset":"USDT",` +
	`"balance":"0.00000000","equity":"0.00000000","unrealizedProfit":"0",` +
	`"realisedProfit":"0.00000000","availableMargin":"0.00000000",` +
	`"usedMargin":"0","freezedMargin":"0","shortUid":"10000001"}}`

func TestParseBalanceRealZero(t *testing.T) {
	b, err := parseBalance([]byte(realZeroPayload))
	if err != nil {
		t.Fatalf("parseBalance: %v", err)
	}
	if b.Asset != "USDT" {
		t.Errorf("Asset = %q, want USDT", b.Asset)
	}
	if b.Equity == nil {
		t.Fatal("Equity is nil on a payload that carried it — a real zero read as missing")
	}
	if *b.Equity != 0 {
		t.Errorf("Equity = %v, want 0", *b.Equity)
	}
	v, ok := b.EquityOrZero()
	if !ok {
		t.Error("EquityOrZero reported not-read on a payload that carried equity")
	}
	if v != 0 {
		t.Errorf("EquityOrZero = %v, want 0", v)
	}
}

func TestParseBalanceNonZero(t *testing.T) {
	b, err := parseBalance([]byte(`{"balance":{"asset":"USDT","balance":"336.30000000","equity":"340.11","usedMargin":"75"}}`))
	if err != nil {
		t.Fatalf("parseBalance: %v", err)
	}
	v, ok := b.EquityOrZero()
	if !ok || v != 340.11 {
		t.Errorf("EquityOrZero = (%v, %v), want (340.11, true) — equity wins over balance", v, ok)
	}
	if b.UsedMargin == nil || *b.UsedMargin != 75 {
		t.Errorf("UsedMargin = %v, want 75", b.UsedMargin)
	}
}

// Falling back to balance when equity is absent, and to nothing when both are.
func TestParseBalanceFallbacks(t *testing.T) {
	b, err := parseBalance([]byte(`{"balance":{"asset":"USDT","balance":"200"}}`))
	if err != nil {
		t.Fatalf("parseBalance: %v", err)
	}
	if v, ok := b.EquityOrZero(); !ok || v != 200 {
		t.Errorf("EquityOrZero = (%v, %v), want (200, true) via the balance fallback", v, ok)
	}

	// A flat (un-nested) shape must still populate rather than zeroing.
	flat, err := parseBalance([]byte(`{"asset":"USDT","balance":"150","equity":"151"}`))
	if err != nil {
		t.Fatalf("parseBalance flat: %v", err)
	}
	if v, ok := flat.EquityOrZero(); !ok || v != 151 {
		t.Errorf("flat shape: EquityOrZero = (%v, %v), want (151, true)", v, ok)
	}
}

// The failure that must never look like zero: a response with no balance
// fields at all has to error, so a caller gating on equity refuses to compute
// rather than computing an infinite leverage against 0.
func TestParseBalanceRejectsShapelessPayload(t *testing.T) {
	for _, p := range []string{`{}`, `{"code":100419,"msg":"IP not whitelisted"}`, `{"balance":{}}`} {
		b, err := parseBalance([]byte(p))
		if err == nil {
			t.Errorf("payload %s parsed without error", p)
		}
		if v, ok := b.EquityOrZero(); ok {
			t.Errorf("payload %s reported equity as read (%v)", p, v)
		}
	}
}
