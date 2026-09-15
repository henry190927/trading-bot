package bingx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/goleak"

	"github.com/henry190927/trading-bot/market"
)

// The rest of ws_test.go tests DecodeMarkFrame, BackoffFor and the URL — all
// pure functions. Nothing drove a connection, so SubscribeMarkPrices' read
// loop and its cancellation watchdog were the two goroutine-bearing paths in
// this package with no coverage at all, and `go test -race` never observed
// them. These do.

// wsServer starts a REAL WebSocket endpoint and points a Stream at it. handle
// runs server-side, once per connection. Returns the stop func separately
// rather than registering t.Cleanup so a caller can order it against a leak
// check — see the defer comment in the first test.
func wsServer(t *testing.T, handle func(*websocket.Conn)) (*Stream, func()) {
	t.Helper()
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		handle(c)
	}))
	return &Stream{URL: "ws" + strings.TrimPrefix(srv.URL, "http")}, srv.Close
}

// drain reads until the peer goes away. The server side of a test that only
// cares about the client's behaviour.
func drain(c *websocket.Conn) {
	for {
		if _, _, err := c.ReadMessage(); err != nil {
			return
		}
	}
}

// Cancellation has no way to interrupt a blocking ReadMessage, so the function
// spawns a watchdog that closes the socket on ctx.Done(). Two things have to
// hold: the read actually unblocks, and the watchdog does not outlive the
// call. The second matters because RunMarkPrices reconnects in a loop — a
// watchdog that survived its connection would leak one goroutine per reconnect
// and the process would accumulate them for as long as the daemon runs.
func TestSubscribeMarkPricesCancelUnblocksAndLeaksNothing(t *testing.T) {
	// defer is LIFO, and the ORDER is the point: stop() must run before the
	// leak check, or the still-open httptest connection is itself counted as a
	// leak and the assertion proves nothing.
	defer goleak.VerifyNone(t)

	subscribed := make(chan struct{})
	s, stop := wsServer(t, func(c *websocket.Conn) {
		_, _, _ = c.ReadMessage() // the subscribe frame
		close(subscribed)
		drain(c)
	})
	defer stop()

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- s.SubscribeMarkPrices(ctx, []market.Symbol{market.BTCUSDT}, make(chan MarkPrice, 1))
	}()
	<-subscribed // in the read loop now, not still dialling
	cancel()

	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled — a cancelled read must "+
				"report the cancellation, not the socket error it caused", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel did not unblock ReadMessage: the watchdog never closed the socket")
	}
}

// The send to `out` is a select/default, so a consumer that stops draining
// costs frames but must never stall the reader. The Pong is the proof: it can
// only be written by the same loop that would have been blocked.
//
// Note this drops the NEWEST frame, the opposite of the SSE hub's
// drop-OLDEST policy (see cmd/web/stream.go). Both are deliberate and they
// differ: a mark price is worth nothing once a newer one exists, so the hub
// keeps the freshest, while here the cheap non-blocking send is what protects
// the socket's read cadence.
func TestSubscribeMarkPricesNeverBlocksOnAFullChannel(t *testing.T) {
	defer goleak.VerifyNone(t)

	const flood = 200
	ponged := make(chan struct{})
	s, stop := wsServer(t, func(c *websocket.Conn) {
		_, _, _ = c.ReadMessage() // subscribe
		for i := 0; i < flood; i++ {
			if err := c.WriteMessage(websocket.TextMessage, []byte(realMarkFrame)); err != nil {
				return
			}
		}
		if err := c.WriteMessage(websocket.TextMessage, []byte("Ping")); err != nil {
			return
		}
		for {
			_, raw, err := c.ReadMessage()
			if err != nil {
				return
			}
			if strings.TrimSpace(string(raw)) == "Pong" {
				close(ponged)
				return
			}
		}
	})
	defer stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	// Capacity 1 and nothing ever receives: every frame after the first is
	// dropped on the floor.
	go func() {
		errc <- s.SubscribeMarkPrices(ctx, []market.Symbol{market.BTCUSDT}, make(chan MarkPrice, 1))
	}()

	select {
	case <-ponged:
	case <-time.After(10 * time.Second):
		t.Fatalf("no Pong after %d undrained frames — the send to `out` blocked the read loop", flood)
	}
	cancel()
	<-errc // let the call return before the leak check
}

