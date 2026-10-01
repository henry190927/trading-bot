package main

import (
	"context"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

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
// no listener, no signal handler and no behaviour change at all. That mirrors
// PPROF_BIND in main.go, which made the same call for the same reason.

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
// It returns immediately; the server runs in the background alongside Gin.
func startGRPC(hub *tickerHub) {
	bind := os.Getenv("GRPC_BIND")
	if bind == "" {
		return
	}
	// Bind BEFORE installing the signal handler. A port clash must leave the
	// process exactly as it was — HTTP serving, SIGTERM still fatal by
	// default — rather than half-wired into a shutdown path that no longer
	// has a server to shut down.
	// ListenConfig rather than net.Listen: the repo's noctx linter is on, and
	// it is right that a listener should be cancellable. There is nothing to
	// cancel at this point in startup, so the context is Background and the
	// shutdown story is the one in Serve.
	var lc net.ListenConfig
	lis, err := lc.Listen(context.Background(), "tcp", bind)
	if err != nil {
		log.Printf("grpc: NOT started — listen %s: %v (HTTP unaffected)", bind, err)
		return
	}

	// Taking over SIGTERM is the part worth being careful about. Until now
	// this process had no handler, so SIGTERM killed it instantly; systemctl
	// restart trading-web was immediate. NotifyContext suppresses that
	// default, which means every path out of here MUST end in the process
	// actually exiting, or a restart would stall until systemd's stop timeout
	// ran out and SIGKILLed it.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)

	go func() {
		serveErr := grpcsrv.Serve(ctx, lis, hub, log.Printf)
		if ctx.Err() == nil {
			// Serve died on its own (listener closed, fatal transport error),
			// not because of a signal. Hand SIGTERM back to the default
			// handler and leave the web UI running — losing gRPC is not a
			// reason to take the dashboard down.
			log.Printf("grpc: stopped unexpectedly: %v — continuing without gRPC", serveErr)
			stop()
			return
		}
		// Shutdown finished: streams drained, GOAWAY flushed. The HTTP side is
		// NOT drained here, deliberately — see docs/grpc.md. It dies with the
		// process, which is exactly what SIGTERM already did to it.
		log.Printf("grpc: shutdown complete — exiting")
		os.Exit(0)
	}()
}
