package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"myFirstGo/trading-bot/bingx"
)

var errTest = errors.New("read: websocket: close 1006 (abnormal closure)")

// The hub's contract is what makes the fan-out safe, so it is tested
// directly rather than through an HTTP round trip.
//
// subscribe() STARTS pollLoop, so a hub with no source would dereference nil
// the moment a test attached a subscriber. The source is injected, and here it
// is a fixed in-memory snapshot — no exchange, no network, no timing.
func newTestHub() *tickerHub {
	return newTickerHub(func(context.Context) any {
		return map[string]any{"tickers": []any{}}
	})
}

// A slow subscriber must never block the hub, and must end up holding the
// NEWEST snapshot — drop-oldest. Drop-newest would leave a stalled tab
// showing a stale price forever once it caught up.
func TestBroadcastDropsOldestNotNewest(t *testing.T) {
	h := newTestHub()
	ch := make(chan []byte, subBuffer)
	h.subs[ch] = struct{}{}

	h.broadcast([]byte(`{"n":1}`))
	h.broadcast([]byte(`{"n":2}`))
	h.broadcast([]byte(`{"n":3}`)) // subscriber has read nothing yet

	got := string(<-ch)
	if got != `{"n":3}` {
		t.Fatalf("pending snapshot = %s, want the newest {\"n\":3}", got)
	}
	select {
	case extra := <-ch:
		t.Errorf("buffer should hold exactly one snapshot, also got %s", extra)
	default:
	}
}

// broadcast must complete even with a subscriber that never reads — the
// whole point of the bounded channel.
func TestBroadcastNeverBlocks(t *testing.T) {
	h := newTestHub()
	dead := make(chan []byte, subBuffer)
	h.subs[dead] = struct{}{}

	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			h.broadcast([]byte(`{"x":1}`))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("broadcast blocked on a subscriber that never reads")
	}
}

// Every attached subscriber gets the snapshot, and `last` is retained for
// replay to whoever connects next.
func TestBroadcastFansOutAndRetainsLast(t *testing.T) {
	h := newTestHub()
	var chans []chan []byte
	for i := 0; i < 4; i++ {
		ch := make(chan []byte, subBuffer)
		h.subs[ch] = struct{}{}
		chans = append(chans, ch)
	}
	h.broadcast([]byte(`{"v":7}`))
	for i, ch := range chans {
		select {
		case got := <-ch:
			if string(got) != `{"v":7}` {
				t.Errorf("sub %d got %s", i, got)
			}
		default:
			t.Errorf("sub %d received nothing", i)
		}
	}
	if string(h.last) != `{"v":7}` {
		t.Errorf("last = %s, want the broadcast payload for replay", h.last)
	}
}

// A new subscriber is handed the last snapshot so a fresh tab paints
// immediately instead of waiting a full interval.
func TestSubscribeReplaysLastSnapshot(t *testing.T) {
	h := newTestHub()
	// Attach and release one subscriber to populate `last` without letting
	// pollLoop run (it would need a real server).
	h.last = []byte(`{"seed":1}`)

	ch, last, release := h.subscribe()
	if string(last) != `{"seed":1}` {
		t.Errorf("replayed snapshot = %s, want the seeded one", last)
	}
	if ch == nil {
		t.Fatal("want a channel")
	}
	release()
}

// Release must be idempotent and must not close an already-closed channel —
// handleStreamTickers defers it, and an early return path can reach it twice.
func TestReleaseIsIdempotent(t *testing.T) {
	h := newTestHub()
	h.last = []byte(`{}`)
	_, _, release := h.subscribe()
	release()
	release() // must not panic on a double close
	if len(h.subs) != 0 {
		t.Errorf("subs left after release: %d", len(h.subs))
	}
}

