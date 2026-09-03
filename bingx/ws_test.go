package bingx

import (
	"bytes"
	"compress/gzip"
	"testing"
	"time"

	"myFirstGo/trading-bot/market"
)

// gz compresses like the exchange does, so the tests exercise the real path
// rather than the plaintext shortcut.
func gz(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Frames captured verbatim from the live endpoint with cmd/wsprobe.
const (
	realMarkFrame = `{"code":0,"dataType":"BTC-USDT@markPrice","data":{"e":"markPriceUpdate","E":1788443796475,"s":"BTC-USDT","p":"78716.3"}}`
	realAckFrame  = `{"id":"probe-1","code":0,"msg":"","dataType":"","data":null}`
	realStockFrm  = `{"code":0,"dataType":"NCSKSNDK2USD-USDT@markPrice","data":{"e":"markPriceUpdate","E":1788443887660,"s":"NCSKSNDK2USD-USDT","p":"1543.30"}}`
)

func TestDecodeMarkFrameGzipped(t *testing.T) {
	isPing, mp, err := DecodeMarkFrame(gz(t, realMarkFrame))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if isPing {
		t.Fatal("a price push is not a ping")
	}
	if mp == nil {
		t.Fatal("want a MarkPrice")
	}
	if mp.Symbol != market.BTCUSDT {
		t.Errorf("symbol = %q, want %q", mp.Symbol, market.BTCUSDT)
	}
	if mp.Price != 78716.3 {
		t.Errorf("price = %v, want 78716.3 (parsed from the string field)", mp.Price)
	}
	// E is milliseconds. 1788443796475 ms — assert via round-trip so the test
	// does not encode a hand-converted date I could get wrong.
	if got := mp.At.UnixMilli(); got != 1788443796475 {
		t.Errorf("At = %d ms, want 1788443796475", got)
	}
}

// Plaintext must still work — inflate detects gzip by magic rather than
// assuming it, so the day BingX stops compressing nothing breaks.
func TestDecodeMarkFramePlaintext(t *testing.T) {
	_, mp, err := DecodeMarkFrame([]byte(realMarkFrame))
	if err != nil || mp == nil {
		t.Fatalf("plaintext frame failed: mp=%v err=%v", mp, err)
	}
	if mp.Price != 78716.3 {
		t.Errorf("price = %v", mp.Price)
	}
}

// The keepalive is an application-level TEXT "Ping". Missing it is how the
// connection gets dropped, so it is the one frame that must never be
// misclassified.
func TestDecodeMarkFramePing(t *testing.T) {
	for _, raw := range [][]byte{
		gz(t, "Ping"),
		[]byte("Ping"),
		[]byte("Ping\n"),   // trailing whitespace must not defeat it
		[]byte("  Ping  "), // nor surrounding whitespace
	} {
		isPing, mp, err := DecodeMarkFrame(raw)
		if err != nil {
			t.Errorf("%q: err = %v", raw, err)
		}
		if !isPing {
			t.Errorf("%q: not recognised as the keepalive", raw)
		}
		if mp != nil {
			t.Errorf("%q: ping must not yield a price", raw)
		}
	}
}

// A subscription ack is normal traffic, not an error and not a price.
func TestDecodeMarkFrameAck(t *testing.T) {
	isPing, mp, err := DecodeMarkFrame(gz(t, realAckFrame))
	if err != nil {
		t.Fatalf("an ack must not be an error, got %v", err)
	}
	if isPing || mp != nil {
		t.Errorf("ack classified as ping=%v mp=%v", isPing, mp)
	}
}

// The US-stock synthetics ride the same socket as the crypto pairs.
func TestDecodeMarkFrameStockSynthetic(t *testing.T) {
	_, mp, err := DecodeMarkFrame(gz(t, realStockFrm))
	if err != nil || mp == nil {
		t.Fatalf("mp=%v err=%v", mp, err)
	}
	if mp.Symbol != market.SNDKUSDT {
		t.Errorf("symbol = %q, want %q", mp.Symbol, market.SNDKUSDT)
	}
	if mp.Price != 1543.30 {
		t.Errorf("price = %v, want 1543.30", mp.Price)
	}
}

// Frames for channels we did not subscribe to must be skipped quietly rather
// than killing the read loop — every other symbol shares the connection.
func TestDecodeMarkFrameOtherChannels(t *testing.T) {
	for _, s := range []string{
		`{"code":0,"dataType":"BTC-USDT@ticker","data":{"e":"24hTicker","E":1,"s":"BTC-USDT","c":"78723.2"}}`,
		`{"code":0,"dataType":"BTC-USDT@trade","data":[{"p":"1"}]}`,
		`{"code":0,"dataType":"x","data":{}}`,
	} {
		isPing, mp, err := DecodeMarkFrame([]byte(s))
		if err != nil {
			t.Errorf("%.40s: unexpected error %v", s, err)
		}
		if isPing || mp != nil {
			t.Errorf("%.40s: should be ignored, got ping=%v mp=%v", s, isPing, mp)
		}
	}
}

func TestDecodeMarkFrameErrors(t *testing.T) {
	if _, _, err := DecodeMarkFrame([]byte(`{"code":100400,"msg":"bad param","dataType":"","data":null}`)); err == nil {
		t.Error("a non-zero code must surface as an error")
	}
	if _, _, err := DecodeMarkFrame([]byte(`not json at all`)); err == nil {
		t.Error("undecodable body must error")
	}
	// A zero or unparseable price is a data error, not a silently-skipped frame:
	// feeding 0 into the hub would show BTC at zero.
	bad := `{"code":0,"dataType":"BTC-USDT@markPrice","data":{"e":"markPriceUpdate","E":1,"s":"BTC-USDT","p":"0"}}`
	if _, mp, err := DecodeMarkFrame([]byte(bad)); err == nil || mp != nil {
		t.Errorf("zero price must error, got mp=%v err=%v", mp, err)
	}
	bad2 := `{"code":0,"dataType":"BTC-USDT@markPrice","data":{"e":"markPriceUpdate","E":1,"s":"BTC-USDT","p":"abc"}}`
	if _, mp, err := DecodeMarkFrame([]byte(bad2)); err == nil || mp != nil {
		t.Errorf("unparseable price must error, got mp=%v err=%v", mp, err)
	}
	if _, _, err := DecodeMarkFrame(nil); err != nil {
		// An empty frame is meaningless but harmless; must not error.
		t.Errorf("empty frame errored: %v", err)
	}
}

func TestSubscribeMarkPricesRejectsNoSymbols(t *testing.T) {
	s := NewStream()
	if err := s.SubscribeMarkPrices(nil, nil, make(chan MarkPrice, 1)); err == nil {
		t.Error("want an error with no symbols")
	}
}

func TestNewStreamURL(t *testing.T) {
	if got := NewStream().URL; got != HostWSSwap {
		t.Errorf("URL = %q, want %q", got, HostWSSwap)
	}
}

// readTimeout has to exceed the server's ping cadence (~5s observed) or a
// healthy but quiet connection is torn down every cycle.
func TestReadTimeoutExceedsPingCadence(t *testing.T) {
	if readTimeout <= 10*time.Second {
		t.Errorf("readTimeout %s is too tight for a ~5s server ping cadence", readTimeout)
	}
}

// The reconnect schedule is exported and pure so it can be asserted rather
// than described. Jitter means the result is a RANGE, so the bounds are what
// gets tested — and the lower bound is what stops a stampede.
func TestBackoffFor(t *testing.T) {
	for _, tc := range []struct {
		attempt    int
		loMs, hiMs int64
	}{
		{0, 500, 1000},     // 1s  -> [500ms, 1s)
		{1, 1000, 2000},    // 2s  -> [1s, 2s)
		{2, 2000, 4000},    // 4s
		{3, 4000, 8000},    // 8s
		{4, 8000, 16000},   // 16s
		{5, 15000, 30000},  // capped at 30s -> [15s, 30s)
		{9, 15000, 30000},  // still capped
		{40, 15000, 30000}, // no overflow into a negative/tiny delay
		{-1, 500, 1000},    // defensive
	} {
		for i := 0; i < 40; i++ {
			got := BackoffFor(tc.attempt).Milliseconds()
			if got < tc.loMs || got >= tc.hiMs {
				t.Fatalf("BackoffFor(%d) = %dms, want [%d, %d)", tc.attempt, got, tc.loMs, tc.hiMs)
			}
		}
	}
}

// Jitter has to actually vary, or the whole point (no lockstep retries) is lost.
func TestBackoffJitterVaries(t *testing.T) {
	seen := map[int64]bool{}
	for i := 0; i < 60; i++ {
		seen[BackoffFor(5).Milliseconds()] = true
	}
	if len(seen) < 10 {
		t.Errorf("only %d distinct delays in 60 draws — jitter is not varying enough to break lockstep", len(seen))
	}
}
