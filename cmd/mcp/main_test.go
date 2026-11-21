package main

import (
	"context"
	"errors"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/tools"
)

// The MCP surface is built from package tools. A conversion that dropped
// "required" or a parameter would change what Claude Desktop sends.
func TestMCPToolKeepsParamsAndRequired(t *testing.T) {
	reg, err := tools.NewRegistry(tools.ReadTools(mcpDeps(bingx.New("", "")))...)
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.All()) != 6 {
		t.Fatalf("%d tools, want the 6 MCP has always had", len(reg.All()))
	}
	c, _ := reg.Get("get_recent_candles")
	mt := mcpTool(c)
	if mt.Name != "get_recent_candles" || mt.Description != c.Description {
		t.Fatalf("name/description changed: %q", mt.Name)
	}
	for _, p := range []string{"symbol", "tf", "n"} {
		if _, ok := mt.InputSchema.Properties[p]; !ok {
			t.Errorf("param %q missing", p)
		}
	}
	req := map[string]bool{}
	for _, r := range mt.InputSchema.Required {
		req[r] = true
	}
	if !req["symbol"] || !req["tf"] || req["n"] {
		t.Errorf("required = %v, want symbol and tf only", mt.InputSchema.Required)
	}
}

// A tool error reaches the model as an error RESULT, not a protocol error —
// the same contract the hand-written handlers had.
func TestMCPHandlerWrapsToolErrors(t *testing.T) {
	failing := tools.Tool{Name: "x", Run: func(context.Context, tools.Args) (string, error) {
		return "", errors.New("no trade with id 9")
	}}
	res, err := mcpHandler(failing)(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatalf("protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("tool error not flagged IsError")
	}
	if txt, ok := res.Content[0].(mcp.TextContent); !ok || txt.Text != "no trade with id 9" {
		t.Errorf("content = %#v", res.Content)
	}

	ok := tools.Tool{Name: "y", Run: func(context.Context, tools.Args) (string, error) { return "fine", nil }}
	res, _ = mcpHandler(ok)(context.Background(), mcp.CallToolRequest{})
	if res.IsError || res.Content[0].(mcp.TextContent).Text != "fine" {
		t.Errorf("success result = %#v", res)
	}
}
