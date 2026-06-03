package market

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
)

func (s Symbol) IsMetal() bool {
	return s == XAUUSDT || s == XAGUSDT
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
