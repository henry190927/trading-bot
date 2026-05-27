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
	TF1h  Timeframe = "1h"
	TF4h  Timeframe = "4h"
	TF1d  Timeframe = "1d"
)
