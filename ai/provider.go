package ai

import (
	"context"
	"os"
	"strings"
)

// Provider is the abstraction over whichever LLM backend the trading-web
// advisor is currently routing through. Two implementations exist:
//
//   - *Client        (ai/client.go)  — Anthropic Messages API
//   - *GeminiClient  (ai/gemini.go)  — Google Gemini generateContent API
//
// Every request flows through the same context builders (BuildTradeAnalysisMessage
// / BuildSymbolAnalysisMessage) and the same SystemPromptQuantAdvisor. Only the
// wire protocol differs. Provider selection is one env var (AI_PROVIDER),
// so switching between them when a new API key lands is a no-code change.
type Provider interface {
	Send(ctx context.Context, opts SendOptions) (*Response, error)

	// IsDryRun reports whether the provider is in test/stub mode. Handlers
	// use this to skip the "APIKey required" error path — dry-run mode lets
	// the whole pipeline run without a real key.
	IsDryRun() bool
}

// IsDryRun on the Anthropic Client — required to satisfy Provider.
func (c *Client) IsDryRun() bool { return c.DryRun }

// NewProvider constructs the LLM provider selected by AI_PROVIDER
// (default "gemini"). Returned client is safe for concurrent use.
//
// Recognized values:
//
//	"gemini"    → Google Gemini free-tier via GEMINI_API_KEY
//	"anthropic" → Anthropic Claude via ANTHROPIC_API_KEY
//
// Each provider carries its own DryRun mode via the parallel env var
// (GEMINI_DRY_RUN / ANTHROPIC_DRY_RUN). Unknown values fall back to Gemini
// with a warning — deliberately fail-open so a typo doesn't take the
// dashboard down; the DryRun stub still renders something meaningful.
func NewProvider() (Provider, string) {
	name := strings.ToLower(strings.TrimSpace(os.Getenv("AI_PROVIDER")))
	switch name {
	case "anthropic":
		c := New()
		c.DryRun = DryRunFromEnv()
		return c, "anthropic"
	case "", "gemini":
		g := NewGemini()
		g.DryRun = GeminiDryRunFromEnv()
		return g, "gemini"
	default:
		g := NewGemini()
		g.DryRun = GeminiDryRunFromEnv()
		return g, "gemini(fallback-from-" + name + ")"
	}
}

// APIKeyForProvider centralizes the env-var lookup so handlers don't have
// to branch on provider. Wraps APIKeyFromEnv / GeminiAPIKeyFromEnv.
func APIKeyForProvider(p Provider) string {
	switch p.(type) {
	case *GeminiClient:
		return GeminiAPIKeyFromEnv()
	default:
		return APIKeyFromEnv()
	}
}
