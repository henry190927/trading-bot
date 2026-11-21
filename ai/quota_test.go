package ai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type cannedReply struct {
	status int
	body   string
}

// modelServer answers generateContent per model, from a queue; an empty
// queue answers with the model's last reply. It counts requests per model.
type modelServer struct {
	mu      sync.Mutex
	queues  map[string][]cannedReply
	last    map[string]cannedReply
	counted map[string]int
}

func (m *modelServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	model := strings.TrimSuffix(filepath.Base(r.URL.Path), ":generateContent")
	m.mu.Lock()
	m.counted[model]++
	rep, ok := m.last[model]
	if q := m.queues[model]; len(q) > 0 {
		rep, ok = q[0], true
		m.queues[model] = q[1:]
		m.last[model] = rep
	}
	m.mu.Unlock()
	if !ok {
		rep = cannedReply{http.StatusOK, okBody}
	}
	if rep.status != 0 {
		w.WriteHeader(rep.status)
	}
	_, _ = io.WriteString(w, rep.body)
}

const okBody = `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`

const perDay429 = `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[
 {"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaId":"GenerateRequestsPerDayPerProjectPerModel-FreeTier","quotaValue":"20"}]},
 {"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"23s"}]}}`

const perMinute429 = `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[
 {"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaId":"GenerateRequestsPerMinutePerProjectPerModel-FreeTier","quotaValue":"5"}]},
 {"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"0.05s"}]}}`

// 2026-11-21 10:00 UTC = 02:00 PST: next reset is 2026-11-22 08:00 UTC.
var testNow = time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
var testReset = time.Date(2026, 11, 22, 8, 0, 0, 0, time.UTC)

func newChain(t *testing.T, ms *modelServer, path string, models ...string) *QuotaChain {
	t.Helper()
	srv := httptest.NewServer(ms)
	t.Cleanup(srv.Close)
	g := NewGemini()
	g.BaseURL = srv.URL + "/v1beta/models/"
	q := NewQuotaChain(g, models, path)
	q.Now = func() time.Time { return testNow }
	return q
}

func newModelServer() *modelServer {
	return &modelServer{queues: map[string][]cannedReply{}, last: map[string]cannedReply{}, counted: map[string]int{}}
}

func ask(t *testing.T, q *QuotaChain) error {
	t.Helper()
	_, err := q.SendWithTools(context.Background(), ToolRequest{APIKey: "k", Turns: []Turn{{Role: RoleUser, Text: "hi"}}})
	return err
}

func TestPerDay429SwitchesModelAndRemembers(t *testing.T) {
	ms := newModelServer()
	ms.queues["a"] = []cannedReply{{http.StatusTooManyRequests, perDay429}}
	q := newChain(t, ms, "", "a", "b")

	if err := ask(t, q); err != nil {
		t.Fatal(err)
	}
	if err := ask(t, q); err != nil {
		t.Fatal(err)
	}
	if ms.counted["a"] != 1 {
		t.Errorf("spent model a was asked %d times, want 1 (it must be skipped after its daily 429)", ms.counted["a"])
	}
	if ms.counted["b"] != 2 {
		t.Errorf("model b asked %d times, want 2", ms.counted["b"])
	}
	st := q.Status()
	if !st[0].Blocked || !st[0].BlockedUntil.Equal(testReset) || st[0].Limit != 20 {
		t.Errorf("a status = %+v, want blocked until %s with limit 20", st[0], testReset)
	}
	if st[1].Blocked || st[1].Used != 2 {
		t.Errorf("b status = %+v", st[1])
	}
}

func TestPerMinute429WaitsAndRetriesSameModel(t *testing.T) {
	ms := newModelServer()
	ms.queues["a"] = []cannedReply{{http.StatusTooManyRequests, perMinute429}, {http.StatusOK, okBody}}
	q := newChain(t, ms, "", "a", "b")
	if err := ask(t, q); err != nil {
		t.Fatal(err)
	}
	if ms.counted["a"] != 2 || ms.counted["b"] != 0 {
		t.Errorf("requests a=%d b=%d, want a=2 b=0", ms.counted["a"], ms.counted["b"])
	}
	if st := q.Status(); st[0].Blocked || st[0].Used != 1 {
		t.Errorf("a status = %+v", st[0])
	}
}

// A per-minute delay longer than MaxRateWait is not waited out in the
// request: the next model is tried instead.
func TestLongRateWaitMovesOn(t *testing.T) {
	ms := newModelServer()
	ms.queues["a"] = []cannedReply{{http.StatusTooManyRequests, strings.Replace(perMinute429, "0.05s", "90s", 1)}}
	q := newChain(t, ms, "", "a", "b")
	if err := ask(t, q); err != nil {
		t.Fatal(err)
	}
	if ms.counted["a"] != 1 || ms.counted["b"] != 1 {
		t.Errorf("requests a=%d b=%d", ms.counted["a"], ms.counted["b"])
	}
	if q.Status()[0].Blocked {
		t.Error("a per-minute limit must not block the model for the day")
	}
}

