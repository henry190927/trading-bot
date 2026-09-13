package bingx

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/henry190927/trading-bot/market"
)

// Stream is the BingX swap-market WebSocket consumer.
//
// Protocol facts, all confirmed against the live endpoint with cmd/wsprobe
// rather than taken from documentation:
//
//   - Payloads arrive as BINARY frames containing gzip. A 145-byte frame
//     decompresses to ~120 bytes of JSON, so the compression barely pays for
//     itself at this size — but it is not optional.
//   - Keepalive is an application-level TEXT "Ping" that must be answered with
//     "Pong". It is NOT a protocol ping/pong, so gorilla's automatic handler
//     never sees it; miss this and the server drops the connection.
//   - ONE connection carries N subscriptions. Verified with 4 dataTypes on a
//     single socket, each delivering ~1 frame/sec independently. This is why
//     the hub needs one upstream connection rather than one per symbol.
//   - markPrice pushes at roughly 1 Hz per symbol — this is a 1-second feed,
//     NOT a per-trade tick stream. Worth stating plainly: it lowers the
//     freshness floor from the REST poller's 3s to ~1s, and no further.
//   - The US-stock synthetics (NCSKSNDK2USD-USDT etc.) work on the same
//     stream as the crypto pairs.
type Stream struct {
	URL string
}

func NewStream() *Stream {
	return &Stream{URL: HostWSSwap}
}

// MarkPrice is one markPriceUpdate push.
type MarkPrice struct {
	Symbol market.Symbol
	Price  float64
	At     time.Time
}

// wsEnvelope is the outer frame shape:
//
//	{"code":0,"dataType":"BTC-USDT@markPrice",
//	 "data":{"e":"markPriceUpdate","E":1788443796475,"s":"BTC-USDT","p":"78716.3"}}
//
// Subscription acks arrive as the same envelope with an empty dataType and a
// null data, which is why data is a RawMessage rather than a struct.
type wsEnvelope struct {
	ID       string          `json:"id"`
	Code     int             `json:"code"`
	Msg      string          `json:"msg"`
	DataType string          `json:"dataType"`
	Data     json.RawMessage `json:"data"`
}

type markPriceData struct {
	Event  string `json:"e"`
	EventT int64  `json:"E"`
	Symbol string `json:"s"`
	Price  string `json:"p"` // stringly-typed, like every other BingX numeric
}

// inflate returns the frame body, transparently un-gzipping when the magic
// bytes say to. Detected rather than assumed so a plaintext control frame
// still decodes if BingX ever stops compressing.
func inflate(raw []byte) ([]byte, error) {
	if len(raw) < 2 || raw[0] != 0x1f || raw[1] != 0x8b {
		return raw, nil
	}
	zr, err := gzip.NewReader(strings.NewReader(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("gzip read: %w", err)
	}
	return out, nil
}

// DecodeMarkFrame classifies one raw frame. Pure, so the protocol handling is
// testable without a socket.
//
// Returns isPing for the keepalive, a non-nil MarkPrice for a price push, and
// (false, nil, nil) for anything else — subscription acks and channels we did
// not ask for are normal traffic, not errors.
func DecodeMarkFrame(raw []byte) (isPing bool, mp *MarkPrice, err error) {
	body, err := inflate(raw)
	if err != nil {
		return false, nil, err
	}
	switch s := strings.TrimSpace(string(body)); {
	case s == "Ping":
		return true, nil, nil
	case s == "":
		// An empty frame carries nothing. Per this function's contract that
		// makes it ignorable traffic, not an error — errors are reserved for
		// frames that SHOULD have meant something and didn't.
		return false, nil, nil
	}
	var env wsEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return false, nil, fmt.Errorf("decode envelope: %w (body=%.120s)", err, body)
	}
	if env.Code != 0 {
		return false, nil, fmt.Errorf("ws error code %d: %s", env.Code, env.Msg)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return false, nil, nil // subscription ack
	}
	var d markPriceData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return false, nil, nil // some other channel's shape; not our problem
	}
	if d.Event != "markPriceUpdate" || d.Symbol == "" {
		return false, nil, nil
	}
	px, perr := strconv.ParseFloat(d.Price, 64)
	if perr != nil || px <= 0 {
		return false, nil, fmt.Errorf("bad price %q for %s", d.Price, d.Symbol)
	}
	return false, &MarkPrice{
		Symbol: market.Symbol(d.Symbol),
		Price:  px,
		At:     time.UnixMilli(d.EventT),
	}, nil
}

// readTimeout must exceed the server's Ping interval, or a quiet-but-healthy
// connection gets torn down. Observed pings land every ~5s; 45s is generous
// while still detecting a half-open socket well before a user would notice.
const readTimeout = 45 * time.Second

