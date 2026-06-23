// Package ai is the LLM-advisor layer of trading-web. It exposes a thin
// Anthropic Messages API client used by /ai/analyze endpoints to get a
// Quant-flavored take on a trade plan or open position. Design goals:
//
//  1. Per-user API key from day 1. The Client carries no key; every Send
//     call takes an explicit apiKey. Callers pull from env (Phase 1) or
//     from a future per-user settings store (Phase 2+) without the ai
//     package having to change.
//  2. DRY-RUN mode for cost-free integration testing. When enabled the
//     client logs the request shape and returns a fabricated response.
//  3. Token usage exposed on every response so callers can enforce
//     budgets and surface cost-per-call to the user.
//
// Phase 1 is non-streaming one-shot calls. Streaming + multi-turn chat
// are Phase 2 additions on the same client.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// DefaultModel is the Anthropic model used unless the caller picks
	// another. Sonnet is the per-call sweet spot ($3/MTok in, $15/MTok
	// out) — Opus reserved for opt-in heavy analyses.
	DefaultModel = "claude-sonnet-4-6"

	// DefaultMaxTokens caps the model's output. ~800 tokens fits a
	// typical Quant trade analysis (4-6 paragraphs) without runaway
	// generation.
	DefaultMaxTokens = 1200

	apiEndpoint    = "https://api.anthropic.com/v1/messages"
	apiVersion     = "2023-06-01"
	requestTimeout = 60 * time.Second
)

// Client is a stateless wrapper over Anthropic /v1/messages. Construct
// once at startup; Send is safe for concurrent use.
type Client struct {
	HTTP   *http.Client
	DryRun bool
}

// New constructs an ai.Client. The HTTP client gets a 60s timeout to
// match Anthropic's typical p99 response time for short conversations.
func New() *Client {
	return &Client{
		HTTP: &http.Client{Timeout: requestTimeout},
	}
}

// Message is a single conversation turn. Phase 1 always sends one User
// message; Phase 2 chat will append Assistant + further User turns.
type Message struct {
	Role    string `json:"role"`    // "user" | "assistant"
	Content string `json:"content"` // plain text; we don't use Anthropic's content-blocks for Phase 1
}

// SendOptions bundles per-call knobs so adding new ones doesn't churn
// the Send signature. APIKey is required (every caller supplies it
// from env or future per-user store).
type SendOptions struct {
	APIKey       string
	Model        string  // empty = DefaultModel
	MaxTokens    int     // 0 = DefaultMaxTokens
	System       string  // system prompt; required for Phase 1
	Messages     []Message
	Temperature  float64 // 0 = Anthropic default (1.0). Set 0.2-0.5 for analytical determinism.
}

// Response captures what callers need from a one-shot call: the text
// and the token counts (for cost tracking + future budget enforcement).
type Response struct {
	Text         string
	InputTokens  int
	OutputTokens int
	Model        string
	StopReason   string // "end_turn" | "max_tokens" | etc.
}

// EstimatedCostUSD returns a rough cost in dollars for a response.
// Pricing as of 2026-06: Sonnet $3/MTok in, $15/MTok out; Opus $15/$75.
// Adjust constants here when pricing changes.
func (r Response) EstimatedCostUSD() float64 {
	inRate := 3.0 / 1e6
	outRate := 15.0 / 1e6
	if strings.Contains(r.Model, "opus") {
		inRate = 15.0 / 1e6
		outRate = 75.0 / 1e6
	}
	return float64(r.InputTokens)*inRate + float64(r.OutputTokens)*outRate
}

// Send issues a one-shot Messages API call. Returns the response text
// and token counts, or an error wrapping the HTTP status / Anthropic
// error body for diagnosability.
func (c *Client) Send(ctx context.Context, opts SendOptions) (*Response, error) {
	if opts.APIKey == "" {
		return nil, fmt.Errorf("ai: APIKey required (env ANTHROPIC_API_KEY or per-user override)")
	}
	if opts.System == "" {
		return nil, fmt.Errorf("ai: System prompt required for Phase 1")
	}
	if len(opts.Messages) == 0 {
		return nil, fmt.Errorf("ai: at least one Message required")
	}
	model := opts.Model
	if model == "" {
		model = DefaultModel
	}
	maxTokens := opts.MaxTokens
	if maxTokens == 0 {
		maxTokens = DefaultMaxTokens
	}

	if c.DryRun {
		mock := fmt.Sprintf("[DRY-RUN ai.Send] would have called model=%s tokens(max)=%d system_len=%d messages=%d last_user_msg_preview=%q",
			model, maxTokens, len(opts.System), len(opts.Messages), truncate(opts.Messages[len(opts.Messages)-1].Content, 200))
		log.Print(mock)
		return &Response{
			Text:         "[DRY-RUN] AI analysis would appear here.\n\nThis is a stub response while ANTHROPIC_DRY_RUN=true.\nFlip BINGX_DRY_RUN-style env off and reload to see real output.",
			InputTokens:  0,
			OutputTokens: 0,
			Model:        model,
			StopReason:   "dry_run",
		}, nil
	}

	body := map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"system":     opts.System,
		"messages":   opts.Messages,
	}
	if opts.Temperature > 0 {
		body["temperature"] = opts.Temperature
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("ai: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiEndpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("ai: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", apiVersion)
	req.Header.Set("x-api-key", opts.APIKey)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ai: http do: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ai: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Anthropic errors come back as {"type":"error","error":{"type":...,"message":...}}.
		// Surface the body verbatim — callers usually want to see the actual reason
		// (rate limit, auth failure, invalid model name, etc.).
		return nil, fmt.Errorf("ai: http %d: %s", resp.StatusCode, raw)
	}

	var decoded struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("ai: decode response: %w (body=%s)", err, raw)
	}

	// Concatenate any text blocks — Anthropic returns a content array even
	// for plain-text responses, with one item of type="text".
	var text strings.Builder
	for _, c := range decoded.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	return &Response{
		Text:         text.String(),
		InputTokens:  decoded.Usage.InputTokens,
		OutputTokens: decoded.Usage.OutputTokens,
		Model:        decoded.Model,
		StopReason:   decoded.StopReason,
	}, nil
}

// APIKeyFromEnv returns ANTHROPIC_API_KEY. Centralized here so Phase 2's
// per-user-override layer can be added by wrapping or replacing this
// single helper rather than hunting through callers.
func APIKeyFromEnv() string {
	return os.Getenv("ANTHROPIC_API_KEY")
}

// DryRunFromEnv reports whether ANTHROPIC_DRY_RUN=true. Mirrors the
// BINGX_DRY_RUN pattern so deploy/test conventions stay parallel.
func DryRunFromEnv() bool {
	return os.Getenv("ANTHROPIC_DRY_RUN") == "true"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
