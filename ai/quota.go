package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// QuotaChain sends tool requests through a list of Gemini models, skipping
// any whose free-tier daily quota is spent (docs/ai_copilot_design.md §11).
//
// The free tier is about 20 requests per model per day and resets at
// midnight Pacific time (15:00 or 16:00 in Taiwan). A per-day 429 marks the
// model spent until that reset and the next model is tried; a per-minute
// 429 waits the server's retryDelay and retries the same model once. A 404
// (the key cannot use that model) skips it until the next reset.
//
// Usage is persisted to statePath so a web restart — every deploy — does not
// forget which models are spent and walk into the same 429s again.
type QuotaChain struct {
	client *GeminiClient
	models []string
	path   string

	// Now is the clock; tests replace it.
	Now func() time.Time
	// MaxRateWait caps how long a per-minute 429 may make the caller wait.
	MaxRateWait time.Duration

	mu    sync.Mutex
	state quotaState
}

type quotaState struct {
	Day    string                 `json:"day"` // Pacific date the counters belong to
	Models map[string]*modelQuota `json:"models"`
}

type modelQuota struct {
	Used         int       `json:"used"`            // successful requests today
	Limit        int       `json:"limit,omitempty"` // per-day limit, once Gemini has told us
	BlockedUntil time.Time `json:"blocked_until,omitempty"`
	Reason       string    `json:"reason,omitempty"`
}

// ModelStatus is one model's standing, for the chat UI.
type ModelStatus struct {
	Model        string    `json:"model"`
	Used         int       `json:"used"`
	Limit        int       `json:"limit,omitempty"`     // 0 = not known yet
	Remaining    int       `json:"remaining,omitempty"` // only when Limit is known
	Blocked      bool      `json:"blocked"`
	BlockedUntil time.Time `json:"blocked_until,omitempty"`
	Reason       string    `json:"reason,omitempty"`
}

// ErrQuotaExhausted means every model in the chain is spent or unavailable.
// Wrapped with the earliest time one comes back.
var ErrQuotaExhausted = errors.New("ai: every model's quota is used up")

var _ ToolCaller = (*QuotaChain)(nil)

// NewQuotaChain builds a chain over models (tried in order) on client.
// statePath may be empty (no persistence).
func NewQuotaChain(client *GeminiClient, models []string, statePath string) *QuotaChain {
	q := &QuotaChain{
		client:      client,
		path:        statePath,
		Now:         time.Now,
		MaxRateWait: 30 * time.Second,
		state:       quotaState{Models: map[string]*modelQuota{}},
	}
	seen := map[string]bool{}
	for _, m := range models {
		m = strings.TrimSpace(m)
		if m != "" && !seen[m] {
			seen[m] = true
			q.models = append(q.models, m)
		}
	}
	q.load()
	return q
}

// Models returns the chain, in order.
func (q *QuotaChain) Models() []string { return append([]string(nil), q.models...) }

// SendWithTools sends req on the first model that is not spent. req.Model is
// ignored: the chain picks the model.
func (q *QuotaChain) SendWithTools(ctx context.Context, req ToolRequest) (*ToolResponse, error) {
	if len(q.models) == 0 {
		return nil, errors.New("ai: quota chain has no models")
	}
	var lastErr error
	for _, model := range q.models {
		if q.blocked(model) {
			continue
		}
		resp, err := q.tryModel(ctx, model, req)
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lastErr = err
		if !q.skipModel(model, err) {
			return nil, err // not a quota / availability problem: do not try others
		}
	}
	until := q.earliestReturn().UTC().Format(time.RFC3339)
	if lastErr != nil {
		return nil, fmt.Errorf("%w (next available %s): %w", ErrQuotaExhausted, until, lastErr)
	}
	return nil, fmt.Errorf("%w (next available %s)", ErrQuotaExhausted, until)
}

