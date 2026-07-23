package onchain

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	moralisBase    = "https://deep-index.moralis.io/api/v2.2"
	moralisTimeout = 30 * time.Second
)

// Moralis is a thin wrapper over the Moralis /erc20/{addr}/owners
// endpoint (Web3 Data API). Free tier gives 1M compute units/month
// which is ~500-1000 lookups depending on how much sub-data we pull.
//
// Every request needs the X-API-Key header. If MORALIS_API_KEY is
// unset, Holders() returns a specific error so the caller can surface
// "please set MORALIS_API_KEY" in the UI rather than a generic 401.
type Moralis struct {
	HTTP *http.Client
}

// NewMoralis constructs a client.
func NewMoralis() *Moralis {
	return &Moralis{HTTP: &http.Client{Timeout: moralisTimeout}}
}

// APIKey returns the value of MORALIS_API_KEY env var, or "" if unset.
// Centralized so callers can gate behavior on the key's presence.
func (m *Moralis) APIKey() string {
	return strings.TrimSpace(os.Getenv("MORALIS_API_KEY"))
}

// Holders returns the top-N holders for an ERC20 contract. Balance is
// scaled from wei to token units using the provided decimals.
//
// Moralis returns holders in descending balance order — we take the
// first `topN` and pass them through as Holder rows.
//
// Chain must map to a supported Moralis chain string. BSC = "bsc",
// ETH = "eth", Base = "base".
func (m *Moralis) Holders(ctx context.Context, chain Chain, contract string, decimals int, topN int) ([]Holder, error) {
	key := m.APIKey()
	if key == "" {
		return nil, fmt.Errorf("MORALIS_API_KEY not set — get a free key at https://moralis.io and add to .env")
	}
	if topN <= 0 {
		topN = TopHoldersDefault
	}
	chainSlug := moralisChain(chain)
	if chainSlug == "" {
		return nil, fmt.Errorf("moralis: unsupported chain %q", chain)
	}

	url := fmt.Sprintf("%s/erc20/%s/owners?chain=%s&order=DESC&limit=%d",
		moralisBase, strings.ToLower(contract), chainSlug, topN)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-API-Key", key)

	resp, err := m.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("moralis holders http: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("moralis holders read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("moralis holders http %d: %s", resp.StatusCode, body)
	}

	var payload struct {
		Result []struct {
			OwnerAddress    string `json:"owner_address"`
			Balance         string `json:"balance"`          // wei-denominated integer as string
			BalanceFormatted string `json:"balance_formatted"` // token-denominated as string
			PercentageRelativeToTotalSupply float64 `json:"percentage_relative_to_total_supply"`
			IsContract      bool   `json:"is_contract"`
			OwnerAddressLabel string `json:"owner_address_label"` // Moralis-labeled if known
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("moralis holders decode: %w (body=%s)", err, snippet(body, 300))
	}

	out := make([]Holder, 0, len(payload.Result))
	for i, r := range payload.Result {
		if i >= topN {
			break
		}
		var balance float64
		if r.BalanceFormatted != "" {
			balance, _ = strconv.ParseFloat(r.BalanceFormatted, 64)
		} else if r.Balance != "" {
			// Fallback: divide raw wei by 10^decimals.
			raw, _ := strconv.ParseFloat(r.Balance, 64)
			balance = raw / math.Pow10(decimals)
		}
		h := Holder{
			Rank:       i + 1,
			Address:    strings.ToLower(r.OwnerAddress),
			Balance:    balance,
			PctSupply:  r.PercentageRelativeToTotalSupply,
			Label:      r.OwnerAddressLabel,
			IsContract: r.IsContract,
		}
		out = append(out, h)
	}
	return out, nil
}

func moralisChain(c Chain) string {
	switch c {
	case ChainBSC:
		return "bsc"
	case ChainETH:
		return "eth"
	case ChainBase:
		return "base"
	}
	return ""
}

// OutgoingTransfers returns ERC20 transfers where the wallet at `address`
// is the sender, filtered to the given contract, within the lookback
// window. Uses Moralis's /wallets/{address}/tokens/transfers endpoint —
// works uniformly on BSC/ETH/Base on the free tier (unlike block-
// explorer v1 endpoints which now require paid tier via Etherscan V2).
//
// Cost: ~10-25 CU per call. Free tier 1M CU/mo comfortably covers a
// 20-holder scan per lookup for hundreds of lookups.
func (m *Moralis) OutgoingTransfers(ctx context.Context, chain Chain, address, contract string, decimals int, lookback time.Duration) ([]OutgoingTransfer, error) {
	key := m.APIKey()
	if key == "" {
		return nil, fmt.Errorf("MORALIS_API_KEY not set")
	}
	chainSlug := moralisChain(chain)
	if chainSlug == "" {
		return nil, fmt.Errorf("moralis: unsupported chain %q", chain)
	}
	fromDate := time.Now().Add(-lookback).UTC().Format(time.RFC3339)
	// v2.2 endpoint: /api/v2.2/{address}/erc20/transfers
	// contract_addresses[] is a repeated query param (URL-encoded []).
	url := fmt.Sprintf(
		"%s/%s/erc20/transfers?chain=%s&from_date=%s&contract_addresses%%5B%%5D=%s&limit=100&order=DESC",
		moralisBase, strings.ToLower(address), chainSlug, fromDate, strings.ToLower(contract))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-API-Key", key)

	resp, err := m.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("moralis transfers http: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("moralis transfers read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("moralis transfers http %d: %s", resp.StatusCode, snippet(body, 300))
	}

	var payload struct {
		Result []struct {
			TransactionHash  string `json:"transaction_hash"`
			FromAddress      string `json:"from_address"`
			ToAddress        string `json:"to_address"`
			Value            string `json:"value"`             // wei-denominated as string
			ValueDecimal     string `json:"value_decimal"`     // token-denominated as string
			TokenDecimal     any    `json:"token_decimals"`    // int OR string in different Moralis versions
			BlockTimestamp   string `json:"block_timestamp"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("moralis transfers decode: %w (body=%s)", err, snippet(body, 300))
	}

	addrLower := strings.ToLower(address)
	out := make([]OutgoingTransfer, 0, len(payload.Result))
	for _, t := range payload.Result {
		// Filter to OUTGOING (from == queried address). Moralis's
		// wallet/tokens/transfers may return both directions — we
		// want only outflows.
		if !strings.EqualFold(t.FromAddress, addrLower) {
			continue
		}
		ts, _ := time.Parse(time.RFC3339, t.BlockTimestamp)
		var amount float64
		if t.ValueDecimal != "" {
			amount, _ = strconv.ParseFloat(t.ValueDecimal, 64)
		}
		out = append(out, OutgoingTransfer{
			TxHash:    t.TransactionHash,
			From:      strings.ToLower(t.FromAddress),
			To:        strings.ToLower(t.ToAddress),
			Amount:    amount,
			Timestamp: ts,
		})
	}
	return out, nil
}

func snippet(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
