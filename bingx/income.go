package bingx

// Account-wide income ledger. READ ONLY — one signed GET, and it takes no
// symbol, which is the whole point of it being here.
//
// Every other history read in this package needs to be TOLD which instrument
// to ask about, so a list of symbols somewhere in the caller decides what can
// be seen. That list is always a guess about the past. On 2026-09-18 a real
// XRP position was invisible to /ops/fills because XRP was not in uiSymbols,
// and on 2026-09-19 an AKE trade was invisible for the identical reason —
// found only by reading this ledger by hand. Two in three days is not a
// missing entry, it is the wrong direction of enumeration.
//
// The same inversion that exchangeOrphans already applies to POSITIONS:
// read-only is not the same as not-authoritative-for-what-exists. Ask the
// account what it did, then go get the detail.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/henry190927/trading-bot/market"
)

// PathIncome is the account-wide ledger: realized PnL, trading fees, funding,
// and liquidation clears. Deposits and wallet transfers do NOT appear — a
// balance that moves without a matching ledger entry is a transfer, which is
// how the 2026-09-18 redeposit and a later +10.05 were identified.
const PathIncome = "/openApi/swap/v2/user/income"

// IncomeRecord is one ledger entry.
type IncomeRecord struct {
	Symbol string // contract code, e.g. "XRP-USDT"
	Type   string // REALIZED_PNL / TRADING_FEE / FUNDING_FEE / INSURANCE_CLEAR
	Income float64
	Info   string
	Time   time.Time
}

// Income returns the ledger entries between start and end, oldest first.
func (c *Client) Income(ctx context.Context, start, end time.Time) ([]IncomeRecord, error) {
	q := url.Values{}
	q.Set("startTime", strconv.FormatInt(start.UnixMilli(), 10))
	q.Set("endTime", strconv.FormatInt(end.UnixMilli(), 10))
	q.Set("limit", "1000")

	raw, err := c.SignedGetRaw(ctx, PathIncome, q)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", PathIncome, err)
	}
	return parseIncome(raw)
}

// parseIncome is split out so the shape can be tested against a real captured
// payload without a network or an API key — the same reason parseHistory is
// separate, and the omission that let /trade/fillHistory go unparsed for
// months.
func parseIncome(raw json.RawMessage) ([]IncomeRecord, error) {
	var rows []struct {
		Symbol     string      `json:"symbol"`
		IncomeType string      `json:"incomeType"`
		Income     json.Number `json:"income"`
		Info       string      `json:"info"`
		Time       json.Number `json:"time"`
	}
	// Numbers arrive as strings here, as everywhere else in this API.
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("%s: unrecognised shape: %s", PathIncome, clip(raw, 160))
	}
	out := make([]IncomeRecord, 0, len(rows))
	for _, r := range rows {
		rec := IncomeRecord{Symbol: r.Symbol, Type: r.IncomeType, Income: num(r.Income), Info: r.Info}
		if ms := num(r.Time); ms > 0 {
			rec.Time = time.UnixMilli(int64(ms))
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

// TradedSymbols returns the distinct contracts with ledger activity in the
// window, so a caller can enumerate what was ACTUALLY traded instead of what
// it expected to find. Deterministic order, so callers and tests are stable.
//
// A funding payment alone puts a symbol in this list, which is correct: a
// position that only paid funding in the window is still a position that the
// caller's per-symbol reads should cover.
func (c *Client) TradedSymbols(ctx context.Context, start, end time.Time) ([]market.Symbol, error) {
	recs, err := c.Income(ctx, start, end)
	if err != nil {
		return nil, err
	}
	return distinctSymbols(recs), nil
}

func distinctSymbols(recs []IncomeRecord) []market.Symbol {
	seen := map[string]bool{}
	var out []market.Symbol
	for _, r := range recs {
		if r.Symbol == "" || seen[r.Symbol] {
			continue
		}
		seen[r.Symbol] = true
		out = append(out, market.Symbol(r.Symbol))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