// The poller runs only while someone is watching: an idle deployment must
// make zero exchange calls. running is the observable proxy for that.
func TestPollerLifecycleFollowsSubscribers(t *testing.T) {
	h := newTestHub()
	h.last = []byte(`{}`)

	if h.running {
		t.Fatal("a fresh hub must not be polling")
	}
	_, _, rel1 := h.subscribe()
	h.mu.Lock()
	r1 := h.running
	h.mu.Unlock()
	if !r1 {
		t.Error("first subscriber must start the poller")
	}

	_, _, rel2 := h.subscribe()
	rel1()
	h.mu.Lock()
	r2, n := h.running, len(h.subs)
	h.mu.Unlock()
	if !r2 {
		t.Error("poller must keep running while a second subscriber remains")
	}
	if n != 1 {
		t.Errorf("subs = %d, want 1", n)
	}

	rel2()
	h.mu.Lock()
	r3 := h.running
	h.mu.Unlock()
	if r3 {
		t.Error("last subscriber leaving must stop the poller")
	}

	// And it must be restartable — a second visitor after an idle period.
	_, _, rel3 := h.subscribe()
	h.mu.Lock()
	r4 := h.running
	h.mu.Unlock()
	if !r4 {
		t.Error("poller must restart for a new subscriber")
	}
	rel3()
}

// Concurrent subscribe/release/broadcast must be race-free. Run with -race.
func TestHubConcurrency(t *testing.T) {
	h := newTestHub()
	h.last = []byte(`{}`)
	var wg sync.WaitGroup

	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				h.broadcast([]byte(`{"t":1}`))
			}
		}
	}()

	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				ch, _, rel := h.subscribe()
				select {
				case <-ch:
				default:
				}
				rel()
			}
		}()
	}
	// Let the churn finish, then stop the broadcaster.
	waited := make(chan struct{})
	go func() { wg.Wait(); close(waited) }()
	time.Sleep(150 * time.Millisecond)
	close(stop)
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("hub deadlocked under concurrent subscribe/release/broadcast")
	}
	if len(h.subs) != 0 {
		t.Errorf("subs leaked: %d", len(h.subs))
	}
}

// The drop counter is the whole point of the observability work: a silent
// drop is correct behaviour that used to be invisible.
func TestDropAndDeliverCounters(t *testing.T) {
	h := newTestHub()
	ch := make(chan []byte, subBuffer)
	h.subs[ch] = struct{}{}

	h.broadcast([]byte(`{"n":1}`)) // fits the 1-slot buffer -> delivered
	if got := h.nDelivered.Load(); got != 1 {
		t.Fatalf("delivered = %d, want 1", got)
	}
	if got := h.nDropped.Load(); got != 0 {
		t.Fatalf("dropped = %d, want 0 — nothing was superseded yet", got)
	}

	// Nobody read, so this one evicts the pending snapshot: 1 drop, and the
	// replacement counts as a delivery.
	h.broadcast([]byte(`{"n":2}`))
	if got := h.nDropped.Load(); got != 1 {
		t.Errorf("dropped = %d, want 1", got)
	}
	if got := h.nDelivered.Load(); got != 2 {
		t.Errorf("delivered = %d, want 2", got)
	}

	h.broadcast([]byte(`{"n":3}`))
	st := h.stats()
	if st.Dropped != 2 || st.Delivered != 3 {
		t.Errorf("stats dropped/delivered = %d/%d, want 2/3", st.Dropped, st.Delivered)
	}
	// 2 drops out of 5 outcomes = 40%.
	if st.DropPct < 39.9 || st.DropPct > 40.1 {
		t.Errorf("drop_pct = %.1f, want 40.0", st.DropPct)
	}
	// A reader that keeps up must not be counted as a drop.
	<-ch
	h.broadcast([]byte(`{"n":4}`))
	if got := h.nDropped.Load(); got != 2 {
		t.Errorf("dropped = %d after a drained buffer, want it unchanged at 2", got)
	}
}

