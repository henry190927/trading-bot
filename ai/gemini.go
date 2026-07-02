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
	"sort"
	"strings"
	"sync"
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

	// DefaultGeminiMaxTokens accommodates 2.5's internal reasoning tokens
	// (which count against maxOutputTokens) PLUS bilingual visible output.
	// Split at 12000: ~4000 tok reasoning + ~4000 tok English block +
	// ~4000 tok zh-TW block. Each language block gets the full multi-
	// section Quant analysis; frontend toggles between them. Bump lower
	// only if a single-language mode ships later.
	DefaultGeminiMaxTokens = 12000

	geminiAPIBase        = "https://generativelanguage.googleapis.com/v1beta/models/"
	geminiRequestTimeout = 90 * time.Second // reasoning models are slower — allow more headroom than the 60s used for Anthropic
)

// GeminiClient is the Google Gemini generateContent counterpart to
// ai.Client (Anthropic). Same Provider interface, same context builders,
// same Quant system prompt — only the wire protocol changes.
//
// Model is mutex-guarded so the /api/ai/model UI endpoint can swap it
// at runtime without a restart. Available-model cache is populated
// lazily on first ListModels() call, refreshed every listModelsTTL.
type GeminiClient struct {
	HTTP   *http.Client
	DryRun bool

	mu    sync.RWMutex
	model string // empty → GEMINI_MODEL env var → DefaultGeminiModel

	listMu       sync.Mutex
	listCache    []string
	listCachedAt time.Time
}

const listModelsTTL = 1 * time.Hour

// NewGemini constructs a GeminiClient. Concurrent-safe.
func NewGemini() *GeminiClient {
	return &GeminiClient{
		HTTP:  &http.Client{Timeout: geminiRequestTimeout},
		model: os.Getenv("GEMINI_MODEL"), // may be "" — Send fills in
	}
}

// IsDryRun on the Gemini client — satisfies Provider.
func (g *GeminiClient) IsDryRun() bool { return g.DryRun }

// CurrentModel returns the active model name. Falls back to
// DefaultGeminiModel when unset. Safe for concurrent read.
func (g *GeminiClient) CurrentModel() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.model != "" {
		return g.model
	}
	return DefaultGeminiModel
}

// SetModel swaps the active model. Caller should validate against the
// ListModels result to avoid setting a nonexistent name (though a bad
// name will just surface as a 404 on the next Send).
func (g *GeminiClient) SetModel(name string) {
	g.mu.Lock()
	g.model = name
	g.mu.Unlock()
}

