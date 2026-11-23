package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeGemini answers generateContent with canned bodies in order and keeps
// every request body.
type fakeGemini struct {
	mu       sync.Mutex
	replies  []string
	statuses []int
	bodies   []map[string]any
	paths    []string
}

func (f *fakeGemini) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	f.bodies = append(f.bodies, m)
	f.paths = append(f.paths, r.URL.Path)
	i := len(f.bodies) - 1
	if i < len(f.statuses) && f.statuses[i] != 0 {
		w.WriteHeader(f.statuses[i])
	}
	if i < len(f.replies) {
		_, _ = io.WriteString(w, f.replies[i])
	}
}

func newFakeClient(t *testing.T, f *fakeGemini) *GeminiClient {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	g := NewGemini()
	g.BaseURL = srv.URL + "/v1beta/models/"
	g.SetModel("test-model")
	return g
}

var testTools = []ToolDecl{
	{Name: "get_trade", Description: "one trade", Parameters: map[string]any{
		"type": "object", "properties": map[string]any{"id": map[string]any{"type": "number"}}, "required": []string{"id"},
	}},
	{Name: "get_backtest_facts", Description: "facts", Parameters: map[string]any{"type": "object", "properties": map[string]any{}}},
}

const callReply = `{"candidates":[{"content":{"role":"model","parts":[
  {"functionCall":{"name":"get_trade","args":{"id":2}},"thoughtSignature":"sig-abc"}
]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":3,"thoughtsTokenCount":4},"modelVersion":"test-model-001"}`

const textReply = `{"candidates":[{"content":{"role":"model","parts":[
  {"text":"thinking about it","thought":true},
  {"text":"Trade #2 is short ETH."}
]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":6}}`

