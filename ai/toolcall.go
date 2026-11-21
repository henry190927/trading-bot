package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Tool calling (function calling) for the copilot's agent loop.
//
// The one-shot Send stays as it is for the existing "AI analysis" buttons.
// A tool-using conversation needs more than text turns: the model asks for
// tools, the caller runs them and sends the results back, and the model
// continues. These types carry that, independent of the wire protocol.

// ToolDecl declares a tool the model may call. Parameters is a JSON Schema
// object (tools.Tool.Schema).
type ToolDecl struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// ToolCall is one tool the model asked for.
type ToolCall struct {
	// ID pairs the call with its result. Gemini supplies one on newer
	// models; when it does not, the client assigns "call-<n>".
	ID   string
	Name string
	Args map[string]any
}

// ToolResult answers one ToolCall.
type ToolResult struct {
	ID      string
	Name    string
	Content string
	IsError bool
}

// Turn roles in a tool conversation.
const (
	RoleUser  = "user"
	RoleModel = "model"
	RoleTool  = "tool"
)

// Turn is one entry of a tool-using conversation.
type Turn struct {
	Role    string       // RoleUser | RoleModel | RoleTool
	Text    string       // user text, or the model's visible text
	Calls   []ToolCall   // model turn: the tools it asked for
	Results []ToolResult // tool turn: the results, one per call

	// Raw is the provider's own encoding of a model turn, replayed verbatim
	// on the next request. Gemini attaches thought signatures to function
	// calls and rejects a follow-up turn that drops them, so a model turn is
	// never rebuilt from Text and Calls when Raw is set.
	Raw json.RawMessage
}

// ToolRequest is one step of a tool conversation.
type ToolRequest struct {
	APIKey      string
	Model       string // empty = the client's current model
	System      string
	Turns       []Turn
	Tools       []ToolDecl
	MaxTokens   int     // 0 = provider default
	Temperature float64 // 0 = provider default
}

// ToolResponse is the model's next turn.
type ToolResponse struct {
	Turn         Turn // Role == RoleModel
	InputTokens  int
	OutputTokens int
	Model        string
	StopReason   string
}

// ToolCaller is a provider that supports tool calling. *GeminiClient
// implements it; the Anthropic client does not yet (docs/ai_copilot_design.md
// §9: Gemini first).
type ToolCaller interface {
	SendWithTools(ctx context.Context, req ToolRequest) (*ToolResponse, error)
}

var _ ToolCaller = (*GeminiClient)(nil)

// SendWithTools sends one step of a tool conversation to Gemini. A 429 is
// returned at once (as *GeminiHTTPError), not retried: QuotaChain owns that
// decision.
func (g *GeminiClient) SendWithTools(ctx context.Context, req ToolRequest) (*ToolResponse, error) {
	if req.APIKey == "" && !g.DryRun {
		return nil, errors.New("ai/gemini: APIKey required (env GEMINI_API_KEY)")
	}
	if len(req.Turns) == 0 {
		return nil, errors.New("ai/gemini: at least one turn required")
	}
	model := req.Model
	if model == "" {
		model = g.CurrentModel()
	}
	if g.DryRun {
		return &ToolResponse{
			Turn:       Turn{Role: RoleModel, Text: "[DRY-RUN ai/gemini.SendWithTools] set GEMINI_API_KEY and GEMINI_DRY_RUN!=true for real output."},
			Model:      "gemini:" + model,
			StopReason: "dry_run",
		}, nil
	}

	payload, err := geminiToolPayload(req)
	if err != nil {
		return nil, err
	}
	raw, err := g.postGenerate(ctx, model, req.APIKey, payload, false)
	if err != nil {
		return nil, err
	}
	resp, err := parseGeminiToolResponse(raw)
	if err != nil {
		return nil, err
	}
	if resp.Model == "" {
		resp.Model = model
	}
	resp.Model = "gemini:" + resp.Model
	return resp, nil
}

// geminiToolPayload builds the generateContent body.
func geminiToolPayload(req ToolRequest) ([]byte, error) {
	contents := make([]any, 0, len(req.Turns))
	for i, t := range req.Turns {
		c, err := geminiContent(t)
		if err != nil {
			return nil, fmt.Errorf("ai/gemini: turn %d: %w", i, err)
		}
		contents = append(contents, c)
	}
	body := map[string]any{"contents": contents}
	if req.System != "" {
		body["systemInstruction"] = map[string]any{"parts": []map[string]string{{"text": req.System}}}
	}
	if len(req.Tools) > 0 {
		decls := make([]map[string]any, 0, len(req.Tools))
		for _, d := range req.Tools {
			decl := map[string]any{"name": d.Name, "description": d.Description}
			// A tool with no parameters omits the schema: Gemini rejects an
			// OBJECT schema whose properties are empty.
			if props, _ := d.Parameters["properties"].(map[string]any); len(props) > 0 {
				decl["parameters"] = d.Parameters
			}
			decls = append(decls, decl)
		}
		body["tools"] = []map[string]any{{"functionDeclarations": decls}}
	}
	gen := map[string]any{}
	if req.MaxTokens > 0 {
		gen["maxOutputTokens"] = req.MaxTokens
	}
	if req.Temperature > 0 {
		gen["temperature"] = req.Temperature
	}
	if len(gen) > 0 {
		body["generationConfig"] = gen
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("ai/gemini: marshal request: %w", err)
	}
	return b, nil
}

