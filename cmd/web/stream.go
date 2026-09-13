package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/market"
)

// Server-owned price fan-out (design B) over Server-Sent Events.
//
// Before this, every open tab polled /api/tickers on its own timer, so the
// latency floor WAS the poll interval and the browser did the same work N
// times. The 3s server cache kept upstream load flat, but nothing could make
// the table move sooner than the next tick.
//
// Now one goroutine owns the upstream refresh and pushes each snapshot to
// every subscriber. Upstream cost is independent of how many tabs are open,
// and a tab learns about a price change as soon as the server does.
//
// SSE rather than WebSocket, deliberately: this stream is one-directional,
// last-value-wins, and loss-tolerant. Those three properties are exactly the
// ones that make SSE the smaller tool — plain HTTP, no upgrade handshake, and
// the browser reconnects on its own. The WebSocket design (and when it WOULD
// be the right call) is written up in docs/price_stream_design.md.
//
// Upstream is now SPLIT, which is the whole point of having kept the seam:
//
//   - bingx websocket @markPrice carries the PRICE, ~1 Hz per symbol over ONE
//     connection for all of them (markLoop).
//   - REST buildTickers carries everything that moves slowly — the 24h-ago
//     reference for change %, funding, the 24h range — every 30s (statsLoop).
//   - emit() overlays the live marks onto the last REST rows and broadcasts at
//     most every broadcastInterval (broadcastLoop).
//
// Subscribers, backpressure and the SSE transport were untouched by that
// change, exactly as the seam predicted. The overlay is non-destructive, so a
// websocket drop degrades to 30s-fresh REST prices rather than freezing or
// blanking the strip.
//
// Freshness floor is now ~1s (the exchange's markPrice cadence), not 3s. It is
// NOT a per-trade tick feed — see the note on bingx.Stream.

const (
	// statsInterval is how often the REST snapshot is rebuilt while at least
	// one subscriber is attached.
	//
	// It used to be 3s because REST was the ONLY source, which made it the
	// freshness floor. Now the websocket carries the price, and everything
	// REST still provides — the 24h-ago reference for change %, funding rate,
	// the symbol list — moves on the order of minutes. Polling that every 3s
	// was 10x of upstream load buying nothing.
	statsInterval = 30 * time.Second
	// broadcastInterval coalesces websocket updates. 11 symbols at ~1 Hz is
	// ~11 pushes/sec; forwarding each one would send 11 SSE frames a second to
	// every tab to move one number. 500ms is still 6x fresher than the old 3s
	// poll and keeps the tick-flash effect looking continuous.
	broadcastInterval = 500 * time.Millisecond
	// streamHeartbeat keeps proxies and load balancers from treating a quiet
	// stream as dead. An SSE comment line is ignored by EventSource.
	streamHeartbeat = 20 * time.Second
	// subBuffer is per-subscriber. It is 1 on purpose: see broadcast.
	subBuffer = 1
)

type tickerHub struct {
	// source produces one snapshot. Injected rather than reaching into
	// *server for two reasons: the hub becomes testable without an exchange,
	// and this is the precise seam an exchange websocket would replace —
	// asserting that in a comment is cheap, making it a parameter is not.
	source func(context.Context) any

	// syms is the upstream subscription list, and shorts maps an exchange
	// symbol back to the short name the snapshot is keyed by.
	syms   []market.Symbol
	shorts map[market.Symbol]string

	mu      sync.Mutex
	subs    map[chan []byte]struct{}
	last    []byte             // most recent snapshot, replayed to a new subscriber
	cancel  context.CancelFunc // stops the loops when the last subscriber leaves
	running bool

	// marks is the live price per short symbol, straight off the websocket,
	// overlaid onto the REST snapshot at broadcast time. Kept separate from
	// the snapshot so a websocket drop degrades to REST-fresh prices instead
	// of freezing or blanking them.
	marksMu  sync.Mutex
	marks    map[string]float64
	statRows []any // last REST rows, awaiting the live-mark overlay
	dirty    atomic.Bool

	// Counters, atomic so /ops can read them without contending on mu.
	//
	// dropped is the one that matters: broadcast discards a stale pending
	// snapshot silently, which is correct but was invisible — there was no way
	// to distinguish a healthy stream from one where every client is starved.
	// drops/sec IS the backpressure signal.
	nDelivered  atomic.Int64
	nDropped    atomic.Int64
	nRefresh    atomic.Int64
	nRefreshErr atomic.Int64
	nSubTotal   atomic.Int64 // cumulative subscribers since boot
	peakSubs    atomic.Int64
	lastAtUnix  atomic.Int64 // unix seconds of the last successful broadcast

	// reportedDrops is the drop count as of the last log line, so the
	// periodic report can print a DELTA rather than a meaningless running
	// total.
	reportedDrops atomic.Int64

	// Upstream (exchange websocket) health. Reconnects were previously only
	// logged, so a flapping upstream was visible in the journal and nowhere
	// else — the /ops card reported the SSE fan-out as healthy the whole time,
	// because it WAS. These separate "we are serving clients" from "we are
	// receiving prices", which are different failures.
	wsUp       atomic.Bool
	wsConnects atomic.Int64
	wsDrops    atomic.Int64
	wsLastMsg  atomic.Int64 // unix seconds of the last mark frame
	wsLastErr  atomic.Value // string
}

