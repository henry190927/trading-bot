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
	// DefaultGeminiModel is the free-tier Gemini reasoning-capable model.
	// 2.5-flash ships with reasoning built in (no separate -thinking-exp
	// SKU anymore) and remains free-tier. Reasoning depth gets Quant-
	// persona analysis closer to Claude-Sonnet quality at $0/call.
	//
	// Override via GEMINI_MODEL env var when a newer variant ships
	// (e.g. gemini-3-flash-preview, gemini-2.5-pro). Any model that
	// supports generateContent works.
	DefaultGeminiModel = "gemini-2.5-flash"

	geminiAPIBase        = "https://generativelanguage.googleapis.com/v1beta/models/"
	geminiRequestTimeout = 90 * time.Second // reasoning models are slower — allow more headroom than the 60s used for Anthropic
)

// GeminiClient is the Google Gemini generateContent counterpart to
// ai.Client (Anthropic). Same Provider interface, same context builders,
// same Quant system prompt — only the wire protocol changes.
type GeminiClient struct {
	HTTP   *http.Client
	Model  string // empty → GEMINI_MODEL env var → DefaultGeminiModel
	DryRun bool
}

// NewGemini constructs a GeminiClient. Concurrent-safe.
func NewGemini() *GeminiClient {
	return &GeminiClient{
		HTTP:  &http.Client{Timeout: geminiRequestTimeout},
		Model: os.Getenv("GEMINI_MODEL"), // may be "" — Send will fill in
	}
}

// IsDryRun on the Gemini client — satisfies Provider.
func (g *GeminiClient) IsDryRun() bool { return g.DryRun }

// Send is the Gemini analogue of Client.Send. Translates SendOptions
// (which is Anthropic-shaped) into Gemini's request shape:
//
//   - opts.System         → systemInstruction.parts[].text
//   - opts.Messages[i]    → contents[i].parts[].text with role "user"/"model"
//     (Anthropic's "assistant" role maps to "model" in Gemini)
//   - opts.Temperature    → generationConfig.temperature
//   - opts.MaxTokens      → generationConfig.maxOutputTokens
//
// Response fields fill:
//
//   - Text          ← candidates[0].content.parts[].text (concatenated)
//   - InputTokens   ← usageMetadata.promptTokenCount
//   - OutputTokens  ← usageMetadata.candidatesTokenCount
//   - Model         ← response.modelVersion (or the requested model on 200)
//   - StopReason    ← candidates[0].finishReason
func (g *GeminiClient) Send(ctx context.Context, opts SendOptions) (*Response, error) {
	if opts.APIKey == "" && !g.DryRun {
		return nil, fmt.Errorf("ai/gemini: APIKey required (env GEMINI_API_KEY or per-user override)")
	}
	if opts.System == "" {
		return nil, fmt.Errorf("ai/gemini: System prompt required")
	}
	if len(opts.Messages) == 0 {
		return nil, fmt.Errorf("ai/gemini: at least one Message required")
	}
	model := opts.Model
	if model == "" {
		model = g.Model
	}
	if model == "" {
		model = DefaultGeminiModel
	}
	maxTokens := opts.MaxTokens
	if maxTokens == 0 {
		maxTokens = DefaultMaxTokens
	}

	if g.DryRun {
		mock := fmt.Sprintf("[DRY-RUN ai/gemini.Send] model=%s system_len=%d messages=%d last_user_preview=%q",
			model, len(opts.System), len(opts.Messages), truncate(opts.Messages[len(opts.Messages)-1].Content, 200))
		log.Print(mock)
		return &Response{
			Text:         "[DRY-RUN via Gemini backend] AI analysis would appear here.\n\nStub response — set GEMINI_API_KEY and GEMINI_DRY_RUN!=true to see real output.",
			InputTokens:  0,
			OutputTokens: 0,
			Model:        model,
			StopReason:   "dry_run",
		}, nil
	}

	// Assemble the Gemini contents array from opts.Messages. Each turn
	// becomes {"role": "...", "parts": [{"text": "..."}]}. Anthropic's
	// "assistant" role is called "model" in Gemini.
	contents := make([]map[string]any, 0, len(opts.Messages))
	for _, m := range opts.Messages {
		role := m.Role
		if role == "assistant" {
			role = "model"
		}
		contents = append(contents, map[string]any{
			"role":  role,
			"parts": []map[string]string{{"text": m.Content}},
		})
	}

	body := map[string]any{
		"systemInstruction": map[string]any{
			"parts": []map[string]string{{"text": opts.System}},
		},
		"contents": contents,
		"generationConfig": map[string]any{
			"maxOutputTokens": maxTokens,
		},
	}
	if opts.Temperature > 0 {
		body["generationConfig"].(map[string]any)["temperature"] = opts.Temperature
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("ai/gemini: marshal request: %w", err)
	}

	url := geminiAPIBase + model + ":generateContent?key=" + opts.APIKey
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("ai/gemini: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ai/gemini: http do: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ai/gemini: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Gemini errors: {"error":{"code":..,"message":"..","status":".."}}.
		// Surface verbatim — usually rate limit / auth / model-not-found.
		return nil, fmt.Errorf("ai/gemini: http %d: %s", resp.StatusCode, raw)
	}

	var decoded struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
				Role string `json:"role"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			TotalTokenCount      int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
		ModelVersion string `json:"modelVersion"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("ai/gemini: decode response: %w (body=%s)", err, raw)
	}
	if len(decoded.Candidates) == 0 {
		return nil, fmt.Errorf("ai/gemini: no candidates in response (body=%s)", raw)
	}

	var text strings.Builder
	for _, p := range decoded.Candidates[0].Content.Parts {
		text.WriteString(p.Text)
	}

	modelStamped := decoded.ModelVersion
	if modelStamped == "" {
		modelStamped = model
	}
	return &Response{
		Text:         text.String(),
		InputTokens:  decoded.UsageMetadata.PromptTokenCount,
		OutputTokens: decoded.UsageMetadata.CandidatesTokenCount,
		Model:        "gemini:" + modelStamped, // prefix so EstimatedCostUSD can identify provider
		StopReason:   decoded.Candidates[0].FinishReason,
	}, nil
}

// GeminiAPIKeyFromEnv returns GEMINI_API_KEY. Centralized so a future
// per-user override layer can wrap this single helper.
func GeminiAPIKeyFromEnv() string {
	return os.Getenv("GEMINI_API_KEY")
}

// GeminiDryRunFromEnv reports whether GEMINI_DRY_RUN=true.
func GeminiDryRunFromEnv() bool {
	return os.Getenv("GEMINI_DRY_RUN") == "true"
}
