package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/henry190927/trading-bot/journal"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/venue"
	"github.com/henry190927/trading-bot/venue/bingxvenue"
)

// Two resolvers, two questions (docs/symbol_catalog_design.md §3):
//
//   - resolveWebSymbol (handlers.go): "may this be TRADED?" — the curated
//     list only. Every call site that has not been deliberately changed uses
//     it, which is what keeps orders, stops, protect, autotrade and the
//     journal on the list. That default is the safety property; do not
//     widen it.
//   - s.resolveViewable (here): "may this be LOOKED AT?" — the list, then
//     the venue catalog. Only read-only paths (chart, bias, structure, and
//     later AI tools and backtests) call it. trading_paths_test.go fails if
//     a trading handler starts accepting a catalog-only symbol.

// viewSym is a symbol a read-only path may show.
type viewSym struct {
	Sym      market.Symbol // the BingX contract code the data is fetched with
	Key      string        // canonical form for URLs, selects and caches: "BTC", or "bingx:NCSKTSLA2USD-USDT"
	Display  string        // for people: "BTC", "TSLA", "GOLD(XAU)"
	Class    venue.Class   // "" when the catalog has not loaded
	Tradable bool          // on the curated list
}

// resolveViewable resolves a short name ("BTC"), a venue ID
// ("bingx:NCSKTSLA2USD-USDT", any case) or a bare BingX contract code.
// A catalog entry that is on the curated list comes back under its short
// name, so one symbol has one Key however it was asked for.
func (s *server) resolveViewable(in string) (viewSym, error) {
	in = strings.TrimSpace(in)
	if sym, ok := market.Resolve(in); ok {
		return s.tradableView(sym), nil
	}
	if s.venues == nil {
		_, err := resolveWebSymbol(in)
		return viewSym{}, err
	}
	inst, _, err := s.venues.Lookup(in)
	if err != nil {
		if s.venues.Len() == 0 {
			return viewSym{}, fmt.Errorf("unknown symbol %q (the symbol catalog has not loaded yet; list symbols: %s)", in, strings.Join(uiSymbols, " / "))
		}
		return viewSym{}, fmt.Errorf("unknown symbol %q: not on the list and not in the catalog", in)
	}
	if inst.Venue != bingxvenue.Name {
		// P4 (twse) routes through the venue's own MarketData; until then
		// every read path below fetches from the BingX client.
		return viewSym{}, fmt.Errorf("symbol %q: venue %s has no chart support yet", in, inst.Venue)
	}
	sym := market.Symbol(inst.Code)
	if market.Short(sym) != "" {
		return s.tradableView(sym), nil
	}
	return viewSym{Sym: sym, Key: string(inst.ID), Display: inst.Display, Class: inst.Class}, nil
}

func (s *server) tradableView(sym market.Symbol) viewSym {
	short := market.Short(sym)
	v := viewSym{Sym: sym, Key: short, Display: short, Tradable: true}
	if s.venues != nil {
		if inst, _, err := s.venues.Lookup(string(sym)); err == nil {
			v.Class = inst.Class
		}
	}
	return v
}

// instrumentsPath is the catalog's disk mirror: INSTRUMENTS_PATH, or
// instruments.json next to the journal (design §6, §11.1).
func instrumentsPath() string {
	if p := strings.TrimSpace(os.Getenv("INSTRUMENTS_PATH")); p != "" {
		return p
	}
	return filepath.Join(filepath.Dir(journal.DefaultPath()), "instruments.json")
}