// LoadPersistedModel reads a single-line model name from disk (if the
// file exists) and calls SetModel. Silent no-op on missing file. Used
// on startup so admin choices survive restarts without editing env.
func (g *GeminiClient) LoadPersistedModel(path string) {
	if path == "" {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	name := strings.TrimSpace(string(b))
	if name == "" {
		return
	}
	g.SetModel(name)
	log.Printf("[ai/gemini] loaded persisted model from %s: %s", path, name)
}

// PersistModel writes the current model name to disk. Called by the
// /api/ai/model POST handler after a successful SetModel so the choice
// survives restarts.
func (g *GeminiClient) PersistModel(path string) error {
	if path == "" {
		return nil
	}
	return os.WriteFile(path, []byte(g.CurrentModel()+"\n"), 0o644)
}

// ListModels returns the text-analysis-relevant Gemini models available
// to the given API key, filtered (no image/tts/computer-use/deep-research
// variants). Cached 1h. Returns cached slice on cache-hit, fetches fresh
// on miss or expiry.
func (g *GeminiClient) ListModels(ctx context.Context, apiKey string) ([]string, error) {
	g.listMu.Lock()
	defer g.listMu.Unlock()
	if len(g.listCache) > 0 && time.Since(g.listCachedAt) < listModelsTTL {
		out := make([]string, len(g.listCache))
		copy(out, g.listCache)
		return out, nil
	}
	if apiKey == "" && !g.DryRun {
		return nil, fmt.Errorf("ai/gemini: ListModels requires APIKey")
	}
	if g.DryRun {
		stub := []string{DefaultGeminiModel, "gemini-2.5-flash", "gemini-2.5-flash-lite", "gemini-2.5-pro", "gemini-2.0-flash", "gemini-3-flash-preview"}
		g.listCache = stub
		g.listCachedAt = time.Now()
		return stub, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://generativelanguage.googleapis.com/v1beta/models?key="+apiKey, nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ai/gemini: ListModels http: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ai/gemini: ListModels read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ai/gemini: ListModels http %d: %s", resp.StatusCode, raw)
	}
	var decoded struct {
		Models []struct {
			Name                       string   `json:"name"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("ai/gemini: ListModels decode: %w", err)
	}
	out := make([]string, 0, len(decoded.Models))
	for _, m := range decoded.Models {
		// strip "models/" prefix
		name := strings.TrimPrefix(m.Name, "models/")
		if !isTextAnalysisModel(name, m.SupportedGenerationMethods) {
			continue
		}
		out = append(out, name)
	}
	// Sort: 3.x > 2.5 > 2.0, flash > lite (deeper first)
	sort.Slice(out, func(i, j int) bool {
		return textAnalysisPriority(out[i]) < textAnalysisPriority(out[j])
	})
	g.listCache = out
	g.listCachedAt = time.Now()
	return out, nil
}

// isTextAnalysisModel decides whether a Gemini model name + its
// supportedGenerationMethods are appropriate for the Quant text
// analysis use case. Excludes image/TTS/computer-use/deep-research
// variants and anything without generateContent support.
func isTextAnalysisModel(name string, methods []string) bool {
	// Must support generateContent.
	hasGenerate := false
	for _, m := range methods {
		if m == "generateContent" {
			hasGenerate = true
			break
		}
	}
	if !hasGenerate {
		return false
	}
	low := strings.ToLower(name)
	// Blocklist: not-for-text-analysis variants.
	blocklist := []string{"image", "tts", "computer-use", "deep-research", "antigravity"}
	for _, b := range blocklist {
		if strings.Contains(low, b) {
			return false
		}
	}
	// Include list: main Gemini text-capable families.
	if strings.HasPrefix(low, "gemini-2.0-flash") ||
		strings.HasPrefix(low, "gemini-2.5-flash") ||
		strings.HasPrefix(low, "gemini-2.5-pro") ||
		strings.HasPrefix(low, "gemini-3-flash") ||
		strings.HasPrefix(low, "gemini-3.1-flash") ||
		strings.HasPrefix(low, "gemini-3-pro") {
		return true
	}
	return false
}

// textAnalysisPriority orders models for the dropdown — newer families
// first, within a family "pro" > "flash" > "lite".
func textAnalysisPriority(name string) int {
	low := strings.ToLower(name)
	fam := 90
	switch {
	case strings.HasPrefix(low, "gemini-3.1-"):
		fam = 10
	case strings.HasPrefix(low, "gemini-3-pro"):
		fam = 20
	case strings.HasPrefix(low, "gemini-3-flash"):
		fam = 30
	case strings.HasPrefix(low, "gemini-2.5-pro"):
		fam = 40
	case strings.HasPrefix(low, "gemini-2.5-flash-lite"):
		fam = 60
	case strings.HasPrefix(low, "gemini-2.5-flash"):
		fam = 50
	case strings.HasPrefix(low, "gemini-2.0-flash-lite"):
		fam = 80
	case strings.HasPrefix(low, "gemini-2.0-flash"):
		fam = 70
	}
	// Stable models before -exp / -preview within same family.
	if strings.Contains(low, "-preview") || strings.Contains(low, "-exp") {
		fam += 5
	}
	return fam
}

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
	// Model resolution: explicit opts wins, otherwise the client's
	// mutex-guarded field (default: env var, then DefaultGeminiModel).
	model := opts.Model
	if model == "" {
		model = g.CurrentModel()
	}
	maxTokens := opts.MaxTokens
	if maxTokens == 0 {
		maxTokens = DefaultGeminiMaxTokens
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

	// Retry transient upstream errors (503 UNAVAILABLE, 429 rate-limited,
	// 500 backend flapping). Google routinely returns 503 during spike
	// windows even though the client's own quota is untouched. Two
	// retries with backoff give ~10s total wait — plenty for a spike to
	// clear without hanging the UI unreasonably.
	var resp *http.Response
	var raw []byte
	for attempt := 0; attempt <= 2; attempt++ {
		if attempt > 0 {
			// Rebuild the request body — some http.Client implementations
			// exhaust bytes.Reader on the first Do(), so subsequent
			// retries would send an empty payload. Cheap to reconstruct.
			req, err = http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
			if err != nil {
				return nil, fmt.Errorf("ai/gemini: rebuild retry request: %w", err)
			}
			req.Header.Set("Content-Type", "application/json")
			// Backoff: 1s → 3s.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt*2+1) * time.Second):
			}
		}
		resp, err = g.HTTP.Do(req)
		if err != nil {
			return nil, fmt.Errorf("ai/gemini: http do: %w", err)
		}
		raw, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("ai/gemini: read response: %w", err)
		}
		if resp.StatusCode == http.StatusOK {
			break
		}
		// Retryable? 429 / 500 / 502 / 503 / 504.
		if resp.StatusCode == 429 || (resp.StatusCode >= 500 && resp.StatusCode <= 504) {
			if attempt < 2 {
				log.Printf("ai/gemini: http %d transient, retrying (attempt %d/2)", resp.StatusCode, attempt+1)
				continue
			}
		}
		// Non-retryable OR retry budget exhausted — surface verbatim.
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
			ThoughtsTokenCount   int `json:"thoughtsTokenCount"` // 2.5+ reasoning tokens (billable against maxOutputTokens)
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
	// Roll reasoning tokens into OutputTokens so the caller sees the full
	// billable output count (matters for future paid tiers + rate-limit
	// awareness). Free tier bills nothing either way.
	outTok := decoded.UsageMetadata.CandidatesTokenCount + decoded.UsageMetadata.ThoughtsTokenCount
	return &Response{
		Text:         text.String(),
		InputTokens:  decoded.UsageMetadata.PromptTokenCount,
		OutputTokens: outTok,
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