// One bad frame must not take the socket down: every other symbol on the same
// connection would go dark with it.
func TestSubscribeMarkPricesSurvivesAMalformedFrame(t *testing.T) {
	defer goleak.VerifyNone(t)

	s, stop := wsServer(t, func(c *websocket.Conn) {
		_, _, _ = c.ReadMessage()
		_ = c.WriteMessage(websocket.TextMessage, []byte("{not json"))
		_ = c.WriteMessage(websocket.TextMessage, []byte(realMarkFrame))
		drain(c)
	})
	defer stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan MarkPrice, 4)
	errc := make(chan error, 1)
	go func() { errc <- s.SubscribeMarkPrices(ctx, []market.Symbol{market.BTCUSDT}, out) }()

	select {
	case mp := <-out:
		if mp.Symbol != market.BTCUSDT || mp.Price != 78716.3 {
			t.Errorf("got %+v, want BTC-USDT @ 78716.3", mp)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the frame after a malformed one never arrived — the bad frame killed the connection")
	}
	cancel()
	<-errc
}

// The watchdog's c.Close() and the loop's c.ReadMessage() are the one piece of
// state those two goroutines share. Cycling the whole connect-read-cancel
// sequence rapidly is what gives -race an interleaving to actually observe;
// a single cancellation almost always lands with the read already parked.
func TestSubscribeMarkPricesCancelRacesTheReadLoop(t *testing.T) {
	defer goleak.VerifyNone(t)

	s, stop := wsServer(t, func(c *websocket.Conn) {
		_, _, _ = c.ReadMessage()
		// Keep writing so the read loop is genuinely busy rather than parked,
		// which is the interleaving a single cancel would miss.
		for {
			if err := c.WriteMessage(websocket.TextMessage, []byte(realMarkFrame)); err != nil {
				return
			}
		}
	})
	defer stop()

	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		out := make(chan MarkPrice, 1)
		go func() { errc <- s.SubscribeMarkPrices(ctx, []market.Symbol{market.BTCUSDT}, out) }()
		cancel()
		select {
		case <-errc:
		case <-time.After(10 * time.Second):
			t.Fatalf("iteration %d: call never returned after cancel", i)
		}
	}
}

// The cancellation test above does NOT cover the watchdog's other exit. When
// ctx is cancelled the watchdog leaves through its own `case <-ctx.Done()`,
// so `defer close(done)` is dead weight on that path — deleting the defer and
// re-running it still passes, which is how this gap was found.
//
// The leak lives on the OTHER exit: the socket dies while ctx is still alive.
// SubscribeMarkPrices returns a read error, and nothing ever closes ctx.Done()
// — so without `close(done)` the watchdog parks on a select whose two cases
// can never fire, holding the conn alive with it. RunMarkPrices reconnects on
// exactly this error, once per drop, for the life of the daemon.
func TestSubscribeMarkPricesReleasesWatchdogWhenTheSocketDies(t *testing.T) {
	defer goleak.VerifyNone(t)

	s, stop := wsServer(t, func(c *websocket.Conn) {
		_, _, _ = c.ReadMessage() // subscribe, then hang up on the client
	})
	defer stop()

	// NOT cancelled, and not deferred-cancel either: a live ctx for the whole
	// test is the condition under which the watchdog can only be released by
	// `done`.
	ctx := context.Background()
	errc := make(chan error, 1)
	go func() {
		errc <- s.SubscribeMarkPrices(ctx, []market.Symbol{market.BTCUSDT}, make(chan MarkPrice, 1))
	}()

	select {
	case err := <-errc:
		if err == nil || errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want the read error from the dropped socket", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the call never returned after the server hung up")
	}
	// goleak (deferred above) is the assertion: the watchdog must be gone.
}