func TestSendWithToolsRoundTripReplaysThoughtSignature(t *testing.T) {
	f := &fakeGemini{replies: []string{callReply, textReply}}
	g := newFakeClient(t, f)
	ctx := context.Background()

	user := Turn{Role: RoleUser, Text: "How is trade 2?"}
	r1, err := g.SendWithTools(ctx, ToolRequest{APIKey: "k", System: "be a quant", Turns: []Turn{user}, Tools: testTools})
	if err != nil {
		t.Fatal(err)
	}

	// Request 1 shape.
	b1 := f.bodies[0]
	if !strings.HasSuffix(f.paths[0], "/test-model:generateContent") {
		t.Errorf("path = %s", f.paths[0])
	}
	if b1["systemInstruction"] == nil {
		t.Error("system instruction missing")
	}
	decls := b1["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)
	if _, has := decls[0].(map[string]any)["parameters"]; !has {
		t.Error("get_trade lost its parameters")
	}
	if _, has := decls[1].(map[string]any)["parameters"]; has {
		t.Error("a tool with no params must omit parameters (Gemini rejects empty OBJECT schemas)")
	}

	// Response 1 parsed.
	if len(r1.Turn.Calls) != 1 || r1.Turn.Calls[0].Name != "get_trade" || r1.Turn.Calls[0].Args["id"] != 2.0 {
		t.Fatalf("calls = %+v", r1.Turn.Calls)
	}
	if r1.Turn.Calls[0].ID != "call-1" {
		t.Errorf("assigned id = %q", r1.Turn.Calls[0].ID)
	}
	if !strings.Contains(string(r1.Turn.Raw), "sig-abc") {
		t.Fatal("raw model content lost the thought signature")
	}
	if r1.OutputTokens != 7 || r1.Model != "gemini:test-model-001" {
		t.Errorf("tokens=%d model=%s", r1.OutputTokens, r1.Model)
	}

	// Request 2: model turn replayed verbatim, tool result as a functionResponse.
	toolTurn := Turn{Role: RoleTool, Results: []ToolResult{{ID: r1.Turn.Calls[0].ID, Name: "get_trade", Content: "short ETH 2000"}}}
	r2, err := g.SendWithTools(ctx, ToolRequest{APIKey: "k", System: "be a quant", Turns: []Turn{user, r1.Turn, toolTurn}, Tools: testTools})
	if err != nil {
		t.Fatal(err)
	}
	contents := f.bodies[1]["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents = %d, want 3", len(contents))
	}
	replayed, _ := json.Marshal(contents[1])
	if !strings.Contains(string(replayed), `"thoughtSignature":"sig-abc"`) {
		t.Errorf("model turn not replayed verbatim: %s", replayed)
	}
	tc := contents[2].(map[string]any)
	if tc["role"] != "user" {
		t.Errorf("function responses go in a user content, got role %v", tc["role"])
	}
	fr := tc["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
	if fr["name"] != "get_trade" || fr["response"].(map[string]any)["content"] != "short ETH 2000" {
		t.Errorf("functionResponse = %v", fr)
	}
	if _, has := fr["id"]; has {
		t.Error("a client-assigned id was sent to Gemini")
	}

	// Response 2: thought parts are not part of the answer.
	if r2.Turn.Text != "Trade #2 is short ETH." || len(r2.Turn.Calls) != 0 {
		t.Errorf("turn = %+v", r2.Turn)
	}
}

func TestGeminiIssuedIDsAndErrorsGoBack(t *testing.T) {
	c, err := geminiContent(Turn{Role: RoleTool, Results: []ToolResult{
		{ID: "fc-9", Name: "get_trade", Content: "ok"},
		{ID: "call-2", Name: "get_open_positions", Content: "BINGX_API_KEY not configured", IsError: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(c)
	s := string(b)
	if !strings.Contains(s, `"id":"fc-9"`) {
		t.Errorf("Gemini's own id not echoed: %s", s)
	}
	if !strings.Contains(s, `"error":"BINGX_API_KEY not configured"`) {
		t.Errorf("tool error not under error: %s", s)
	}
}

func TestModelTurnWithoutRawIsRebuilt(t *testing.T) {
	c, err := geminiContent(Turn{Role: RoleModel, Text: "let me check", Calls: []ToolCall{{Name: "get_backtest_facts"}}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(c)
	if !strings.Contains(string(b), `"functionCall":{"args":{},"name":"get_backtest_facts"}`) {
		t.Errorf("rebuilt turn = %s", b)
	}
	if _, err := geminiContent(Turn{Role: RoleModel}); err == nil {
		t.Error("empty model turn accepted")
	}
	if _, err := geminiContent(Turn{Role: "system"}); err == nil {
		t.Error("unknown role accepted")
	}
}

// A 400 is not retried and comes back typed, so the copilot can tell a bad
// request from a quota answer.
func TestHTTPErrorIsTypedAndNotRetried(t *testing.T) {
	f := &fakeGemini{statuses: []int{http.StatusBadRequest}, replies: []string{`{"error":{"code":400,"message":"bad"}}`}}
	g := newFakeClient(t, f)
	_, err := g.SendWithTools(context.Background(), ToolRequest{APIKey: "k", Turns: []Turn{{Role: RoleUser, Text: "hi"}}})
	var he *GeminiHTTPError
	if !errors.As(err, &he) || he.Status != http.StatusBadRequest {
		t.Fatalf("err = %v", err)
	}
	if len(f.bodies) != 1 {
		t.Errorf("400 was retried: %d requests", len(f.bodies))
	}
}

// Send goes through the same postGenerate now; its behaviour must not change.
func TestSendStillWorksThroughPostGenerate(t *testing.T) {
	f := &fakeGemini{replies: []string{textReply}}
	g := newFakeClient(t, f)
	resp, err := g.Send(context.Background(), SendOptions{APIKey: "k", System: "s", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	// Send has always concatenated every part, thought parts included.
	if !strings.Contains(resp.Text, "Trade #2 is short ETH.") || resp.Model != "gemini:test-model" {
		t.Errorf("resp = %+v", resp)
	}
}

// RequireTool sends functionCallingConfig mode ANY; without it the model
// chooses (no toolConfig at all).
func TestRequireToolSetsModeAny(t *testing.T) {
	f := &fakeGemini{replies: []string{callReply, textReply}}
	g := newFakeClient(t, f)
	turns := []Turn{{Role: RoleUser, Text: "how is BTC"}}
	if _, err := g.SendWithTools(context.Background(), ToolRequest{APIKey: "k", Turns: turns, Tools: testTools, RequireTool: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.SendWithTools(context.Background(), ToolRequest{APIKey: "k", Turns: turns, Tools: testTools}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := f.bodies[0]["toolConfig"].(map[string]any)
	fc, _ := cfg["functionCallingConfig"].(map[string]any)
	if fc["mode"] != "ANY" {
		t.Errorf("toolConfig = %v, want functionCallingConfig.mode ANY", f.bodies[0]["toolConfig"])
	}
	if _, ok := f.bodies[1]["toolConfig"]; ok {
		t.Errorf("toolConfig sent without RequireTool: %v", f.bodies[1]["toolConfig"])
	}
}
