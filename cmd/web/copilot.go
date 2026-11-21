package main

import (
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/henry190927/trading-bot/ai"
	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/copilot"
	"github.com/henry190927/trading-bot/journal"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/tools"
)

// The web copilot: one chat drawer on every page (docs/ai_copilot_design.md).
// P1 is read-only — the model answers by calling the same tools MCP serves.

// copilotService bundles the agent, the quota chain it sends through, and
// why it is off when it is.
type copilotService struct {
	agent    *copilot.Agent
	chain    *ai.QuotaChain
	apiKey   string
	disabled string // non-empty = off, and why (for the status endpoint / log)
}

// Limits on what the browser may send. The browser keeps the conversation;
// the server only ever sees the tail of it.
const (
	copilotMaxHistory = 12   // messages sent to the model per question
	copilotMaxChars   = 4000 // per message, and for the page context
)

// newCopilot wires the copilot from the environment:
//
//	COPILOT_MODELS       comma-separated fallback Gemini models, tried in
//	                     order after the model selected in the UI (the
//	                     same setting /api/ai/model changes)
//	COPILOT_QUOTA_PATH   per-model usage file (default: copilot_quota.json
//	                     next to the journal)
//	COPILOT_AUDIT_PATH   tool-call log (default: copilot_audit.jsonl next to
//	                     the journal)
func newCopilot(p ai.Provider, bx *bingx.Client) *copilotService {
	g, ok := p.(*ai.GeminiClient)
	if !ok {
		return &copilotService{disabled: "the copilot needs AI_PROVIDER=gemini (tool calling is Gemini-only so far)"}
	}
	key := ai.GeminiAPIKeyFromEnv()
	if key == "" && !g.DryRun {
		return &copilotService{disabled: "GEMINI_API_KEY is not set"}
	}
	dir := filepath.Dir(journal.DefaultPath())
	chain := ai.NewQuotaChain(g, splitList(os.Getenv("COPILOT_MODELS")), envOr("COPILOT_QUOTA_PATH", filepath.Join(dir, "copilot_quota.json")))
	// The model picked in the drawer's selector (persisted by
	// /api/ai/model) always goes first.
	chain.Primary = g.CurrentModel
	reg, err := tools.NewRegistry(tools.ReadTools(webToolDeps(bx))...)
	if err != nil {
		return &copilotService{disabled: "tools: " + err.Error()}
	}
	audit := copilot.NewAuditLog(envOr("COPILOT_AUDIT_PATH", filepath.Join(dir, "copilot_audit.jsonl")))
	return &copilotService{
		chain:  chain,
		apiKey: key,
		agent:  &copilot.Agent{LLM: chain, Tools: reg, APIKey: key, Audit: audit.Write},
	}
}

// webToolDeps gives the shared tools the web's symbol set (every UI symbol,
// not just MCP's four) and the journal this process writes.
func webToolDeps(bx *bingx.Client) tools.Deps {
	var syms []market.Symbol
	for _, s := range uiSymbols {
		if sym, err := resolveWebSymbol(s); err == nil {
			syms = append(syms, sym)
		}
	}
	return tools.Deps{
		Exchange:       bx,
		HasExchangeKey: bx.APIKey != "",
		ReadJournal:    func() ([]journal.Trade, error) { return journal.ReadAll("") },
		ResolveSymbol:  resolveWebSymbol,
		ShortName:      shortSymbol,
		Symbols:        syms,
		SymbolHint:     strings.Join(uiSymbols, ", "),
	}
}

type copilotChatRequest struct {
	Messages []copilot.Message `json:"messages"`
	Page     struct {
		Path    string `json:"path"`
		Title   string `json:"title"`
		Context string `json:"context"`
	} `json:"page"`
}

// handleCopilotChat — POST /api/copilot/chat. One question, one answer
// (P1 does not stream). The conversation lives in the browser.
func (s *server) handleCopilotChat(c *gin.Context) {
	tr := s.translator(langOf(c))
	cp := s.copilot
	if cp == nil || cp.agent == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": tr("copilot.err.off")})
		return
	}
	var req copilotChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": tr("copilot.err.bad")})
		return
	}
	msgs := cleanHistory(req.Messages)
	if len(msgs) == 0 || msgs[len(msgs)-1].Role != "user" {
		c.JSON(http.StatusBadRequest, gin.H{"error": tr("copilot.err.bad")})
		return
	}
	pageCtx := ""
	if req.Page.Path != "" {
		pageCtx = clip("path: "+req.Page.Path+"\ntitle: "+req.Page.Title+"\n"+req.Page.Context, copilotMaxChars)
	}

	agent := *cp.agent // per-request copy: the system prompt follows the page language
	agent.System = copilot.SystemPrompt(langOf(c))
	start := time.Now()
	res, err := agent.Run(c.Request.Context(), msgs, pageCtx)
	quota := cp.chain.Status()
	if err != nil {
		// The Gemini key rides in the request URL, so a transport error can
		// quote it. Never send an error to the browser verbatim; redact it
		// before it reaches the log.
		log.Printf("copilot: %s", redact(err.Error(), cp.apiKey))
		status, msg := http.StatusBadGateway, tr("copilot.err.llm")
		if errors.Is(err, ai.ErrQuotaExhausted) {
			status, msg = http.StatusTooManyRequests, tr("copilot.err.quota")
		}
		c.JSON(status, gin.H{"error": msg, "quota": quota})
		return
	}
	note := ""
	if res.Truncated {
		note = tr("copilot.stop." + res.Reason)
	}
	log.Printf("copilot: %d steps, %d tools, %d/%d tokens, %s, %s", res.Steps, len(res.Tools), res.InputTokens, res.OutputTokens, res.Model, time.Since(start).Round(time.Millisecond))
	c.JSON(http.StatusOK, gin.H{
		"reply":     res.Reply,
		"truncated": res.Truncated,
		"note":      note,
		"steps":     res.Steps,
		"tools":     res.Tools,
		"model":     res.Model,
		"quota":     quota,
	})
}

// handleCopilotStatus — GET /api/copilot/status: on/off and today's usage
// per model, for the drawer header.
func (s *server) handleCopilotStatus(c *gin.Context) {
	cp := s.copilot
	if cp == nil || cp.agent == nil {
		reason := "not configured"
		if cp != nil {
			reason = cp.disabled
		}
		c.JSON(http.StatusOK, gin.H{"enabled": false, "reason": reason})
		return
	}
	c.JSON(http.StatusOK, gin.H{"enabled": true, "quota": cp.chain.Status()})
}

// cleanHistory keeps the last copilotMaxHistory well-formed messages,
// each clipped, and drops empty or unknown-role ones.
func cleanHistory(in []copilot.Message) []copilot.Message {
	out := make([]copilot.Message, 0, len(in))
	for _, m := range in {
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		t := strings.TrimSpace(m.Text)
		if t == "" {
			continue
		}
		out = append(out, copilot.Message{Role: m.Role, Text: clip(t, copilotMaxChars)})
	}
	if len(out) > copilotMaxHistory {
		out = out[len(out)-copilotMaxHistory:]
	}
	// The model expects the conversation to open with the user.
	for len(out) > 0 && out[0].Role != "user" {
		out = out[1:]
	}
	return out
}

func clip(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}

func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "***")
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