// hubStats is the /ops view of the fan-out.
type hubStats struct {
	Running     bool    `json:"running"`
	Subscribers int     `json:"subscribers"`
	PeakSubs    int64   `json:"peak_subs"`
	SubTotal    int64   `json:"sub_total"`
	Delivered   int64   `json:"delivered"`
	Dropped     int64   `json:"dropped"`
	DropPct     float64 `json:"drop_pct"`
	Refreshes   int64   `json:"refreshes"`
	RefreshErrs int64   `json:"refresh_errs"`
	IntervalSec int     `json:"interval_sec"`
	LastAgeSec  int64   `json:"last_age_sec"` // -1 when nothing has been broadcast yet

	// Upstream websocket, reported separately from the fan-out above: the SSE
	// side can be perfectly healthy while no prices are arriving at all.
	WSUp        bool   `json:"ws_up"`
	WSConnects  int64  `json:"ws_connects"`
	WSDrops     int64  `json:"ws_drops"`
	WSLastAgeS  int64  `json:"ws_last_age_sec"` // -1 = no frame yet
	WSLastErr   string `json:"ws_last_err,omitempty"`
	WSLiveSyms  int    `json:"ws_live_syms"`
	WSTotalSyms int    `json:"ws_total_syms"`
}

func (h *tickerHub) stats() hubStats {
	h.mu.Lock()
	n, running := len(h.subs), h.running
	h.mu.Unlock()

	del, drp := h.nDelivered.Load(), h.nDropped.Load()
	age := int64(-1)
	if t := h.lastAtUnix.Load(); t > 0 {
		age = time.Now().Unix() - t
	}
	pct := 0.0
	if tot := del + drp; tot > 0 {
		pct = float64(drp) / float64(tot) * 100
	}
	wsAge := int64(-1)
	if t := h.wsLastMsg.Load(); t > 0 {
		wsAge = time.Now().Unix() - t
	}
	lastErr, _ := h.wsLastErr.Load().(string)
	h.marksMu.Lock()
	liveSyms := 0
	for _, px := range h.marks {
		if px > 0 {
			liveSyms++
		}
	}
	h.marksMu.Unlock()

	return hubStats{
		Running: running, Subscribers: n,
		PeakSubs: h.peakSubs.Load(), SubTotal: h.nSubTotal.Load(),
		Delivered: del, Dropped: drp, DropPct: pct,
		Refreshes: h.nRefresh.Load(), RefreshErrs: h.nRefreshErr.Load(),
		IntervalSec: int(statsInterval / time.Second),
		LastAgeSec:  age,

		WSUp: h.wsUp.Load(), WSConnects: h.wsConnects.Load(), WSDrops: h.wsDrops.Load(),
		WSLastAgeS: wsAge, WSLastErr: lastErr,
		WSLiveSyms: liveSyms, WSTotalSyms: len(h.syms),
	}
}

func newTickerHub(source func(context.Context) any) *tickerHub {
	h := &tickerHub{
		source: source,
		subs:   map[chan []byte]struct{}{},
		marks:  map[string]float64{},
		shorts: map[market.Symbol]string{},
	}
	// Subscribe to exactly the symbols the strip shows, resolved through the
	// same helper the REST path uses so the two cannot disagree about what a
	// short name means.
	for _, short := range tickerSymbols {
		if sym, err := resolveWebSymbol(short); err == nil {
			h.syms = append(h.syms, sym)
			h.shorts[sym] = short
		}
	}
	return h
}

// subscribe attaches a client and returns its channel plus a release func.
//
// The poller starts on the FIRST subscriber and stops with the last, so an
// idle deployment makes no exchange calls at all — the old design polled
// forever from whichever browser happened to be open.
func (h *tickerHub) subscribe() (<-chan []byte, []byte, func()) {
	ch := make(chan []byte, subBuffer)

	h.mu.Lock()
	h.subs[ch] = struct{}{}
	last := h.last
	h.nSubTotal.Add(1)
	if n := int64(len(h.subs)); n > h.peakSubs.Load() {
		h.peakSubs.Store(n)
	}
	if !h.running {
		ctx, cancel := context.WithCancel(context.Background())
		h.cancel, h.running = cancel, true
		go h.statsLoop(ctx)
		go h.markLoop(ctx)
		go h.broadcastLoop(ctx)
	}
	h.mu.Unlock()

	return ch, last, func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		if len(h.subs) == 0 && h.running {
			h.cancel()
			h.running = false
		}
		h.mu.Unlock()
	}
}

