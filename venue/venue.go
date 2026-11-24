// Package venue is the read-only market-data layer behind the symbol catalog
// (docs/symbol_catalog_design.md).
//
// Two questions are kept apart on purpose: what may be LOOKED AT (anything a
// registered venue lists) and what may be TRADED (the curated list in
// market/resolve.go). This package answers only the first. Nothing here can
// place an order, and the trading paths in cmd/web never consult it — they
// keep resolving through market.Resolve, so a symbol that is viewable but not
// curated is refused there by default.
//
// The MarketData interface carries only plain data (ID, Instrument, Candle,
// context) so it maps one-to-one onto proto/marketdata/v1 if the adapters ever
// move behind a gRPC market-data gateway (design §10). Not built now.
package venue

import (
	"context"
	"fmt"
	"strings"

	"github.com/henry190927/trading-bot/market"
)

// DefaultVenue is the venue of an ID written without a prefix, so the
// existing "BTC-USDT" contract codes stay valid IDs.
const DefaultVenue = "bingx"

// ID identifies an instrument across venues: "<venue>:<code>", e.g.
// "bingx:NCSKTSLA2USD-USDT" or "twse:2330".
type ID string

// ParseID normalises user input into an ID. The venue is lower-cased and the
// code upper-cased, because every caller upper-cases what it reads from a
// query string ("BINGX:NCSK…") and contract codes are upper-case anyway.
// Input without a prefix is a DefaultVenue code.
func ParseID(s string) (ID, error) {
	s = strings.TrimSpace(s)
	v, code, ok := strings.Cut(s, ":")
	if !ok {
		v, code = DefaultVenue, s
	}
	v = strings.ToLower(strings.TrimSpace(v))
	code = strings.ToUpper(strings.TrimSpace(code))
	if v == "" || code == "" {
		return "", fmt.Errorf("bad instrument id %q (want venue:code)", s)
	}
	return ID(v + ":" + code), nil
}

// MakeID builds the canonical ID for a venue's code.
func MakeID(venue, code string) ID {
	return ID(strings.ToLower(venue) + ":" + strings.ToUpper(code))
}

// Venue is the part before the colon.
func (id ID) Venue() string { v, _, _ := strings.Cut(string(id), ":"); return v }

// Code is the venue's own code, the part after the colon.
func (id ID) Code() string { _, c, _ := strings.Cut(string(id), ":"); return c }

// Class is an asset class, used for search ordering and labels.
type Class string

const (
	ClassCrypto    Class = "crypto"
	ClassUSStock   Class = "us-stock"
	ClassFX        Class = "fx"
	ClassCommodity Class = "commodity"
	ClassIndex     Class = "index"
	ClassTWStock   Class = "tw-stock"
)

// classRank is the search order within one tradability tier (design §6).
var classRank = map[Class]int{
	ClassCrypto: 0, ClassUSStock: 1, ClassCommodity: 2, ClassIndex: 3, ClassFX: 4, ClassTWStock: 5,
}

// Session is a market's regular trading hours. A nil *Session means 24/7.
type Session struct {
	TZ    string `json:"tz"`    // IANA zone, e.g. "Asia/Taipei"
	Open  string `json:"open"`  // "09:00"
	Close string `json:"close"` // "13:30"
}

// Instrument is one viewable symbol.
type Instrument struct {
	ID             ID       `json:"id"`
	Venue          string   `json:"venue"`
	Code           string   `json:"code"`    // the venue's own code
	Display        string   `json:"display"` // for people only: "TSLA", "GOLD(XAU)". Never resolved — names collide.
	Asset          string   `json:"asset,omitempty"`
	Class          Class    `json:"class"`
	Quote          string   `json:"quote"`
	PricePrecision int      `json:"price_precision"`
	Session        *Session `json:"session,omitempty"`
	// Tradable means "on THIS system's curated list", not "the exchange
	// trades it". Informational: the trading paths do not read it.
	Tradable bool `json:"tradable"`
}

// MarketData is one venue's read-only market data. One implementation per
// exchange / market; adding a venue is writing one of these and registering
// it (design §5).
type MarketData interface {
	Venue() string
	// Instruments lists every viewable instrument, delisted ones excluded.
	// The Registry caches the result; implementations need not.
	Instruments(ctx context.Context) ([]Instrument, error)
	// Klines returns CLOSED candles, oldest first — the same contract as
	// bingx.Client.Klines, so engine code can consume it unchanged.
	Klines(ctx context.Context, code string, tf market.Timeframe, limit int) ([]market.Candle, error)
	// Timeframes lists what this venue serves (free TW data is daily only).
	Timeframes() []market.Timeframe
}

// Tradability is optionally implemented by a MarketData whose instruments can
// be on the curated list. The Registry re-applies it to instruments loaded
// from the disk cache, so a symbol added to (or removed from) the list since
// the cache was written is marked correctly without waiting for a refresh.
type Tradability interface {
	IsTradable(code string) bool
}
