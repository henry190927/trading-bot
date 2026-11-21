// Package tools is the single source of the tools an LLM may call against
// the trading data: journal trades, live positions, candles, macro events,
// backtest facts.
//
// Two front ends register the same tools: cmd/mcp (Claude Desktop / Claude
// Code over MCP) and the web copilot's agent loop. A tool is defined once
// here — name, description, parameters, and a Run func that returns plain
// text for the model — so the two can never drift apart.
//
// Tools are deterministic Go: they never call an LLM. That matters on the
// Gemini free tier, where the budget is requests per day (docs/
// ai_copilot_design.md §11): the scatter is done by tools, the model is
// asked once to gather.
package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// ParamType is the JSON Schema type of a tool parameter.
type ParamType string

const (
	String  ParamType = "string"
	Number  ParamType = "number"
	Integer ParamType = "integer"
	Boolean ParamType = "boolean"
)

// Param describes one tool argument.
type Param struct {
	Name        string
	Type        ParamType
	Description string
	Required    bool
	Enum        []string // optional; only for String
}

// Kind separates tools the agent may run on its own from tools that only
// propose a change for a human to confirm (docs/ai_copilot_design.md §3).
type Kind int

const (
	// Read tools run automatically; their result goes back to the model.
	Read Kind = iota
	// Propose tools never change anything. Their Run validates the
	// arguments and returns a proposal; a person confirms it in the UI,
	// which then calls the existing guarded endpoint.
	Propose
)

func (k Kind) String() string {
	if k == Propose {
		return "propose"
	}
	return "read"
}

// Tool is one callable tool.
type Tool struct {
	Name        string
	Description string
	Params      []Param
	Kind        Kind
	// Run executes the tool. The returned text is what the model sees. An
	// error is reported to the model as a tool error (it may retry with
	// different arguments); it never aborts the conversation.
	Run func(ctx context.Context, args Args) (string, error)
}

// Schema returns the parameters as a JSON Schema object, the shape both MCP
// and Gemini function declarations accept.
func (t Tool) Schema() map[string]any {
	props := map[string]any{}
	required := []string{}
	for _, p := range t.Params {
		prop := map[string]any{"type": string(p.Type)}
		if p.Description != "" {
			prop["description"] = p.Description
		}
		if len(p.Enum) > 0 {
			prop["enum"] = p.Enum
		}
		props[p.Name] = prop
		if p.Required {
			required = append(required, p.Name)
		}
	}
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// Registry holds tools by name.
type Registry struct {
	list   []Tool
	byName map[string]Tool
}

// NewRegistry builds a registry. Duplicate or empty names are an error: a
// second tool with the same name would silently shadow the first.
func NewRegistry(ts ...Tool) (*Registry, error) {
	r := &Registry{byName: map[string]Tool{}}
	for _, t := range ts {
		if strings.TrimSpace(t.Name) == "" || t.Run == nil {
			return nil, fmt.Errorf("tools: tool %q has no name or no Run", t.Name)
		}
		if _, dup := r.byName[t.Name]; dup {
			return nil, fmt.Errorf("tools: duplicate tool name %q", t.Name)
		}
		r.byName[t.Name] = t
		r.list = append(r.list, t)
	}
	return r, nil
}

// All returns the tools in registration order.
func (r *Registry) All() []Tool { return append([]Tool(nil), r.list...) }

// Get returns the tool named name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.byName[name]
	return t, ok
}

// Names returns the tool names, sorted.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.list))
	for _, t := range r.list {
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out
}

// Args are a tool call's decoded JSON arguments. JSON numbers arrive as
// float64 from both MCP and Gemini.
type Args map[string]any

// String returns args[key] as a string, or "" when absent or not a string.
func (a Args) String(key string) string {
	if v, ok := a[key].(string); ok {
		return v
	}
	return ""
}

// Number returns args[key] as a float64.
func (a Args) Number(key string) (float64, bool) {
	switch v := a[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	}
	return 0, false
}

// Int returns args[key] as a positive int, or def when absent or <= 0.
func (a Args) Int(key string, def int) int {
	if v, ok := a.Number(key); ok && v > 0 {
		return int(v)
	}
	return def
}
