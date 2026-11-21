package main

import (
	"context"
	"log"
	"net"
	"os"

	"github.com/henry190927/trading-bot/grpcsrv"
)

// The gRPC surface lives INSIDE the web process, not in a cmd/grpc of its own.
//
// The reason is the hub. tickerHub owns the single exchange mark-price
// websocket, and it owns it per PROCESS: a separate cmd/grpc binary could not
// attach to this one's hub, so it would have to build its own — a second
// websocket to BingX, a second REST poller, and two numbers that can disagree
// about the price of BTC. "Reuse the ticker hub" and "separate process" are
// mutually exclusive here, and reuse is the more valuable of the two.
//
// The cost of that choice is that gRPC shares the web process's fate. It is
// bounded by keeping the surface read-only and opt-in: GRPC_BIND unset means
// no listener and no behaviour change at all. That mirrors PPROF_BIND in
// main.go, which made the same call for the same reason.

// Subscribe adapts the hub to grpcsrv.PriceSource.
//
// A one-line wrapper rather than renaming subscribe -> Subscribe: a Go
// interface can only be satisfied by an EXPORTED method, and renaming would
// touch stream.go and its tests for no behavioural reason. Keeping the rename
// out of the diff keeps the SSE path provably untouched.
func (h *tickerHub) Subscribe() (<-chan []byte, []byte, func()) { return h.subscribe() }

// Compile-time proof that the real hub satisfies the seam — not just the fake
// the grpcsrv tests use.
var _ grpcsrv.PriceSource = (*tickerHub)(nil)

// startGRPC brings up the MarketData service when GRPC_BIND is set, e.g.
//
//	GRPC_BIND=127.0.0.1:9090 make web
//
// It returns immediately; the server runs in the background alongside Gin and
// shuts down when ctx is cancelled (main cancels it on SIGTERM / SIGINT).
// The returned channel is closed once gRPC has fully stopped — streams
// drained, GOAWAY flushed — so main can wait for it. It is already closed
// when gRPC is not configured or failed to start.
func startGRPC(ctx context.Context, hub *tickerHub) <-chan struct{} {
	done := make(chan struct{})
	bind := os.Getenv("GRPC_BIND")
	if bind == "" {
		close(done)
		return done
	}
	// ListenConfig rather than net.Listen: the repo's noctx linter is on, and
	// it is right that a listener should be cancellable. A port clash leaves
	// the web UI running — losing gRPC is not a reason to take it down.
	var lc net.ListenConfig
	lis, err := lc.Listen(ctx, "tcp", bind)
	if err != nil {
		log.Printf("grpc: NOT started — listen %s: %v (HTTP unaffected)", bind, err)
		close(done)
		return done
	}

	// Signal handling lives in main, not here. This used to install its own
	// SIGTERM handler and os.Exit(0) once gRPC stopped, which ended the
	// process before the HTTP side had drained anything. Now main owns the
	// shutdown order: HTTP and gRPC drain side by side, then the process
	// exits.
	go func() {
		defer close(done)
		serveErr := grpcsrv.Serve(ctx, lis, hub, log.Printf)
		if ctx.Err() == nil {
			// Serve died on its own (listener closed, fatal transport error),
			// not because of shutdown. Keep the web UI running without gRPC.
			log.Printf("grpc: stopped unexpectedly: %v — continuing without gRPC", serveErr)
			return
		}
		log.Printf("grpc: shutdown complete")
	}()
	return done
}
