package bingx

import (
	"context"
	"encoding/json"
	"fmt"
)

// Contract is one row of the public perpetual contract list. Only the fields
// the symbol catalog reads are decoded; the rest of the payload is ignored.
type Contract struct {
	Symbol            string  `json:"symbol"`      // contract code, e.g. "NCSKTSLA2USD-USDT"
	DisplayName       string  `json:"displayName"` // "TSLA-USDT", "GOLD(XAU)-USDT"
	Asset             string  `json:"asset"`
	Currency          string  `json:"currency"` // quote, "USDT"
	PricePrecision    int     `json:"pricePrecision"`
	QuantityPrecision int     `json:"quantityPrecision"`
	TradeMinUSDT      float64 `json:"tradeMinUSDT"`
	// Status 1 = trading. 25 is what delisted / suspended contracts carry
	// (RACE, MS on 2026-11-24); anything other than 1 is left out of the
	// catalog by the caller.
	Status int `json:"status"`
}

// ContractStatusTrading is the Status of a live contract.
const ContractStatusTrading = 1

// Contracts lists every perpetual contract. Public endpoint, no signature.
// Endpoint: GET /openApi/swap/v2/quote/contracts
func (c *Client) Contracts(ctx context.Context) ([]Contract, error) {
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := c.getJSON(ctx, PathContract, nil, &env); err != nil {
		return nil, fmt.Errorf("bingx contracts: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("bingx contracts: api code %d: %s", env.Code, env.Msg)
	}
	var out []Contract
	if err := json.Unmarshal(env.Data, &out); err != nil {
		return nil, fmt.Errorf("bingx contracts: decode data: %w", err)
	}
	return out, nil
}