// A healthy stream must log nothing; a starved one must log a DELTA, once.
func TestReportDropsOnlyOnChange(t *testing.T) {
	h := newTestHub()
	if h.reportDrops(); h.reportedDrops.Load() != 0 {
		t.Error("no drops must leave the report watermark at 0")
	}
	h.nDropped.Store(5)
	h.reportDrops()
	if got := h.reportedDrops.Load(); got != 5 {
		t.Errorf("watermark = %d, want 5", got)
	}
	// Second call with no new drops must be a no-op (this is what keeps a
	// steady-state starved stream from logging every 3s forever).
	h.reportDrops()
	if got := h.reportedDrops.Load(); got != 5 {
		t.Errorf("watermark moved without new drops: %d", got)
	}
}

func TestStatsSubscriberGauge(t *testing.T) {
	h := newTestHub()
	h.last = []byte(`{}`)

	if st := h.stats(); st.Subscribers != 0 || st.Running || st.LastAgeSec != -1 {
		t.Fatalf("fresh hub stats = %+v, want 0 subs / not running / age -1", st)
	}

	_, _, r1 := h.subscribe()
	_, _, r2 := h.subscribe()
	_, _, r3 := h.subscribe()
	st := h.stats()
	if st.Subscribers != 3 {
		t.Errorf("subscribers = %d, want 3", st.Subscribers)
	}
	if st.PeakSubs != 3 {
		t.Errorf("peak = %d, want 3", st.PeakSubs)
	}
	if st.SubTotal != 3 {
		t.Errorf("cumulative = %d, want 3", st.SubTotal)
	}
	if st.IntervalSec != int(statsInterval/time.Second) {
		t.Errorf("interval = %d", st.IntervalSec)
	}

	r1()
	r2()
	r3()
	st = h.stats()
	if st.Subscribers != 0 {
		t.Errorf("subscribers = %d after release, want 0", st.Subscribers)
	}
	// Peak is a high-water mark — it must NOT fall back with the live count,
	// otherwise it answers a question nobody asked.
	if st.PeakSubs != 3 {
		t.Errorf("peak fell to %d; a high-water mark must persist", st.PeakSubs)
	}
	if st.Running {
		t.Error("running must be false once everyone left")
	}
}

// LastAgeSec drives a staleness readout, so it must start at -1 (never) and
// become a real age after a broadcast.
func TestStatsLastAge(t *testing.T) {
	h := newTestHub()
	if h.stats().LastAgeSec != -1 {
		t.Fatal("want -1 before any broadcast")
	}
	h.lastAtUnix.Store(time.Now().Unix() - 7)
	if got := h.stats().LastAgeSec; got < 6 || got > 8 {
		t.Errorf("age = %d, want ~7", got)
	}
}

// The live-mark overlay is where the websocket and the REST snapshot meet, so
// its failure modes are the ones that would show a wrong price on the strip.
func hubWithStats(rows []any) *tickerHub {
	h := newTestHub()
	h.statRows = rows
	return h
}

func row(symbol string, price float64, extra ...any) map[string]any {
	m := map[string]any{"symbol": symbol, "price": price, "changePct": 1.5}
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i].(string)] = extra[i+1]
	}
	return m
}