// SubscribeMarkPrices opens ONE connection for every symbol and pushes updates
// to out until ctx is cancelled or the connection fails. It does not retry —
// RunMarkPrices supervises that, so this function stays a single, readable
// pass over the protocol.
//
// Sends to out are non-blocking: a full channel drops the update. Mark prices
// are last-value-wins, so a superseded price carries no information — the same
// reasoning that licenses the SSE hub's drop-oldest policy. Do not copy this
// to a fills or orders stream.
func (s *Stream) SubscribeMarkPrices(ctx context.Context, syms []market.Symbol, out chan<- MarkPrice) error {
	if len(syms) == 0 {
		return errors.New("no symbols")
	}
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c, resp, err := websocket.DefaultDialer.DialContext(dialCtx, s.URL, nil)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("dial %s: %w (http %s)", s.URL, err, resp.Status)
		}
		return fmt.Errorf("dial %s: %w", s.URL, err)
	}
	defer c.Close()

	for i, sym := range syms {
		req := map[string]any{
			"id":       fmt.Sprintf("mark-%d", i+1),
			"reqType":  "sub",
			"dataType": string(sym) + "@markPrice",
		}
		if err := c.WriteJSON(req); err != nil {
			return fmt.Errorf("subscribe %s: %w", sym, err)
		}
	}

	// Closing the socket is what unblocks ReadMessage on cancellation; there
	// is no way to interrupt a blocking read otherwise.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = c.Close()
		case <-done:
		}
	}()

	for {
		if err := c.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
			return err
		}
		_, raw, err := c.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read: %w", err)
		}
		isPing, mp, derr := DecodeMarkFrame(raw)
		if derr != nil {
			// A single malformed frame is not worth dropping the connection
			// for; every other symbol on this socket would go dark with it.
			continue
		}
		if isPing {
			if err := c.WriteMessage(websocket.TextMessage, []byte("Pong")); err != nil {
				return fmt.Errorf("pong: %w", err)
			}
			continue
		}
		if mp == nil {
			continue
		}
		select {
		case out <- *mp:
		default:
		}
	}
}

// StreamEvent reports connection lifecycle to the caller.
//
// A struct rather than a formatted string: the first version handed back
// pre-worded log lines, which meant a caller that wanted to SURFACE health
// (an /ops indicator, a counter) had to parse English back into fields. The
// caller owns the wording; this owns the facts.
type StreamEvent struct {
	Kind    string        // "connecting" | "dropped" | "stopped"
	Symbols int           // how many channels the connection carries
	Attempt int           // 1-based, resets after a long-lived connection
	Up      time.Duration // how long the dropped connection lasted
	Err     error         // why it dropped
	Retry   time.Duration // how long until the next attempt
}

// BackoffFor computes the reconnect wait for a given attempt: exponential,
// halved, plus up to that much jitter again — so the result lands in
// [delay/2, delay). Exported and pure so the schedule is testable and so a
// caller can display the ceiling.
//
// Jitter is not decoration: without it every process (and every
// browser-driven reconnect behind it) retries in lockstep and stampedes a
// server that is already struggling.
func BackoffFor(attempt int) time.Duration {
	const (
		baseDelay = 1 * time.Second
		maxDelay  = 30 * time.Second
	)
	if attempt < 0 {
		attempt = 0
	}
	delay := maxDelay
	if attempt < 16 {
		if d := baseDelay << attempt; d > 0 && d < maxDelay {
			delay = d
		}
	}
	half := delay / 2
	return half + time.Duration(rand.Int63n(int64(half)))
}

// RunMarkPrices keeps the subscription alive across drops until ctx is done.
//
// onEvent, when non-nil, is called for every connect / drop / stop. A feed
// that silently reconnects is a quieter way of going blind, so the caller is
// given every transition rather than only the fatal ones.
func (s *Stream) RunMarkPrices(ctx context.Context, syms []market.Symbol, out chan<- MarkPrice, onEvent func(StreamEvent)) {
	emit := func(e StreamEvent) {
		if onEvent != nil {
			e.Symbols = len(syms)
			onEvent(e)
		}
	}
	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		emit(StreamEvent{Kind: "connecting", Attempt: attempt + 1})
		err := s.SubscribeMarkPrices(ctx, syms, out)
		if ctx.Err() != nil {
			emit(StreamEvent{Kind: "stopped", Attempt: attempt + 1, Up: time.Since(start)})
			return
		}
		up := time.Since(start)
		// A connection that stayed up counts as healthy: reset the backoff so
		// one bad night does not leave the retry delay pinned at the ceiling.
		if up > time.Minute {
			attempt = 0
		}
		wait := BackoffFor(attempt)
		emit(StreamEvent{Kind: "dropped", Attempt: attempt + 1, Up: up, Err: err, Retry: wait})
		attempt++
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// SubscribeKlines pushes closed candles. Channel name: "<sym>@kline_<tf>".
func (s *Stream) SubscribeKlines(ctx context.Context, sym market.Symbol, tf market.Timeframe, out chan<- market.Candle) error {
	return errors.New("bingx.Stream.SubscribeKlines: not implemented")
}

// SubscribeTrades streams individual trade prints for CVD. Channel: "<sym>@trade".
func (s *Stream) SubscribeTrades(ctx context.Context, sym market.Symbol, out chan<- market.Trade) error {
	return errors.New("bingx.Stream.SubscribeTrades: not implemented")
}

// SubscribeDepth streams L2 snapshots/diffs for liquidity-zone detection.
func (s *Stream) SubscribeDepth(ctx context.Context, sym market.Symbol, out chan<- market.Depth) error {
	return errors.New("bingx.Stream.SubscribeDepth: not implemented")
}
