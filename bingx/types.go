package bingx

// Endpoints. BingX has two product lines we care about:
//   - Perpetual swap (BTC-USDT, ETH-USDT): host swap-api.bingx.com
//   - Standard Futures (XAU-USDT, XAG-USDT): currently under open-api with
//     a separate path. Confirm with current BingX docs before wiring.
const (
	HostSwap     = "https://open-api.bingx.com"
	HostWSSwap   = "wss://open-api-swap.bingx.com/swap-market"
	PathKlines   = "/openApi/swap/v3/quote/klines"
	PathDepth    = "/openApi/swap/v2/quote/depth"
	PathFunding        = "/openApi/swap/v2/quote/premiumIndex"
	PathFundingHistory = "/openApi/swap/v2/quote/fundingRate"
	PathOpenInt        = "/openApi/swap/v2/quote/openInterest"
	PathContract       = "/openApi/swap/v2/quote/contracts"

	// Signed endpoints (require APIKey + APISecret + IP whitelist + trade scope).
	PathPositions = "/openApi/swap/v2/user/positions"
	PathOrder     = "/openApi/swap/v2/trade/order"
)

type RawKline struct {
	OpenTime  int64   `json:"time"`
	Open      float64 `json:"open,string"`
	High      float64 `json:"high,string"`
	Low       float64 `json:"low,string"`
	Close     float64 `json:"close,string"`
	Volume    float64 `json:"volume,string"`
	CloseTime int64   `json:"closeTime"`
}

type RawTrade struct {
	Time       int64   `json:"T"`
	Price      float64 `json:"p,string"`
	Quantity   float64 `json:"q,string"`
	BuyerMaker bool    `json:"m"`
}