func TestEmitOverlaysLiveMarks(t *testing.T) {
	h := hubWithStats([]any{
		row("BTC", 78000),
		row("ETH", 2400),
		row("XAG", 65.05),
	})
	h.marks["BTC"] = 78940.5 // websocket is ahead of the 30s REST snapshot
	// ETH and XAG have no websocket price yet.

	ch := make(chan []byte, subBuffer)
	h.subs[ch] = struct{}{}
	h.emit()

	var got struct {
		Tickers []map[string]any `json:"tickers"`
	}
	if err := json.Unmarshal(<-ch, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Tickers) != 3 {
		t.Fatalf("rows = %d, want 3", len(got.Tickers))
	}
	by := map[string]map[string]any{}
	for _, r := range got.Tickers {
		by[r["symbol"].(string)] = r
	}
	if px := by["BTC"]["price"].(float64); px != 78940.5 {
		t.Errorf("BTC price = %v, want the websocket value 78940.5", px)
	}
	if by["BTC"]["live"] != true {
		t.Errorf("BTC should be flagged live, got %v", by["BTC"]["live"])
	}
	// The overlay must be NON-DESTRUCTIVE: a symbol the websocket has not
	// delivered yet keeps its REST price instead of blanking.
	if px := by["ETH"]["price"].(float64); px != 2400 {
		t.Errorf("ETH price = %v, want the REST value 2400 (no ws price yet)", px)
	}
	if _, flagged := by["ETH"]["live"]; flagged {
		t.Error("ETH must not be flagged live — no websocket price arrived")
	}
	// Fields REST owns must survive the overlay untouched.
	if by["BTC"]["changePct"].(float64) != 1.5 {
		t.Errorf("changePct lost in the overlay: %v", by["BTC"]["changePct"])
	}
}

// The overlay must not mutate statRows, or the next REST refresh would be
// diffed against websocket-contaminated values.
func TestEmitDoesNotMutateStatRows(t *testing.T) {
	orig := row("BTC", 78000)
	h := hubWithStats([]any{orig})
	h.marks["BTC"] = 78940.5
	ch := make(chan []byte, subBuffer)
	h.subs[ch] = struct{}{}
	h.emit()
	<-ch
	if orig["price"].(float64) != 78000 {
		t.Errorf("statRows row was mutated to %v — emit must copy", orig["price"])
	}
	if _, ok := orig["live"]; ok {
		t.Error("emit added a field to the source row")
	}
}

// A zero or negative websocket price must never reach the strip.
func TestEmitIgnoresNonPositiveMarks(t *testing.T) {
	h := hubWithStats([]any{row("BTC", 78000)})
	h.marks["BTC"] = 0
	ch := make(chan []byte, subBuffer)
	h.subs[ch] = struct{}{}
	h.emit()
	var got struct {
		Tickers []map[string]any `json:"tickers"`
	}
	if err := json.Unmarshal(<-ch, &got); err != nil {
		t.Fatal(err)
	}
	if px := got.Tickers[0]["price"].(float64); px != 78000 {
		t.Errorf("price = %v, want the REST value kept when the mark is 0", px)
	}
}

// With no REST snapshot yet there is nothing to overlay onto, and emitting a
// bare price list would drop every other column. Emit must stay silent.
func TestEmitSkipsBeforeFirstSnapshot(t *testing.T) {
	h := newTestHub()
	h.marks["BTC"] = 78940.5
	ch := make(chan []byte, subBuffer)
	h.subs[ch] = struct{}{}
	h.emit()
	select {
	case b := <-ch:
		t.Errorf("emitted before the first REST snapshot: %s", b)
	default:
	}
}

func TestTickerRowsShapes(t *testing.T) {
	// The source is injected, so tickerRows must not assume one map type.
	if got := tickerRows(gin.H{"tickers": []any{row("BTC", 1)}}); len(got) != 1 {
		t.Errorf("gin.H with []any: got %d rows", len(got))
	}
	if got := tickerRows(gin.H{"tickers": []gin.H{{"symbol": "BTC"}}}); len(got) != 1 {
		t.Errorf("gin.H with []gin.H (what buildTickers returns): got %d rows", len(got))
	}
	if got := tickerRows(map[string]any{"tickers": []any{row("BTC", 1)}}); len(got) != 1 {
		t.Errorf("plain map: got %d rows", len(got))
	}
	// Anything else must be reported as nil so refresh can count an error
	// rather than silently broadcasting an empty strip.
	for _, bad := range []any{nil, gin.H{}, gin.H{"tickers": "nope"}, "string", 42} {
		if got := tickerRows(bad); got != nil {
			t.Errorf("%#v should yield nil, got %#v", bad, got)
		}
	}
}