// geminiContent encodes one Turn as a Gemini "content".
func geminiContent(t Turn) (any, error) {
	switch t.Role {
	case RoleUser:
		return map[string]any{"role": "user", "parts": []map[string]any{{"text": t.Text}}}, nil
	case RoleModel:
		if len(t.Raw) > 0 {
			return t.Raw, nil // verbatim: keeps thought signatures
		}
		parts := []map[string]any{}
		if t.Text != "" {
			parts = append(parts, map[string]any{"text": t.Text})
		}
		for _, c := range t.Calls {
			fc := map[string]any{"name": c.Name, "args": nonNilArgs(c.Args)}
			parts = append(parts, map[string]any{"functionCall": fc})
		}
		if len(parts) == 0 {
			return nil, errors.New("empty model turn")
		}
		return map[string]any{"role": "model", "parts": parts}, nil
	case RoleTool:
		// Function responses go back in a "user" content, one part each.
		parts := make([]map[string]any, 0, len(t.Results))
		for _, r := range t.Results {
			response := map[string]any{"content": r.Content}
			if r.IsError {
				response = map[string]any{"error": r.Content}
			}
			fr := map[string]any{"name": r.Name, "response": response}
			if r.ID != "" && !strings.HasPrefix(r.ID, assignedIDPrefix) {
				fr["id"] = r.ID
			}
			parts = append(parts, map[string]any{"functionResponse": fr})
		}
		if len(parts) == 0 {
			return nil, errors.New("tool turn without results")
		}
		return map[string]any{"role": "user", "parts": parts}, nil
	}
	return nil, fmt.Errorf("unknown role %q", t.Role)
}

// assignedIDPrefix marks IDs the client made up; they are not sent back to
// Gemini, which only knows the ones it issued.
const assignedIDPrefix = "call-"

func nonNilArgs(a map[string]any) map[string]any {
	if a == nil {
		return map[string]any{}
	}
	return a
}

// parseGeminiToolResponse decodes a generateContent body into the model's
// next turn, keeping the raw content for replay.
func parseGeminiToolResponse(raw []byte) (*ToolResponse, error) {
	var decoded struct {
		Candidates []struct {
			Content      json.RawMessage `json:"content"`
			FinishReason string          `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
		} `json:"usageMetadata"`
		ModelVersion string `json:"modelVersion"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("ai/gemini: decode response: %w (body=%s)", err, raw)
	}
	if len(decoded.Candidates) == 0 {
		return nil, fmt.Errorf("ai/gemini: no candidates in response (body=%s)", raw)
	}
	cand := decoded.Candidates[0]

	var content struct {
		Parts []struct {
			Text         string `json:"text"`
			Thought      bool   `json:"thought"`
			FunctionCall *struct {
				ID   string         `json:"id"`
				Name string         `json:"name"`
				Args map[string]any `json:"args"`
			} `json:"functionCall"`
		} `json:"parts"`
	}
	if len(cand.Content) > 0 {
		if err := json.Unmarshal(cand.Content, &content); err != nil {
			return nil, fmt.Errorf("ai/gemini: decode content: %w", err)
		}
	}

	turn := Turn{Role: RoleModel, Raw: cand.Content}
	var text strings.Builder
	for _, p := range content.Parts {
		switch {
		case p.FunctionCall != nil:
			id := p.FunctionCall.ID
			if id == "" {
				id = fmt.Sprintf("%s%d", assignedIDPrefix, len(turn.Calls)+1)
			}
			turn.Calls = append(turn.Calls, ToolCall{ID: id, Name: p.FunctionCall.Name, Args: nonNilArgs(p.FunctionCall.Args)})
		case p.Thought:
			// Reasoning summary, not part of the answer.
		default:
			text.WriteString(p.Text)
		}
	}
	turn.Text = text.String()
	if len(turn.Raw) == 0 {
		turn.Raw = nil
	}

	return &ToolResponse{
		Turn:         turn,
		InputTokens:  decoded.UsageMetadata.PromptTokenCount,
		OutputTokens: decoded.UsageMetadata.CandidatesTokenCount + decoded.UsageMetadata.ThoughtsTokenCount,
		Model:        decoded.ModelVersion,
		StopReason:   cand.FinishReason,
	}, nil
}
