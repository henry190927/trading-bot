package market

import "strings"

type Symbol string

// Note on metals: BingX swap doesn't list XAU-USDT / XAG-USDT directly.
// Spot gold/silver trade as CFD-style perpetuals under the NCCO* family.
// We keep the friendly names BTCUSDT/ETHUSDT/XAUUSDT/XAGUSDT and map them
// to the BingX contract codes at the client boundary.
const (
	BTCUSDT Symbol = "BTC-USDT"
	ETHUSDT Symbol = "ETH-USDT"
	XAUUSDT Symbol = "NCCOGOLD2USD-USDT"
	XAGUSDT Symbol = "NCCOXAG2USD-USDT"
	// BRENTUSDT — not in All() yet; pre-flight backtest candidate as of
	// 2026-06-03. Activate by adding to All() once the 60/90/120d A/B
	// clears the user's strategy-change gate.
	BRENTUSDT Symbol = "NCCO1OILBRENT2USD-USDT"
	// US-stock synthetics (NCSK* family). DELIBERATELY NOT in All(): they
	// are forward-log only (web analyze / chart / setups / journal), kept
	// out of the daemon scan + ntfy until the backtest survivors clear the
	// ship-gate on live data. SNDK gets the counter-trend structure veto,
	// NVDA gets the 樞紐區 zone vote (their respective 2026-08-15 A/B
	// survivors). See isStructureVetoSymbol / isStructureZoneVoteSymbol.
	SNDKUSDT Symbol = "NCSKSNDK2USD-USDT"
	NVDAUSDT Symbol = "NCSKNVDA2USD-USDT"
	// Added 2026-09-04 after a 4-arm x 3-window strategy-fit A/B. Codes read
	// off BingX's contract list with cmd/contracts, never inferred: the
	// prefix varies by asset class (NCSK stocks, NCCO commodities, NCSI
	// indices) and the ticker sits mid-string, so a guessed code resolves to
	// nothing and fails silently.
	SPCXUSDT Symbol = "NCSKSPCX2USD-USDT" // SpaceX
	MSTRUSDT Symbol = "NCSKMSTR2USD-USDT" // MicroStrategy — the BTC-correlated one
	APPUSDT  Symbol = "NCSKAPP2USD-USDT"  // AppLovin

	// Crypto-alt forward-log symbols (StructMomentum thread). Same discipline
	// as the stock synthetics: DELIBERATELY NOT in All() (out of daemon/ntfy)
	// until live data clears the ship-gate. SOL/LINK cleared the StructMomentum
	// A/B strongly, SUI/HYPE marginally; NEAR runs the MR engine (it A/B'd as a
	// mean-reversion symbol, not momentum). Strategy per (symbol,TF) is set in
	// signal.strategyFor. See docs/struct_momentum_strategy_design.md.
	SOLUSDT  Symbol = "SOL-USDT"
	LINKUSDT Symbol = "LINK-USDT"
	SUIUSDT  Symbol = "SUI-USDT"
	HYPEUSDT Symbol = "HYPE-USDT"
	NEARUSDT Symbol = "NEAR-USDT"

	// XRP added 2026-09-19, after a real position was opened on it on 09-18
	// and the tooling could not see it: /ops/fills iterates uiSymbols, XRP was
	// not on the list, so the trade was invisible until the income ledger was
	// read by hand. The naked-position guard was never blind to it —
	// orphanCycle enumerates AllPositions and falls back to the raw contract
	// code — but a symbol that can hold a position and cannot be reconciled is
	// half-supported, which is worse than unsupported.
	//
	// NOT in All(), same discipline as every other forward-log symbol, and NOT
	// in signal.strategyFor: XRP therefore runs the MR engine by default.
	// StructMomentum's alt expansion (XRP included) is still BLOCKED — one pass
	// in six tested pairs. Adding a contract code is not adding a strategy.
	XRPUSDT Symbol = "XRP-USDT"
)

func (s Symbol) IsMetal() bool {
	return s == XAUUSDT || s == XAGUSDT
}

// IsUSStock reports whether s is one of BingX's CFD-style US-equity
// synthetics. Their contract codes are NCSK<TICKER>2USD-USDT, where the
// metals use NCCO instead.
//
// Prefix rather than an enumerated list, on purpose: cmd/contracts resolves
// any ticker to its NCSK code and the roster has grown four times, so a list
// would silently stop covering new additions — and the thing that consults
// this (the cash-open stop guard) failing open on a NEW ticker is the exact
// case it exists for.
func (s Symbol) IsUSStock() bool {
	return strings.HasPrefix(string(s), "NCSK")
}

func All() []Symbol {
	return []Symbol{BTCUSDT, ETHUSDT, XAUUSDT, XAGUSDT}
}

type Timeframe string

const (
	TF1m  Timeframe = "1m"
	TF5m  Timeframe = "5m"
	TF15m Timeframe = "15m"
	TF30m Timeframe = "30m"
	TF1h  Timeframe = "1h"
	TF2h  Timeframe = "2h"
	TF4h  Timeframe = "4h"
	TF1d  Timeframe = "1d"
)
