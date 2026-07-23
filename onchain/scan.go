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

const scanTimeout = 20 * time.Second

// ScanClient wraps the block-explorer APIs for each supported chain.
// Uses each chain's own v1 endpoint (api.etherscan.io / api.bscscan.com
// / api.basescan.org) because Etherscan V2's unified endpoint does NOT
// cover BSC or Base on the free tier — only paid tiers get multi-chain
// coverage via V2. Per-chain v1 endpoints stay free for each chain
// individually.
//
// Env vars: ETHERSCAN_API_KEY, BSCSCAN_API_KEY, BASESCAN_API_KEY.
// If a chain-specific key is missing, that chain's scan will error
// (partial-error surfaced by the orchestrator, not fatal).
type ScanClient struct {
	HTTP *http.Client
}

// NewScanClient constructs a client. No key required at construction —
// API key is pulled from env per call so mid-session key rotation works.
func NewScanClient() *ScanClient {
	return &ScanClient{HTTP: &http.Client{Timeout: scanTimeout}}
}

// APIKeyFor returns the chain-specific free-tier API key.
// Each chain's scan needs its OWN key registered at that chain's
// explorer site. ETHERSCAN_API_KEY is not multi-chain on free tier.
func (s *ScanClient) APIKeyFor(chain Chain) string {
	switch chain {
	case ChainBSC:
		return strings.TrimSpace(os.Getenv("BSCSCAN_API_KEY"))
	case ChainETH:
		return strings.TrimSpace(os.Getenv("ETHERSCAN_API_KEY"))
	case ChainBase:
		return strings.TrimSpace(os.Getenv("BASESCAN_API_KEY"))
	}
	return ""
}

// endpointFor returns the base URL of the chain's v1 API endpoint.
func endpointFor(chain Chain) string {
	switch chain {
	case ChainBSC:
		return "https://api.bscscan.com/api"
	case ChainETH:
		return "https://api.etherscan.io/api"
	case ChainBase:
		return "https://api.basescan.org/api"
	}
	return ""
}

// registerHintFor returns a friendly URL to point the user at when
// their chain-specific key is missing.
func registerHintFor(chain Chain) string {
	switch chain {
	case ChainBSC:
		return "https://bscscan.com/apis"
	case ChainETH:
		return "https://etherscan.io/apis"
	case ChainBase:
		return "https://basescan.org/apis"
	}
	return ""
}

// envVarFor is the .env variable name for the chain's API key.
func envVarFor(chain Chain) string {
	switch chain {
	case ChainBSC:
		return "BSCSCAN_API_KEY"
	case ChainETH:
		return "ETHERSCAN_API_KEY"
	case ChainBase:
		return "BASESCAN_API_KEY"
	}
	return ""
}

// OutgoingTransfer is one ERC20 transfer event with from == the address
// we queried. Amount is scaled by token decimals.
type OutgoingTransfer struct {
	TxHash    string
	From      string
	To        string
	Amount    float64
	Timestamp time.Time
}

// OutgoingTokenTransfers returns ERC20 outgoing transfers from `address`
// for `contract` within the lookback window. Uses the v2 tokentx action.
//
// Free-tier rate limit is 5 rps. Caller should sequence per-holder
// queries with a small stagger — we don't enforce it here so upstream
// can decide (parallel with limit is fine for a handful of holders).
func (s *ScanClient) OutgoingTokenTransfers(ctx context.Context, chain Chain, address, contract string, decimals int, lookback time.Duration) ([]OutgoingTransfer, error) {
	key := s.APIKeyFor(chain)
	if key == "" {
		return nil, fmt.Errorf("%s API key not set — get a free key at %s and add to .env as %s",
			chain.ExplorerName(), registerHintFor(chain), envVarFor(chain))
	}
	endpoint := endpointFor(chain)
	if endpoint == "" {
		return nil, fmt.Errorf("scan: unsupported chain %q", chain)
	}

	// tokentx returns both incoming and outgoing transfers for the address.
	// We filter to outgoing (from == address) and within the window on our side.
	url := fmt.Sprintf(
		"%s?module=account&action=tokentx&address=%s&contractaddress=%s&startblock=0&endblock=99999999&page=1&offset=100&sort=desc&apikey=%s",
		endpoint, strings.ToLower(address), strings.ToLower(contract), key)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s http: %w", chain.ExplorerName(), err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s read: %w", chain.ExplorerName(), err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s http %d: %s", chain.ExplorerName(), resp.StatusCode, snippet(body, 300))
	}

	var payload struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("%s decode: %w (body=%s)", chain.ExplorerName(), err, snippet(body, 300))
	}
	// status=0 usually means "No transactions found" — treat as empty, not error.
	if payload.Status != "1" {
		if strings.Contains(payload.Message, "No transactions") {
			return nil, nil
		}
		// Rate-limit / invalid-key surfaces as status=0 with a message.
		return nil, fmt.Errorf("%s api: %s (%s)", chain.ExplorerName(), payload.Message, snippet(payload.Result, 200))
	}

	var txs []struct {
		Hash            string `json:"hash"`
		From            string `json:"from"`
		To              string `json:"to"`
		Value           string `json:"value"`
		TokenDecimal    string `json:"tokenDecimal"`
		TimeStamp       string `json:"timeStamp"`
	}
	if err := json.Unmarshal(payload.Result, &txs); err != nil {
		return nil, fmt.Errorf("%s tx-array decode: %w", chain.ExplorerName(), err)
	}

	cutoff := time.Now().Add(-lookback)
	addrLower := strings.ToLower(address)
	out := make([]OutgoingTransfer, 0, 8)
	for _, t := range txs {
		if !strings.EqualFold(t.From, addrLower) {
			continue // skip incoming
		}
		tsSec, _ := strconv.ParseInt(t.TimeStamp, 10, 64)
		ts := time.Unix(tsSec, 0)
		if ts.Before(cutoff) {
			continue
		}
		// Resolve decimals: prefer tx-level tokenDecimal if present,
		// fallback to the passed contract decimals.
		txDec := decimals
		if td, err := strconv.Atoi(t.TokenDecimal); err == nil && td > 0 {
			txDec = td
		}
		amount, _ := strconv.ParseFloat(t.Value, 64)
		amount = amount / math.Pow10(txDec)
		out = append(out, OutgoingTransfer{
			TxHash:    t.Hash,
			From:      strings.ToLower(t.From),
			To:        strings.ToLower(t.To),
			Amount:    amount,
			Timestamp: ts,
		})
	}
	return out, nil
}