func TestModelNotFoundIsSkipped(t *testing.T) {
	ms := newModelServer()
	ms.queues["a"] = []cannedReply{{http.StatusNotFound, `{"error":{"code":404}}`}}
	q := newChain(t, ms, "", "a", "b")
	if err := ask(t, q); err != nil {
		t.Fatal(err)
	}
	if st := q.Status()[0]; !st.Blocked || !strings.Contains(st.Reason, "not available") {
		t.Errorf("a status = %+v", st)
	}
}

func TestAllSpentIsErrQuotaExhausted(t *testing.T) {
	ms := newModelServer()
	ms.queues["a"] = []cannedReply{{http.StatusTooManyRequests, perDay429}}
	ms.queues["b"] = []cannedReply{{http.StatusTooManyRequests, perDay429}}
	q := newChain(t, ms, "", "a", "b")
	err := ask(t, q)
	if !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), testReset.Format(time.RFC3339)) {
		t.Errorf("error should say when quota returns: %v", err)
	}
	// Nothing is asked again before the reset.
	_ = ask(t, q)
	if ms.counted["a"] != 1 || ms.counted["b"] != 1 {
		t.Errorf("spent models asked again: a=%d b=%d", ms.counted["a"], ms.counted["b"])
	}
}

// A bad request is the caller's problem, not the model's quota: it must not
// fall through and spend another model's requests on the same bad payload.
func TestOtherErrorsDoNotFallThrough(t *testing.T) {
	ms := newModelServer()
	ms.queues["a"] = []cannedReply{{http.StatusBadRequest, `{"error":{"code":400}}`}}
	q := newChain(t, ms, "", "a", "b")
	if err := ask(t, q); err == nil {
		t.Fatal("400 swallowed")
	}
	if ms.counted["b"] != 0 {
		t.Error("a 400 fell through to the next model")
	}
}

func TestStatePersistsAndRollsOver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota.json")
	ms := newModelServer()
	ms.queues["a"] = []cannedReply{{http.StatusTooManyRequests, perDay429}}
	q := newChain(t, ms, path, "a", "b")
	if err := ask(t, q); err != nil {
		t.Fatal(err)
	}

	// A restart (new chain, same file) remembers a is spent and b's count.
	q2 := newChain(t, ms, path, "a", "b")
	st := q2.Status()
	if !st[0].Blocked || st[1].Used != 1 {
		t.Fatalf("after reload: %+v", st)
	}

	// The next Pacific day starts clean.
	q2.Now = func() time.Time { return testReset.Add(time.Minute) }
	st = q2.Status()
	if st[0].Blocked || st[0].Used != 0 || st[1].Used != 0 {
		t.Errorf("after rollover: %+v", st)
	}
}

func TestClassifyRateLimitWithoutDetails(t *testing.T) {
	kind, wait, _ := classifyGeminiError(&GeminiHTTPError{Status: http.StatusTooManyRequests, Body: []byte(`{}`)})
	if kind != quotaPerMinute || wait != 5*time.Second {
		t.Errorf("kind=%v wait=%v", kind, wait)
	}
	if k, _, _ := classifyGeminiError(errors.New("network down")); k != otherError {
		t.Errorf("plain error classified as %v", k)
	}
}

func TestNextResetIsPacificMidnight(t *testing.T) {
	if got := nextReset(testNow); !got.Equal(testReset) {
		t.Errorf("nextReset(%s) = %s, want %s", testNow, got, testReset)
	}
	// In summer (PDT, UTC-7) midnight is 07:00 UTC = 15:00 Taiwan.
	summer := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	if got := nextReset(summer); !got.Equal(time.Date(2026, 7, 2, 7, 0, 0, 0, time.UTC)) {
		t.Errorf("summer reset = %s", got)
	}
}

// Several chat tabs may ask at once; every success must be counted.
func TestConcurrentRequestsCountEverySuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota.json")
	q := newChain(t, newModelServer(), path, "a")
	const n = 12
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = ask(t, q)
		}()
	}
	wg.Wait()
	if got := q.Status()[0].Used; got != n {
		t.Errorf("used = %d, want %d", got, n)
	}
	if got := newChain(t, newModelServer(), path, "a").Status()[0].Used; got != n {
		t.Errorf("persisted used = %d, want %d", got, n)
	}
}
