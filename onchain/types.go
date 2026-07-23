// Package onchain provides an on-demand per-coin lookup that surfaces
// distribution / dump-signal patterns without requiring a paid data
// source (Nansen / Arkham).
//
// The MVP flow is:
//
//  1. CoinGecko resolves a user-typed symbol (e.g. "AKE") to a set of
//     (chain, contract-address) pairs. Multi-chain tokens are handled
//     by preferring the platform with the largest liquidity.
//  2. Moralis (free tier) returns the top N holders for the resolved
//     contract, with balance + %supply.
//  3. For each top holder, the appropriate block-explorer API
//     (BscScan / Etherscan / Basescan) is queried for recent outgoing
//     token transfers.
//  4. Any transfer that lands at a known CEX deposit / hot-wallet
//     address is flagged as a dump signal. The CEX-address list is
//     seeded from an embedded JSON and can be refreshed at runtime
//     from an external URL (env CEX_ADDRESSES_URL).
//
// Design notes:
//   - Every external client fails soft — a missing API key or a 429
//     surfaces as a partial result rather than blowing up the whole
//     lookup.
//   - Caching is 15 min per (symbol, chain) to keep free-tier quotas
//     comfortable.
//   - No engine coupling — this is a discretionary research tool per
//     the user's [[feedback-strategy-scope]] rule.
package onchain

import "time"

// Chain identifies which EVM network a token contract lives on. Only
// EVM chains are supported in the MVP — Solana / TRON / non-EVM
// networks would need a different holder API and are out of scope.
type Chain string

const (
	ChainBSC  Chain = "bsc"
	ChainETH  Chain = "eth"
	ChainBase Chain = "base"
)

// AllChains is the ordered list of chains the MVP supports. Ordered by
// most-common-for-alt-plays first so cross-chain tokens resolve to a
// sensible default.
func AllChains() []Chain {
	return []Chain{ChainBSC, ChainETH, ChainBase}
}

// String returns the on-chain slug used in URLs / labels.
func (c Chain) String() string { return string(c) }

// ExplorerName is a human-readable name for the block explorer that
// serves this chain (used in error messages + UI labels).
func (c Chain) ExplorerName() string {
	switch c {
	case ChainBSC:
		return "BscScan"
	case ChainETH:
		return "Etherscan"
	case ChainBase:
		return "Basescan"
	}
	return string(c)
}

// CoinResolved is what CoinGecko lookup returns for a user-typed symbol.
type CoinResolved struct {
	Symbol       string  // canonical ticker uppercased (e.g. "AKE")
	CoinGeckoID  string  // gecko id (e.g. "akedo")
	Name         string  // display name (e.g. "Akedo")
	Contracts    []ContractOnChain
	MarketCapUSD float64 // if known
	FDVUSD       float64 // fully diluted valuation
}

// ContractOnChain pairs a chain with its ERC20 contract address.
type ContractOnChain struct {
	Chain    Chain
	Address  string  // 0x... lowercase
	Decimals int
}

// Holder is one row of a top-holders table.
type Holder struct {
	Rank       int
	Address    string  // 0x... lowercase
	Balance    float64 // token-denominated (after decimals)
	PctSupply  float64 // 0-100
	Label      string  // human label if known ("Binance hot wallet", "PancakeSwap V3 LP", ...)
	IsCEX      bool    // true if address matches a known CEX deposit/hot wallet
	IsContract bool    // true if address is a smart contract (best-effort detection)
}

// DumpSignal is a single outgoing transfer from a top holder to a known
// CEX hot wallet within the lookback window.
type DumpSignal struct {
	HolderRank      int
	FromAddress     string
	ToAddress       string
	ToExchange      string  // "Binance" / "BingX" / etc — the exchange name matched
	AmountTokens    float64 // token-denominated
	AmountUSDApprox float64 // best-effort USD value at time of tx (0 if unknown)
	Timestamp       time.Time
	TxHash          string
}

// LookupResult is the full response for a symbol lookup — assembled by
// dump_signal.go from the individual provider results.
type LookupResult struct {
	Symbol      string
	CoinGeckoID string
	Name        string
	Chain       Chain
	Contract    string  // 0x... lowercase, canonical contract on the chosen chain

	Holders     []Holder
	DumpSignals []DumpSignal

	FetchedAt time.Time
	CacheHit  bool

	// PartialErrors carries provider errors that didn't block the whole
	// lookup (e.g. Moralis returned but scan failed). Rendered in the UI
	// so the user knows the result is incomplete.
	PartialErrors []string
}

// LookbackDefault is the transfer-scan window for dump-signal detection.
const LookbackDefault = 48 * time.Hour

// TopHoldersDefault is how many holders we pull from Moralis in the MVP.
// Kept at 20 to stay well inside free-tier compute-unit budgets.
const TopHoldersDefault = 20
