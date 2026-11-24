package bingxvenue

import (
	"context"
	"testing"

	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/venue"
)

type fakeSrc struct {
	rows    []bingx.Contract
	gotSym  market.Symbol
	candles []market.Candle
}

func (f *fakeSrc) Contracts(context.Context) ([]bingx.Contract, error) { return f.rows, nil }
func (f *fakeSrc) Klines(_ context.Context, sym market.Symbol, _ market.Timeframe, _ int) ([]market.Candle, error) {
	f.gotSym = sym
	return f.candles, nil
}

// Rows copied from the live payload on 2026-11-24 (fields trimmed).
var liveRows = []bingx.Contract{
	{Symbol: "BTC-USDT", DisplayName: "BTC-USDT", Asset: "BTC", Currency: "USDT", PricePrecision: 1, Status: 1},
	{Symbol: "NCSKTSLA2USD-USDT", DisplayName: "TSLA-USDT", Asset: "NCSKTSLA2USD", Currency: "USDT", PricePrecision: 2, Status: 1},
	{Symbol: "NCFXEUR2USD-USDT", DisplayName: "EURUSD-USDT", Asset: "NCFXEUR2USD", Currency: "USDT", Status: 1},
	{Symbol: "NCSINASDAQ1002USD-USDT", DisplayName: "NASDAQ100-USDT", Asset: "NCSINASDAQ1002USD", Currency: "USDT", Status: 1},
	{Symbol: "NCCOGOLD2USD-USDT", DisplayName: "GOLD(XAU)-USDT", Asset: "NCCOGOLD2USD", Currency: "USDT", Status: 1},
	{Symbol: "NCSKMS2USD-USDT", DisplayName: "MS-USDT", Currency: "USDT", Status: 25}, // delisted: excluded
}

func TestInstruments(t *testing.T) {
	a := &Adapter{src: &fakeSrc{rows: liveRows}}
	got, err := a.Instruments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d instruments, want 5 (status 25 excluded): %+v", len(got), got)
	}
	want := map[string]struct {
		display  string
		class    venue.Class
		tradable bool
	}{
		"BTC-USDT":               {"BTC", venue.ClassCrypto, true},
		"NCSKTSLA2USD-USDT":      {"TSLA", venue.ClassUSStock, false},
		"NCFXEUR2USD-USDT":       {"EURUSD", venue.ClassFX, false},
		"NCSINASDAQ1002USD-USDT": {"NASDAQ100", venue.ClassIndex, false},
		"NCCOGOLD2USD-USDT":      {"GOLD(XAU)", venue.ClassCommodity, true}, // = XAU on the list
	}
	for _, in := range got {
		w, ok := want[in.Code]
		if !ok {
			t.Errorf("unexpected %s", in.Code)
			continue
		}
		if in.Display != w.display || in.Class != w.class || in.Tradable != w.tradable {
			t.Errorf("%s: got display=%q class=%s tradable=%v, want %q %s %v",
				in.Code, in.Display, in.Class, in.Tradable, w.display, w.class, w.tradable)
		}
		if in.ID != venue.ID("bingx:"+in.Code) || in.Venue != "bingx" || in.Quote != "USDT" {
			t.Errorf("%s: id=%s venue=%s quote=%s", in.Code, in.ID, in.Venue, in.Quote)
		}
	}
}

func TestKlinesPassesCode(t *testing.T) {
	f := &fakeSrc{candles: []market.Candle{{Close: 1}}}
	a := &Adapter{src: f}
	cs, err := a.Klines(context.Background(), "ncsktsla2usd-usdt", market.TF1h, 10)
	if err != nil || len(cs) != 1 {
		t.Fatalf("klines: %v %v", cs, err)
	}
	if f.gotSym != "NCSKTSLA2USD-USDT" {
		t.Fatalf("code passed as %q", f.gotSym)
	}
}
