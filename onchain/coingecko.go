package onchain

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	coingeckoBase       = "https://api.coingecko.com/api/v3"
	coingeckoTimeout    = 20 * time.Second
	symbolCacheTTL      = 1 * time.Hour
	coinDetailCacheTTL  = 30 * time.Minute
	coingeckoUserAgent  = "trading-bot-onchain/1.0"
)

// CoinGecko is a thin free-tier client that resolves a user-typed
// symbol to a CoinGecko id + contract-per-chain.
//
// Two caches keep RPS under the free tier's ~30 req/min limit:
//
//   - symbolIndex: full /coins/list (id + symbol + name) refreshed 1h
//   - detail:      per-coin-id /coins/{id} response, 30 min TTL
//
// Both caches are read under RLock, populated under Lock.
type CoinGecko struct {
	HTTP *http.Client

	// symbolIndex: uppercased ticker -> matching coin refs (may be many
	// due to symbol collisions like "AKE").
	indexMu       sync.RWMutex
	symbolIndex   map[string][]coinRef
	indexLoadedAt time.Time

	detailMu    sync.RWMutex
	detailCache map[string]detailCacheEntry // key = coin id
}

type coinRef struct {
	ID     string
	Symbol string
	Name   string
}

type detailCacheEntry struct {
	Resolved CoinResolved
	CachedAt time.Time
}

// NewCoinGecko constructs a client. No API key needed for the free tier.
func NewCoinGecko() *CoinGecko {
	return &CoinGecko{
		HTTP:        &http.Client{Timeout: coingeckoTimeout},
		symbolIndex: map[string][]coinRef{},
		detailCache: map[string]detailCacheEntry{},
	}
}

// Resolve takes a user-typed identifier — either a ticker (e.g. "AKE")
// or a CoinGecko id (e.g. "akedo") — and returns the coin's contract
// addresses per supported chain.
//
// Ambiguous tickers (many coins share the same symbol) are resolved by
// picking the coin with the highest market cap. When user passes the
// exact coin-id, that's used verbatim (bypasses the symbol index).
func (c *CoinGecko) Resolve(ctx context.Context, input string) (CoinResolved, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return CoinResolved{}, fmt.Errorf("coingecko: input required")
	}

	// Direct coin-id path — try to fetch detail directly.
	// Heuristic: coin ids are lowercase-with-dashes ("lorenzo-protocol").
	// Tickers are usually short + uppercased. If user's input has a
	// dash OR is all lowercase and >4 chars, try coin-id first.
	looksLikeID := strings.Contains(input, "-") || (input == strings.ToLower(input) && len(input) > 4)

	if looksLikeID {
		res, err := c.fetchDetail(ctx, input)
		if err == nil {
			return res, nil
		}
		// Fall through to ticker lookup if direct id failed.
	}

	// Ticker path — resolve via symbol index.
	refs, err := c.lookupByTicker(ctx, input)
	if err != nil {
		return CoinResolved{}, err
	}
	if len(refs) == 0 {
		return CoinResolved{}, fmt.Errorf("coingecko: no coin matches ticker %q", input)
	}
	// Prefer the highest-marketcap match. We don't have marketcap in the
	// index — best proxy is: pick the FIRST /coins/{id} that returns
	// with a non-zero marketcap AND at least one contract on a supported
	// chain. In practice CoinGecko's /coins/list is ordered by
	// marketcap-ish already, so first-viable usually wins.
	var lastErr error
	for _, r := range refs {
		res, err := c.fetchDetail(ctx, r.ID)
		if err != nil {
			lastErr = err
			continue
		}
		if len(res.Contracts) > 0 {
			return res, nil
		}
	}
	if lastErr != nil {
		return CoinResolved{}, fmt.Errorf("coingecko: no viable match for %q (last error: %w)", input, lastErr)
	}
	return CoinResolved{}, fmt.Errorf("coingecko: matched %d coin(s) for %q but none have a supported-chain contract", len(refs), input)
}

