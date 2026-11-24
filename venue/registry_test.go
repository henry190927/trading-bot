package venue

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/henry190927/trading-bot/market"
)

type fakeVenue struct {
	name     string
	list     []Instrument
	err      error
	tradable map[string]bool
}

func (f *fakeVenue) Venue() string { return f.name }
func (f *fakeVenue) Instruments(context.Context) ([]Instrument, error) {
	return f.list, f.err
}
func (f *fakeVenue) Klines(context.Context, string, market.Timeframe, int) ([]market.Candle, error) {
	return nil, nil
}
func (f *fakeVenue) Timeframes() []market.Timeframe { return nil }
func (f *fakeVenue) IsTradable(code string) bool    { return f.tradable[code] }

func inst(code, display string, class Class, tradable bool) Instrument {
	return Instrument{Venue: "bingx", Code: code, Display: display, Class: class, Quote: "USDT", Tradable: tradable}
}

func sample() []Instrument {
	return []Instrument{
		inst("NCFXEUR2USD-USDT", "EURUSD", ClassFX, false),
		inst("NCSKTSLA2USD-USDT", "TSLA", ClassUSStock, false),
		inst("TSLAX-USDT", "TSLAX", ClassCrypto, false), // a crypto token whose name starts with TSLA
		inst("BTC-USDT", "BTC", ClassCrypto, true),
		inst("NCSINASDAQ1002USD-USDT", "NASDAQ100", ClassIndex, false),
	}
}

func TestParseID(t *testing.T) {
	cases := map[string]ID{
		"bingx:NCSKTSLA2USD-USDT": "bingx:NCSKTSLA2USD-USDT",
		"BINGX:NCSKTSLA2USD-USDT": "bingx:NCSKTSLA2USD-USDT", // handlers upper-case the query
		"NCSKTSLA2USD-USDT":       "bingx:NCSKTSLA2USD-USDT", // no prefix = bingx
		" twse:2330 ":             "twse:2330",
	}
	for in, want := range cases {
		got, err := ParseID(in)
		if err != nil || got != want {
			t.Errorf("ParseID(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", ":", "bingx:", ":BTC"} {
		if _, err := ParseID(bad); err == nil {
			t.Errorf("ParseID(%q) accepted", bad)
		}
	}
	if id := ID("twse:2330"); id.Venue() != "twse" || id.Code() != "2330" {
		t.Errorf("Venue/Code = %q %q", id.Venue(), id.Code())
	}
}

func TestLookup(t *testing.T) {
	r := NewRegistry("", nil)
	r.Register(&fakeVenue{name: "bingx", list: sample()})
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	in, md, err := r.Lookup("BINGX:ncsktsla2usd-usdt")
	if err != nil || in.Display != "TSLA" || md.Venue() != "bingx" {
		t.Fatalf("Lookup: %+v %v", in, err)
	}
	if _, _, err := r.Lookup("TSLA"); !errors.Is(err, ErrNotFound) {
		t.Errorf("display name resolved (it must not — names collide): %v", err)
	}
	if _, _, err := r.Lookup("okx:BTC-USDT"); err == nil {
		t.Error("unregistered venue resolved")
	}
}

func TestSearchOrder(t *testing.T) {
	r := NewRegistry("", nil)
	r.Register(&fakeVenue{name: "bingx", list: sample()})
	r.Refresh(context.Background())

	codes := func(ins []Instrument) []string {
		var out []string
		for _, in := range ins {
			out = append(out, in.Code)
		}
		return out
	}
	// Exact "TSLA" (the stock) beats the crypto token that merely starts
	// with it, even though crypto ranks above US stocks by class.
	got := codes(r.Search("tsla", 0))
	if len(got) != 2 || got[0] != "NCSKTSLA2USD-USDT" || got[1] != "TSLAX-USDT" {
		t.Errorf("Search(tsla) = %v", got)
	}
	// Empty query: tradable first, then class order crypto → stock → index → fx.
	got = codes(r.Search("", 0))
	want := []string{"BTC-USDT", "TSLAX-USDT", "NCSKTSLA2USD-USDT", "NCSINASDAQ1002USD-USDT", "NCFXEUR2USD-USDT"}
	if len(got) != len(want) {
		t.Fatalf("Search('') = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Search('') = %v, want %v", got, want)
		}
	}
	if n := len(r.Search("", 2)); n != 2 {
		t.Errorf("limit ignored: %d", n)
	}
	// Code and asset match too.
	if got := codes(r.Search("NCSI", 0)); len(got) != 1 || got[0] != "NCSINASDAQ1002USD-USDT" {
		t.Errorf("Search(NCSI) = %v", got)
	}
}

// A failed refresh keeps the previous list; the disk mirror serves a restart
// while the venue is down, with tradability re-applied from the current list.
func TestRefreshFailureAndDiskCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instruments.json")
	v := &fakeVenue{name: "bingx", list: sample()}
	r := NewRegistry(path, nil)
	r.Register(v)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cache not written: %v", err)
	}

	v.list, v.err = nil, errors.New("down")
	if err := r.Refresh(context.Background()); err == nil {
		t.Fatal("refresh error swallowed")
	}
	if r.Len() != 5 {
		t.Fatalf("failed refresh emptied the catalog: %d", r.Len())
	}
	v.err = nil // empty list without an error is also treated as a failure
	if err := r.Refresh(context.Background()); err == nil || r.Len() != 5 {
		t.Fatalf("empty list accepted: err=%v len=%d", err, r.Len())
	}

	// Restart with the venue down. TSLA has since been added to the curated
	// list; the cache says it is not tradable, the venue says it is.
	down := &fakeVenue{name: "bingx", err: errors.New("down"),
		tradable: map[string]bool{"NCSKTSLA2USD-USDT": true, "BTC-USDT": true}}
	r2 := NewRegistry(path, nil)
	r2.Register(down)
	if err := r2.LoadCache(); err != nil {
		t.Fatal(err)
	}
	r2.Refresh(context.Background())
	in, _, err := r2.Lookup("bingx:NCSKTSLA2USD-USDT")
	if err != nil {
		t.Fatalf("cache not served: %v", err)
	}
	if !in.Tradable {
		t.Error("tradability not re-applied on cache load")
	}
}