// The whole point of the upstream indicator is the DOWN state, which cannot be
// produced on a live box without breaking the feed for real. The state machine
// is tested directly instead.
func TestOnWSEventTransitions(t *testing.T) {
	h := newTestHub()
	st := h.stats()
	if st.WSUp || st.WSConnects != 0 || st.WSDrops != 0 {
		t.Fatalf("fresh hub: %+v", st)
	}
	if st.WSLastAgeS != -1 {
		t.Errorf("ws_last_age_sec = %d before any frame, want -1", st.WSLastAgeS)
	}

	h.onWSEvent(bingx.StreamEvent{Kind: "connecting", Symbols: 11, Attempt: 1})
	st = h.stats()
	if st.WSConnects != 1 {
		t.Errorf("connects = %d, want 1", st.WSConnects)
	}
	// Connecting is NOT up. A dial that succeeds and then delivers nothing must
	// not read as healthy — only a received frame proves the feed works.
	if st.WSUp {
		t.Error("ws_up must stay false until a frame actually arrives")
	}

	// Simulate the first frame (what markLoop does).
	h.wsUp.Store(true)
	h.wsLastMsg.Store(time.Now().Unix())
	if !h.stats().WSUp {
		t.Error("want up after a frame")
	}

	h.onWSEvent(bingx.StreamEvent{
		Kind: "dropped", Symbols: 11, Attempt: 1,
		Up: 42 * time.Second, Err: errTest, Retry: 700 * time.Millisecond,
	})
	st = h.stats()
	if st.WSUp {
		t.Error("a drop must clear ws_up")
	}
	if st.WSDrops != 1 {
		t.Errorf("drops = %d, want 1", st.WSDrops)
	}
	if st.WSLastErr != errTest.Error() {
		t.Errorf("ws_last_err = %q, want %q — the reason has to survive to /ops", st.WSLastErr, errTest)
	}

	// Reconnect: connects increments, and the previous error stays visible
	// (it is the LAST drop reason, not a live-state flag).
	h.onWSEvent(bingx.StreamEvent{Kind: "connecting", Symbols: 11, Attempt: 2})
	st = h.stats()
	if st.WSConnects != 2 || st.WSDrops != 1 {
		t.Errorf("connects/drops = %d/%d, want 2/1", st.WSConnects, st.WSDrops)
	}
	if st.WSLastErr == "" {
		t.Error("last drop reason should persist across a reconnect")
	}

	h.onWSEvent(bingx.StreamEvent{Kind: "stopped", Symbols: 11, Up: time.Minute})
	if h.stats().WSUp {
		t.Error("stopped must clear ws_up")
	}
}

// A drop with no error attached must still record something readable.
func TestOnWSEventDropWithoutError(t *testing.T) {
	h := newTestHub()
	h.onWSEvent(bingx.StreamEvent{Kind: "dropped", Attempt: 1})
	if got := h.stats().WSLastErr; got != "unknown" {
		t.Errorf("ws_last_err = %q, want %q", got, "unknown")
	}
}

// live/total drives the "即時標的" cell, which is what shows a PARTIAL feed —
// the failure where some symbols stream and others silently don't.
func TestStatsLiveSymbolCount(t *testing.T) {
	h := newTestHub()
	total := h.stats().WSTotalSyms
	if total == 0 {
		t.Fatal("hub resolved no symbols — the count would be meaningless")
	}
	if got := h.stats().WSLiveSyms; got != 0 {
		t.Errorf("live = %d before any frame, want 0", got)
	}
	h.marksMu.Lock()
	h.marks["BTC"] = 78900
	h.marks["ETH"] = 2430
	h.marks["XAG"] = 0 // a zero price must not count as live
	h.marksMu.Unlock()
	if got := h.stats().WSLiveSyms; got != 2 {
		t.Errorf("live = %d, want 2 (the zero-priced symbol must not count)", got)
	}
}