// lookupByTicker uses CoinGecko's /search endpoint which returns coins
// ranked by market cap — critical for ambiguous tickers like "BANK"
// (Bankless DAO vs Lorenzo Protocol) where the alphabetical /coins/list
// puts the smaller-cap coin first. Only exact symbol matches are kept
// (case-insensitive), preserving /search's marketcap ordering.
func (c *CoinGecko) lookupByTicker(ctx context.Context, ticker string) ([]coinRef, error) {
	url := coingeckoBase + "/search?query=" + strings.ToLower(ticker)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", coingeckoUserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("coingecko /search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("coingecko /search: http %d: %s", resp.StatusCode, body)
	}
	var payload struct {
		Coins []struct {
			ID            string `json:"id"`
			Symbol        string `json:"symbol"`
			Name          string `json:"name"`
			MarketCapRank int    `json:"market_cap_rank"`
		} `json:"coins"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("coingecko /search decode: %w", err)
	}
	// Keep exact symbol matches only — /search returns fuzzy matches
	// (a search for "BANK" would return coins containing "bank" in
	// their name too). We want strictly the ticker AKE, not "AKEcoin".
	target := strings.ToUpper(ticker)
	var refs []coinRef
	for _, r := range payload.Coins {
		if strings.ToUpper(r.Symbol) == target {
			refs = append(refs, coinRef{ID: r.ID, Symbol: r.Symbol, Name: r.Name})
		}
	}
	return refs, nil
}

// fetchDetail hits /coins/{id} to get platform contract addresses.
// Cached 30 min per coin id.
func (c *CoinGecko) fetchDetail(ctx context.Context, id string) (CoinResolved, error) {
	c.detailMu.RLock()
	if cached, ok := c.detailCache[id]; ok && time.Since(cached.CachedAt) < coinDetailCacheTTL {
		c.detailMu.RUnlock()
		return cached.Resolved, nil
	}
	c.detailMu.RUnlock()

	url := fmt.Sprintf("%s/coins/%s?localization=false&tickers=false&community_data=false&developer_data=false&sparkline=false", coingeckoBase, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return CoinResolved{}, err
	}
	req.Header.Set("User-Agent", coingeckoUserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return CoinResolved{}, fmt.Errorf("coingecko /coins/%s: %w", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return CoinResolved{}, fmt.Errorf("coingecko: coin id %q not found", id)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return CoinResolved{}, fmt.Errorf("coingecko /coins/%s: http %d: %s", id, resp.StatusCode, body)
	}

	var payload struct {
		ID         string            `json:"id"`
		Symbol     string            `json:"symbol"`
		Name       string            `json:"name"`
		Platforms  map[string]string `json:"platforms"`
		DetailPlatforms map[string]struct {
			DecimalPlace  int    `json:"decimal_place"`
			ContractAddress string `json:"contract_address"`
		} `json:"detail_platforms"`
		MarketData struct {
			MarketCap map[string]float64 `json:"market_cap"`
			FDV       map[string]float64 `json:"fully_diluted_valuation"`
		} `json:"market_data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return CoinResolved{}, fmt.Errorf("coingecko /coins/%s decode: %w", id, err)
	}

	res := CoinResolved{
		Symbol:      strings.ToUpper(payload.Symbol),
		CoinGeckoID: payload.ID,
		Name:        payload.Name,
	}
	if v, ok := payload.MarketData.MarketCap["usd"]; ok {
		res.MarketCapUSD = v
	}
	if v, ok := payload.MarketData.FDV["usd"]; ok {
		res.FDVUSD = v
	}
	// Map CoinGecko platform slugs to our Chain enum. Only the chains
	// we can query (Moralis+scan) are kept.
	platformMap := map[string]Chain{
		"binance-smart-chain": ChainBSC,
		"ethereum":            ChainETH,
		"base":                ChainBase,
	}
	for slug, addr := range payload.Platforms {
		chain, ok := platformMap[slug]
		if !ok || addr == "" {
			continue
		}
		decimals := 18
		if dp, ok := payload.DetailPlatforms[slug]; ok && dp.DecimalPlace > 0 {
			decimals = dp.DecimalPlace
		}
		res.Contracts = append(res.Contracts, ContractOnChain{
			Chain:    chain,
			Address:  strings.ToLower(strings.TrimSpace(addr)),
			Decimals: decimals,
		})
	}

	c.detailMu.Lock()
	c.detailCache[id] = detailCacheEntry{Resolved: res, CachedAt: time.Now()}
	c.detailMu.Unlock()
	return res, nil
}
