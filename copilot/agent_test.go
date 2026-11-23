package copilot

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/ai"
	"github.com/henry190927/trading-bot/tools"
)

// scriptLLM answers each request with the next scripted turn and keeps the
// requests it saw.
type scriptLLM struct {
	mu    sync.Mutex
	turns []ai.Turn
	errAt int // 1-based request number that fails; 0 = never
	reqs  []ai.ToolRequest
}

func (s *scriptLLM) SendWithTools(_ context.Context, req ai.ToolRequest) (*ai.ToolResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, req)
	n := len(s.reqs)
	if s.errAt == n {
		return nil, errors.New("quota gone")
	}
	turn := s.turns[len(s.turns)-1]
	if n <= len(s.turns) {
		turn = s.turns[n-1]
	}
	return &ai.ToolResponse{Turn: turn, InputTokens: 10, OutputTokens: 5, Model: "gemini:fake"}, nil
}

func call(name string, args map[string]any) ai.ToolCall {
	return ai.ToolCall{ID: "id-" + name, Name: name, Args: args}
}

func registry(t *testing.T, extra ...tools.Tool) *tools.Registry {
	t.Helper()
	base := []tools.Tool{
		{Name: "fast", Kind: tools.Read, Run: func(context.Context, tools.Args) (string, error) { return "fast-result", nil }},
		{Name: "slow", Kind: tools.Read, Run: func(ctx context.Context, _ tools.Args) (string, error) {
			select {
			case <-time.After(100 * time.Millisecond):
				return "slow-result", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}},
		{Name: "echo", Kind: tools.Read, Params: []tools.Param{{Name: "x", Type: tools.String}},
			Run: func(_ context.Context, a tools.Args) (string, error) { return "echo:" + a.String("x"), nil }},
		{Name: "propose_x", Kind: tools.Propose, Run: func(context.Context, tools.Args) (string, error) { return "should not run", nil }},
	}
	reg, err := tools.NewRegistry(append(base, extra...)...)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func ask(t *testing.T, a *Agent, q string) *Result {
	t.Helper()
	res, err := a.Run(context.Background(), []Message{{Role: "user", Text: q}}, "")
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestDirectAnswer(t *testing.T) {
	llm := &scriptLLM{turns: []ai.Turn{{Role: ai.RoleModel, Text: " BTC looks flat. "}}}
	res := ask(t, &Agent{LLM: llm, Tools: registry(t)}, "hi")
	if res.Reply != "BTC looks flat." || res.Steps != 1 || res.Truncated {
		t.Fatalf("res = %+v", res)
	}
}

// Two calls in one step run in parallel and come back in the calls' order.
func TestToolsRunInParallelAndKeepOrder(t *testing.T) {
	llm := &scriptLLM{turns: []ai.Turn{
		{Role: ai.RoleModel, Calls: []ai.ToolCall{call("slow", nil), call("fast", nil), call("slow", nil)}},
		{Role: ai.RoleModel, Text: "done"},
	}}
	start := time.Now()
	res := ask(t, &Agent{LLM: llm, Tools: registry(t)}, "check")
	if el := time.Since(start); el > 250*time.Millisecond {
		t.Errorf("three tools (two 100ms) took %v — not run in parallel", el)
	}
	if res.Reply != "done" || res.Steps != 2 || len(res.Tools) != 3 {
		t.Fatalf("res = %+v", res)
	}
	got := llm.reqs[1].Turns[len(llm.reqs[1].Turns)-1]
	if got.Role != ai.RoleTool || len(got.Results) != 3 {
		t.Fatalf("tool turn = %+v", got)
	}
	want := []string{"slow-result", "fast-result", "slow-result"}
	for i, r := range got.Results {
		if r.Content != want[i] || r.ID != llm.turns[0].Calls[i].ID {
			t.Errorf("result %d = %+v, want %s", i, r, want[i])
		}
	}
}

func TestMaxStepsIsReportedNotHidden(t *testing.T) {
	llm := &scriptLLM{turns: []ai.Turn{{Role: ai.RoleModel, Text: "still checking", Calls: []ai.ToolCall{call("fast", nil)}}}}
	res := ask(t, &Agent{LLM: llm, Tools: registry(t), MaxSteps: 3}, "loop")
	if !res.Truncated || res.Reason != StopMaxSteps || res.Steps != 3 || len(llm.reqs) != 3 {
		t.Fatalf("res = %+v, requests = %d", res, len(llm.reqs))
	}
	if res.Reply != "still checking" {
		t.Errorf("reply = %q, want the last text the model gave", res.Reply)
	}
}

func TestToolFailuresBecomeErrorResults(t *testing.T) {
	boom := tools.Tool{Name: "boom", Kind: tools.Read, Run: func(context.Context, tools.Args) (string, error) { panic("kaboom") }}
	bad := tools.Tool{Name: "bad", Kind: tools.Read, Run: func(context.Context, tools.Args) (string, error) { return "", errors.New("no trade with id 9") }}
	stuck := tools.Tool{Name: "stuck", Kind: tools.Read, Run: func(ctx context.Context, _ tools.Args) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}}
	llm := &scriptLLM{turns: []ai.Turn{
		{Role: ai.RoleModel, Calls: []ai.ToolCall{call("nope", nil), call("propose_x", nil), call("boom", nil), call("bad", nil), call("stuck", nil)}},
		{Role: ai.RoleModel, Text: "recovered"},
	}}
	var audited []ToolTrace
	var mu sync.Mutex
	a := &Agent{LLM: llm, Tools: registry(t, boom, bad, stuck), ToolTimeout: 50 * time.Millisecond,
		Audit: func(tr ToolTrace) { mu.Lock(); audited = append(audited, tr); mu.Unlock() }}
	res := ask(t, a, "go")
	if res.Reply != "recovered" {
		t.Fatalf("conversation did not continue: %+v", res)
	}
	results := llm.reqs[1].Turns[len(llm.reqs[1].Turns)-1].Results
	wantSub := []string{"unknown tool nope", "not available in this chat", "kaboom", "no trade with id 9", "deadline exceeded"}
	for i, r := range results {
		if !r.IsError || !strings.Contains(r.Content, wantSub[i]) {
			t.Errorf("result %d = %+v, want error containing %q", i, r, wantSub[i])
		}
	}
	if len(audited) != 5 {
		t.Errorf("audited %d calls, want 5", len(audited))
	}
}

func TestPageContextGoesOnTheLastQuestionOnly(t *testing.T) {
	llm := &scriptLLM{turns: []ai.Turn{{Role: ai.RoleModel, Text: "ok"}}}
	a := &Agent{LLM: llm, Tools: registry(t)}
	hist := []Message{{Role: "user", Text: "first"}, {Role: "assistant", Text: "answer"}, {Role: "user", Text: "second"}}
	if _, err := a.Run(context.Background(), hist, "/today: BTC long #93"); err != nil {
		t.Fatal(err)
	}
	turns := llm.reqs[0].Turns
	if turns[0].Text != "first" || turns[1].Role != ai.RoleModel {
		t.Errorf("history altered: %+v", turns[:2])
	}
	if !strings.Contains(turns[2].Text, "/today: BTC long #93") || !strings.HasSuffix(turns[2].Text, "second") {
		t.Errorf("last question = %q", turns[2].Text)
	}
}

func TestBadConversation(t *testing.T) {
	a := &Agent{LLM: &scriptLLM{}, Tools: registry(t)}
	for _, h := range [][]Message{nil, {{Role: "assistant", Text: "x"}}, {{Role: "user", Text: "  "}}} {
		if _, err := a.Run(context.Background(), h, ""); !errors.Is(err, ErrBadConversation) {
			t.Errorf("history %v: err = %v", h, err)
		}
	}
}

func TestLLMErrorReturnsPartialResult(t *testing.T) {
	llm := &scriptLLM{errAt: 2, turns: []ai.Turn{{Role: ai.RoleModel, Calls: []ai.ToolCall{call("fast", nil)}}}}
	res, err := (&Agent{LLM: llm, Tools: registry(t)}).Run(context.Background(), []Message{{Role: "user", Text: "q"}}, "")
	if err == nil || res == nil || res.Steps != 1 || len(res.Tools) != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestDeclarationsOfferOnlyReadTools(t *testing.T) {
	llm := &scriptLLM{turns: []ai.Turn{{Role: ai.RoleModel, Text: "ok"}}}
	ask(t, &Agent{LLM: llm, Tools: registry(t)}, "q")
	for _, d := range llm.reqs[0].Tools {
		if d.Name == "propose_x" {
			t.Error("a propose tool was offered before the confirm-card flow exists")
		}
	}
	if len(llm.reqs[0].Tools) != 3 {
		t.Errorf("offered %d tools, want 3", len(llm.reqs[0].Tools))
	}
}

func TestLongToolOutputIsTruncated(t *testing.T) {
	long := tools.Tool{Name: "long", Kind: tools.Read, Run: func(context.Context, tools.Args) (string, error) { return strings.Repeat("字", 50), nil }}
	llm := &scriptLLM{turns: []ai.Turn{{Role: ai.RoleModel, Calls: []ai.ToolCall{call("long", nil)}}, {Role: ai.RoleModel, Text: "ok"}}}
	ask(t, &Agent{LLM: llm, Tools: registry(t, long), MaxToolOutput: 10}, "q")
	got := llm.reqs[1].Turns[len(llm.reqs[1].Turns)-1].Results[0].Content
	if !strings.HasPrefix(got, strings.Repeat("字", 10)+"\n…[truncated]") {
		t.Errorf("truncated = %q", got)
	}
}

// NeedsData forces a tool call on the first step only, and only for the
// questions it picks.
func TestNeedsDataRequiresAToolOnTheFirstStep(t *testing.T) {
	needs := func(q string) bool { return strings.Contains(q, "BTC") }
	llm := &scriptLLM{turns: []ai.Turn{{Role: ai.RoleModel, Calls: []ai.ToolCall{call("fast", nil)}}, {Role: ai.RoleModel, Text: "ok"}}}
	ask(t, &Agent{LLM: llm, Tools: registry(t), NeedsData: needs}, "how is BTC")
	if !llm.reqs[0].RequireTool || llm.reqs[1].RequireTool {
		t.Errorf("RequireTool per step = %v, %v; want true, false", llm.reqs[0].RequireTool, llm.reqs[1].RequireTool)
	}
	llm2 := &scriptLLM{turns: []ai.Turn{{Role: ai.RoleModel, Text: "hi"}}}
	ask(t, &Agent{LLM: llm2, Tools: registry(t), NeedsData: needs}, "hello")
	if llm2.reqs[0].RequireTool {
		t.Error("a greeting was forced to call a tool")
	}
}