// tryModel sends on one model, waiting out at most one per-minute 429.
func (q *QuotaChain) tryModel(ctx context.Context, model string, req ToolRequest) (*ToolResponse, error) {
	req.Model = model
	for attempt := 0; ; attempt++ {
		resp, err := q.client.SendWithTools(ctx, req)
		if err == nil {
			q.recordSuccess(model)
			return resp, nil
		}
		kind, wait, _ := classifyGeminiError(err)
		if kind != quotaPerMinute || attempt > 0 || wait > q.MaxRateWait {
			return nil, err
		}
		log.Printf("ai/quota: %s per-minute limit, retrying in %s", model, wait)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// skipModel records why model failed and reports whether the next model
// should be tried.
func (q *QuotaChain) skipModel(model string, err error) bool {
	kind, _, limit := classifyGeminiError(err)
	switch kind {
	case quotaPerDay:
		q.block(model, "daily quota used up", limit)
		return true
	case modelUnavailable:
		q.block(model, "model not available for this key", 0)
		return true
	case quotaPerMinute:
		return true // still rate limited after one wait: try the next model
	}
	return false
}

type quotaKind int

const (
	otherError quotaKind = iota
	quotaPerDay
	quotaPerMinute
	modelUnavailable
)

// classifyGeminiError reads a Gemini error body. Free-tier 429s carry a
// google.rpc.QuotaFailure naming the quota ("...PerDay..." or
// "...PerMinute...") and usually a RetryInfo delay.
func classifyGeminiError(err error) (kind quotaKind, retryAfter time.Duration, limit int) {
	var he *GeminiHTTPError
	if !errors.As(err, &he) {
		return otherError, 0, 0
	}
	if he.Status == http.StatusNotFound {
		return modelUnavailable, 0, 0
	}
	if he.Status != http.StatusTooManyRequests {
		return otherError, 0, 0
	}
	var body struct {
		Error struct {
			Details []struct {
				Type       string `json:"@type"`
				RetryDelay string `json:"retryDelay"`
				Violations []struct {
					QuotaID    string `json:"quotaId"`
					QuotaValue string `json:"quotaValue"`
				} `json:"violations"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(he.Body, &body)
	kind = quotaPerMinute // a 429 that names no quota: treat as a rate limit
	for _, d := range body.Error.Details {
		if d.RetryDelay != "" {
			if dur, perr := time.ParseDuration(d.RetryDelay); perr == nil {
				retryAfter = dur
			}
		}
		for _, v := range d.Violations {
			if strings.Contains(v.QuotaID, "PerDay") {
				kind = quotaPerDay
				if n, perr := strconv.Atoi(v.QuotaValue); perr == nil {
					limit = n
				}
			}
		}
	}
	if kind == quotaPerMinute && retryAfter == 0 {
		retryAfter = 5 * time.Second
	}
	return kind, retryAfter, limit
}

// pacific is where Gemini's daily quota resets.
var pacific = func() *time.Location {
	if loc, err := time.LoadLocation("America/Los_Angeles"); err == nil {
		return loc
	}
	return time.FixedZone("PST", -8*3600)
}()

// nextReset is the next Pacific midnight after t.
func nextReset(t time.Time) time.Time {
	p := t.In(pacific)
	return time.Date(p.Year(), p.Month(), p.Day()+1, 0, 0, 0, 0, pacific)
}

func pacificDay(t time.Time) string { return t.In(pacific).Format("2006-01-02") }

// rollover clears counters and blocks when the Pacific day has changed.
// Caller holds mu.
func (q *QuotaChain) rollover() {
	day := pacificDay(q.Now())
	if q.state.Day != day {
		q.state = quotaState{Day: day, Models: map[string]*modelQuota{}}
	}
}

func (q *QuotaChain) entry(model string) *modelQuota {
	m := q.state.Models[model]
	if m == nil {
		m = &modelQuota{}
		q.state.Models[model] = m
	}
	return m
}

func (q *QuotaChain) blocked(model string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.rollover()
	m := q.state.Models[model]
	return m != nil && q.Now().Before(m.BlockedUntil)
}

func (q *QuotaChain) recordSuccess(model string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.rollover()
	q.entry(model).Used++
	q.saveLocked()
}

func (q *QuotaChain) block(model, reason string, limit int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.rollover()
	m := q.entry(model)
	m.BlockedUntil = nextReset(q.Now())
	m.Reason = reason
	if limit > 0 {
		m.Limit = limit
	}
	log.Printf("ai/quota: %s skipped until %s: %s", model, m.BlockedUntil.Format(time.RFC3339), reason)
	q.saveLocked()
}

func (q *QuotaChain) earliestReturn() time.Time {
	q.mu.Lock()
	defer q.mu.Unlock()
	earliest := nextReset(q.Now())
	for _, m := range q.state.Models {
		if !m.BlockedUntil.IsZero() && m.BlockedUntil.Before(earliest) {
			earliest = m.BlockedUntil
		}
	}
	return earliest
}

// Status reports every model in the chain, in order.
func (q *QuotaChain) Status() []ModelStatus {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.rollover()
	now := q.Now()
	out := make([]ModelStatus, 0, len(q.models))
	for _, model := range q.models {
		st := ModelStatus{Model: model}
		if m := q.state.Models[model]; m != nil {
			st.Used, st.Limit = m.Used, m.Limit
			if m.Limit > 0 && m.Limit > m.Used {
				st.Remaining = m.Limit - m.Used
			}
			if now.Before(m.BlockedUntil) {
				st.Blocked, st.BlockedUntil, st.Reason = true, m.BlockedUntil, m.Reason
			}
		}
		out = append(out, st)
	}
	return out
}

func (q *QuotaChain) load() {
	if q.path == "" {
		return
	}
	b, err := os.ReadFile(q.path)
	if err != nil {
		return // missing file = fresh day
	}
	var st quotaState
	if err := json.Unmarshal(b, &st); err != nil || st.Models == nil {
		log.Printf("ai/quota: ignoring unreadable %s: %v", q.path, err)
		return
	}
	// No rollover here: the day check happens on first use, against the
	// clock the chain actually runs on.
	q.mu.Lock()
	q.state = st
	q.mu.Unlock()
}

// saveLocked writes the state atomically (tmp + rename). Best effort: a
// failed write only means a restart forgets today's counts. Caller holds mu.
func (q *QuotaChain) saveLocked() {
	if q.path == "" {
		return
	}
	b, err := json.MarshalIndent(q.state, "", "  ")
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(q.path), ".quota-*.tmp")
	if err != nil {
		log.Printf("ai/quota: save: %v", err)
		return
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp.Name())
		log.Printf("ai/quota: save: write %v close %v", werr, cerr)
		return
	}
	if err := os.Rename(tmp.Name(), q.path); err != nil {
		_ = os.Remove(tmp.Name())
		log.Printf("ai/quota: save: %v", err)
	}
}