func (h *tickerHub) statsLoop(ctx context.Context) {
	log.Printf("stream: stats loop started (REST every %s)", statsInterval)
	defer log.Printf("stream: stats loop stopped (no subscribers)")

	tick := time.NewTicker(statsInterval)
	defer tick.Stop()
	h.refresh(ctx) // don't make the first subscriber wait a full interval
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			h.refresh(ctx)
		}
	}
}

// markLoop feeds live prices from the exchange websocket. One connection for
// every symbol (see bingx.Stream) — supervised, so a drop reconnects with
// jittered backoff and says so rather than going quietly stale.
func (h *tickerHub) markLoop(ctx context.Context) {
	if len(h.syms) == 0 {
		log.Printf("stream: no resolvable symbols — markprice ws not started")
		return
	}
	ch := make(chan bingx.MarkPrice, 256)
	go func() {
		st := bingx.NewStream()
		st.RunMarkPrices(ctx, h.syms, ch, h.onWSEvent)
	}()
	// A connection can be established and then deliver nothing (wrong channel
	// name, silent server). wsUp alone would read healthy, so staleness is
	// tracked separately and the /ops card flags it.
	defer h.wsUp.Store(false)
	for {
		select {
		case <-ctx.Done():
			return
		case mp := <-ch:
			// The first frame is the only proof the connection actually works;
			// a successful dial is not.
			h.wsUp.Store(true)
			short, ok := h.shorts[mp.Symbol]
			if !ok {
				continue
			}
			h.marksMu.Lock()
			changed := h.marks[short] != mp.Price
			h.marks[short] = mp.Price
			h.marksMu.Unlock()
			// Only a CHANGED price is worth a broadcast. BingX republishes the
			// same mark when nothing traded, and forwarding those would make
			// every tab repaint (and the tick-flash logic re-evaluate) for no
			// information.
			h.wsLastMsg.Store(time.Now().Unix())
			if changed {
				h.dirty.Store(true)
			}
		}
	}
}

// onWSEvent records upstream health and writes the log line. The wording
// lives here rather than in package bingx so /ops gets structured fields and
// the journal gets a sentence, from the same event.
func (h *tickerHub) onWSEvent(e bingx.StreamEvent) {
	switch e.Kind {
	case "connecting":
		h.wsConnects.Add(1)
		log.Printf("stream: markprice ws connecting (%d symbols, attempt %d)", e.Symbols, e.Attempt)
	case "dropped":
		h.wsUp.Store(false)
		h.wsDrops.Add(1)
		msg := "unknown"
		if e.Err != nil {
			msg = e.Err.Error()
		}
		h.wsLastErr.Store(msg)
		log.Printf("stream: markprice ws DROPPED after %s: %s — retry in %s (attempt %d)",
			e.Up.Round(time.Second), msg, e.Retry.Round(time.Millisecond), e.Attempt)
	case "stopped":
		h.wsUp.Store(false)
		log.Printf("stream: markprice ws stopped after %s (no subscribers)", e.Up.Round(time.Second))
	}
}

// broadcastLoop emits at most one snapshot per broadcastInterval, and only
// when something actually changed.
func (h *tickerHub) broadcastLoop(ctx context.Context) {
	tick := time.NewTicker(broadcastInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if h.dirty.CompareAndSwap(true, false) {
				h.emit()
			}
		}
	}
}

func (h *tickerHub) refresh(ctx context.Context) {
	if h.source == nil {
		return
	}
	fctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	snap := h.source(fctx)
	h.nRefresh.Add(1)
	rows := tickerRows(snap)
	if rows == nil {
		h.nRefreshErr.Add(1)
		log.Printf("stream: snapshot had no tickers array")
		return
	}
	h.marksMu.Lock()
	h.statRows = rows
	h.marksMu.Unlock()
	// A fresh REST snapshot changes funding / 24h range even if no price
	// moved, so it always earns a broadcast.
	h.dirty.Store(true)
}

// tickerRows pulls the rows out of whatever shape the source returned. The
// source is injected, so this must not assume gin.H specifically.
func tickerRows(snap any) []any {
	switch m := snap.(type) {
	case gin.H:
		if v, ok := m["tickers"].([]gin.H); ok {
			out := make([]any, len(v))
			for i := range v {
				out[i] = v[i]
			}
			return out
		}
		if v, ok := m["tickers"].([]any); ok {
			return v
		}
	case map[string]any:
		if v, ok := m["tickers"].([]any); ok {
			return v
		}
	}
	return nil
}

