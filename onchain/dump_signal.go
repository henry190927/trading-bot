package onchain

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

const resultCacheTTL = 15 * time.Minute

// Service is the top-level orchestrator for on-chain lookups. Composes:
//
//   - CoinGecko: user input → contract address per supported chain
//   - Moralis: top-N holders for the resolved contract
//   - Scan: outgoing token transfers per holder within lookback window
//   - CEXRegistry: match transfer destination to known CEX wallets
//
// Cached by (input, chain) for 15 min. Failures in one provider stage
// don't block the whole result — they surface as PartialErrors on the
// LookupResult so the UI can render "holders returned but scan failed".
type Service struct {
	CoinGecko *CoinGecko
	Moralis   *Moralis
	Scan      *ScanClient
	CEX       *CEXRegistry

	cacheMu sync.Mutex
	cache   map[string]cacheEntry
}

type cacheEntry struct {
	Result   LookupResult
	CachedAt time.Time
}

// NewService constructs an orchestrator with default providers.
func NewService() *Service {
	return &Service{
		CoinGecko: NewCoinGecko(),
		Moralis:   NewMoralis(),
		Scan:      NewScanClient(),
		CEX:       NewCEXRegistry(),
		cache:     map[string]cacheEntry{},
	}
}

// Lookup runs the full pipeline for a user-typed symbol/id and optional
// chain preference. If preferredChain is empty and the coin exists on
// multiple chains, the first supported chain (BSC > ETH > Base) is
// picked.
func (s *Service) Lookup(ctx context.Context, input string, preferredChain Chain) (LookupResult, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return LookupResult{}, fmt.Errorf("input required")
	}

	cacheKey := strings.ToLower(input) + "|" + string(preferredChain)
	s.cacheMu.Lock()
	if e, ok := s.cache[cacheKey]; ok && time.Since(e.CachedAt) < resultCacheTTL {
		e.Result.CacheHit = true
		s.cacheMu.Unlock()
		return e.Result, nil
	}
	s.cacheMu.Unlock()

	// Step 1: resolve to contract.
	coin, err := s.CoinGecko.Resolve(ctx, input)
	if err != nil {
		return LookupResult{}, fmt.Errorf("resolve: %w", err)
	}
	if len(coin.Contracts) == 0 {
		return LookupResult{}, fmt.Errorf("coin %q (%s) has no supported-chain contract (BSC/ETH/Base only)", coin.Name, coin.CoinGeckoID)
	}
	chosen := pickContract(coin.Contracts, preferredChain)

	res := LookupResult{
		Symbol:      coin.Symbol,
		CoinGeckoID: coin.CoinGeckoID,
		Name:        coin.Name,
		Chain:       chosen.Chain,
		Contract:    chosen.Address,
		FetchedAt:   time.Now(),
	}

	// Step 2: holders (fatal — no holders = no signal).
	holders, err := s.Moralis.Holders(ctx, chosen.Chain, chosen.Address, chosen.Decimals, TopHoldersDefault)
	if err != nil {
		return LookupResult{}, fmt.Errorf("moralis holders: %w", err)
	}
	// Label holders with CEX registry hits (Moralis's own label wins if present).
	for i := range holders {
		if e, ok := s.CEX.Lookup(chosen.Chain, holders[i].Address); ok {
			holders[i].IsCEX = true
			if holders[i].Label == "" {
				holders[i].Label = fmt.Sprintf("%s (%s)", e.Exchange, e.Type)
			}
		}
	}
	res.Holders = holders

	// Step 3: outgoing tx scan per holder (parallel, best-effort).
	dumpSignals, scanErrors := s.scanAllHolders(ctx, chosen, holders)
	res.DumpSignals = dumpSignals
	if len(scanErrors) > 0 {
		res.PartialErrors = append(res.PartialErrors, scanErrors...)
	}

	// Cache.
	s.cacheMu.Lock()
	s.cache[cacheKey] = cacheEntry{Result: res, CachedAt: time.Now()}
	s.cacheMu.Unlock()
	return res, nil
}

// pickContract chooses which chain's contract to lookup. Honors user
// preference if present + valid; otherwise picks by chain priority.
func pickContract(contracts []ContractOnChain, preferred Chain) ContractOnChain {
	if preferred != "" {
		for _, c := range contracts {
			if c.Chain == preferred {
				return c
			}
		}
	}
	priority := map[Chain]int{ChainBSC: 0, ChainETH: 1, ChainBase: 2}
	best := contracts[0]
	for _, c := range contracts[1:] {
		if priority[c.Chain] < priority[best.Chain] {
			best = c
		}
	}
	return best
}

// scanAllHolders queries Moralis for each holder's outgoing transfers
// in the lookback window and matches destinations against the CEX
// registry to produce DumpSignal rows.
//
// Uses Moralis (not the block explorers) because as of 2025 Etherscan
// V2 migration locked BSC / Base scan endpoints behind paid tier —
// Moralis remains free-tier for all EVM chains. Cost is ~10-25 CU per
// holder query, so 20 holders ≈ 500 CU per lookup, well inside the
// 1M CU/mo free budget.
//
// We SKIP holders labeled as known contracts or as CEX wallets —
// their outgoing transfers are noise (LP moves, exchange internal
// transfers), not the dump signals we care about.
//
// Sequential with 250ms pacing = ~4 rps, comfortably inside Moralis's
// 25 rps free-tier ceiling.
func (s *Service) scanAllHolders(ctx context.Context, contract ContractOnChain, holders []Holder) ([]DumpSignal, []string) {
	var signals []DumpSignal
	var errs []string
	for _, h := range holders {
		if h.IsCEX || h.IsContract {
			continue
		}
		select {
		case <-ctx.Done():
			return signals, append(errs, "context cancelled during scan")
		case <-time.After(250 * time.Millisecond):
		}
		txs, err := s.Moralis.OutgoingTransfers(ctx, contract.Chain, h.Address, contract.Address, contract.Decimals, LookbackDefault)
		if err != nil {
			errs = append(errs, fmt.Sprintf("holder #%d transfers: %v", h.Rank, err))
			log.Printf("[onchain] holder #%d (%s) transfer error: %v", h.Rank, h.Address, err)
			continue
		}
		for _, t := range txs {
			cex, ok := s.CEX.Lookup(contract.Chain, t.To)
			if !ok {
				continue
			}
			signals = append(signals, DumpSignal{
				HolderRank:   h.Rank,
				FromAddress:  t.From,
				ToAddress:    t.To,
				ToExchange:   cex.Exchange,
				AmountTokens: t.Amount,
				Timestamp:    t.Timestamp,
				TxHash:       t.TxHash,
			})
		}
	}
	return signals, errs
}