// emit overlays the live marks onto the last REST rows, marshals ONCE, and
// fans out.
//
// The overlay is deliberately non-destructive: a symbol with no websocket
// price yet keeps its REST price, so a partial feed degrades per-symbol
// instead of blanking the strip.
func (h *tickerHub) emit() {
	h.marksMu.Lock()
	rows := make([]any, 0, len(h.statRows))
	for _, r := range h.statRows {
		switch row := r.(type) {
		case gin.H:
			cp := make(gin.H, len(row)+1)
			for k, v := range row {
				cp[k] = v
			}
			if short, ok := row["symbol"].(string); ok {
				if px, ok := h.marks[short]; ok && px > 0 {
					cp["price"] = px
					cp["live"] = true
				}
			}
			rows = append(rows, cp)
		case map[string]any:
			cp := make(map[string]any, len(row)+1)
			for k, v := range row {
				cp[k] = v
			}
			if short, ok := row["symbol"].(string); ok {
				if px, ok := h.marks[short]; ok && px > 0 {
					cp["price"] = px
					cp["live"] = true
				}
			}
			rows = append(rows, cp)
		default:
			rows = append(rows, r)
		}
	}
	h.marksMu.Unlock()

	if len(rows) == 0 {
		return
	}
	b, err := json.Marshal(gin.H{"tickers": rows})
	if err != nil {
		h.nRefreshErr.Add(1)
		log.Printf("stream: marshal snapshot: %v", err)
		return
	}
	h.broadcast(b)
	h.lastAtUnix.Store(time.Now().Unix())
	h.reportDrops()
}

// reportDrops logs only when drops have occurred SINCE THE LAST REPORT, so a
// healthy stream stays silent and a starved one is visible in the journal
// without a per-drop log line (which at the broadcast cadence x N subscribers
// would be spam).
func (h *tickerHub) reportDrops() {
	total := h.nDropped.Load()
	prev := h.reportedDrops.Load()
	if total == prev {
		return
	}
	h.reportedDrops.Store(total)
	h.mu.Lock()
	n := len(h.subs)
	h.mu.Unlock()
	log.Printf("stream: %d snapshot(s) dropped for slow subscribers since last report (total %d, %d subscriber(s)) — clients are not keeping up with the %s broadcast interval",
		total-prev, total, n, broadcastInterval)
}

// broadcast pushes to every subscriber without ever blocking on one.
//
// A stalled tab (backgrounded, throttled, dead TCP window) must not hold up
// the hub or anyone else. Each subscriber has a 1-slot buffer and a slow one
// gets its PENDING snapshot replaced by the newer one — drop-oldest, not
// drop-newest. That is only safe because this stream is last-value-wins: a
// superseded price carries no information. The same policy on an order or
// trade feed would be a bug; the semantics are what license it.
func (h *tickerHub) broadcast(b []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last = b
	for ch := range h.subs {
		select {
		case ch <- b:
			h.nDelivered.Add(1)
		default:
			// Buffer full: discard the stale pending value, then enqueue.
			select {
			case <-ch:
				h.nDropped.Add(1)
			default:
			}
			select {
			case ch <- b:
				h.nDelivered.Add(1)
			default:
			}
		}
	}
}

// handleStreamTickers — GET /api/stream/tickers (text/event-stream).
func (s *server) handleStreamTickers(c *gin.Context) {
	ch, last, release := s.hub.subscribe()
	defer release()

	h := c.Writer.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	// Defeats nginx-style response buffering, which would otherwise hold
	// events until the buffer filled and make the stream look broken.
	h.Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)
	c.Writer.Flush()

	// Replay the newest snapshot immediately so a fresh tab paints at once
	// instead of waiting for the next interval.
	if last != nil {
		if !writeSSE(c.Writer, "tickers", last) {
			return
		}
	}

	hb := time.NewTicker(streamHeartbeat)
	defer hb.Stop()
	ctx := c.Request.Context()
	for {
		select {
		case <-ctx.Done(): // client navigated away / closed the tab
			return
		case b, ok := <-ch:
			if !ok {
				return
			}
			if !writeSSE(c.Writer, "tickers", b) {
				return
			}
		case <-hb.C:
			if _, err := io.WriteString(c.Writer, ": ping\n\n"); err != nil {
				return
			}
			c.Writer.Flush()
		}
	}
}

// writeSSE emits one event frame and flushes it. Returns false once the
// connection is gone, which is the signal to unwind and release the sub.
func writeSSE(w gin.ResponseWriter, event string, data []byte) bool {
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return false
	}
	w.Flush()
	return true
}
