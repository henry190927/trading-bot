// Package grpcsrv serves the MarketData gRPC service off the web process's
// existing ticker hub.
//
// The hub (cmd/web/stream.go) already owns the only upstream mark-price
// websocket, already conflates to one snapshot per 500ms, already drops the
// stale pending value for a slow subscriber, and already starts on the first
// subscriber and stops with the last. None of that is re-implemented here: a
// gRPC stream attaches to the hub exactly the way an SSE browser tab does, so
// the two transports share one upstream connection and cannot disagree about
// a price.
//
// That is why this package depends on nothing but an interface. The hub lives
// in package main (cmd/web) and cannot be imported; PriceSource is the seam
// that lets it be passed in, and lets the tests drive the service from a fake
// without an exchange, a port or a goroutine schedule to get right.
package grpcsrv

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/henry190927/trading-bot/proto/marketdata/v1"
)

const (
	// unaryWait caps GetMarkPrice when the CLIENT set no deadline. Subscribing
	// to an idle hub starts its loops, and the first snapshot only lands once
	// the REST fan-out returns — without a cap, a unary call against a cold
	// process would hang for as long as the client was willing to wait, which
	// for a generated client is forever.
	unaryWait = 10 * time.Second

	// shutdownGrace bounds GracefulStop. See Serve.
	shutdownGrace = 15 * time.Second

	// Keepalive: these streams are long-lived and mostly quiet between price
	// moves, so a peer that dies without a FIN (laptop lid, NAT timeout, VPN
	// drop) would otherwise hold a hub subscription until the OS gave up on
	// the socket. The server pings after 30s of inactivity and gives up 10s
	// later; EnforcementPolicy stops a misbehaving client from pinging more
	// often than every 10s, which is what makes the ping budget a server
	// decision rather than a client one.
	keepaliveTime      = 30 * time.Second
	keepaliveTimeout   = 10 * time.Second
	keepaliveMinClient = 10 * time.Second
)

// PriceSource is the ticker hub, seen from here.
//
// Subscribe returns the subscriber's channel, the newest snapshot already
// broadcast (nil if there is none yet) and a release func that MUST be called
// exactly once. Release is what decrements the hub's refcount, so a leaked
// release keeps the upstream websocket open forever. Every exit path in this
// file runs it through defer.
//
// The payload is the hub's marshalled snapshot, `{"tickers":[...]}`. It is
// JSON because the hub marshals ONCE and fans the same bytes out to every
// subscriber — making it hand out a typed value instead would move that
// marshal into each SSE client. Decoding here is the price of not regressing
// the existing hot path; at 11 symbols and 2 snapshots/sec it is noise.
type PriceSource interface {
	Subscribe() (<-chan []byte, []byte, func())
}

// Service implements pb.MarketDataServer.
type Service struct {
	pb.UnimplementedMarketDataServer

	src PriceSource
	now func() time.Time

	// done is closed by Drain. Streams select on it, so shutdown can end an
	// RPC that would otherwise never return on its own — see Serve.
	done      chan struct{}
	drainOnce sync.Once
}

// NewService wires the service to a price source.
func NewService(src PriceSource) *Service {
	return &Service{src: src, now: time.Now, done: make(chan struct{})}
}

// Drain tells every in-flight stream to finish.
//
// This exists because GracefulStop alone is NOT enough for a server-streaming
// RPC. GracefulStop stops accepting connections and then waits for in-flight
// RPCs to return — but StreamMarkPrices only returns when the CLIENT goes
// away, so a single attached client would block shutdown indefinitely. Drain
// is the application-level half: the handler returns UNAVAILABLE, which is
// the code a gRPC client retries on, and GracefulStop then completes.
func (s *Service) Drain() {
	s.drainOnce.Do(func() { close(s.done) })
}

// tickerRow is the subset of a hub snapshot row this service carries. The
// hub's rows also hold funding, 24h range and change %, which are not mark
// prices and are not this service's business.
type tickerRow struct {
	Symbol string  `json:"symbol"`
	Price  float64 `json:"price"`
	Live   bool    `json:"live"`
}

type hubSnapshot struct {
	Tickers []tickerRow `json:"tickers"`
}

func decodeSnapshot(b []byte) ([]tickerRow, error) {
	var snap hubSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, err
	}
	for i := range snap.Tickers {
		snap.Tickers[i].Symbol = strings.ToUpper(snap.Tickers[i].Symbol)
	}
	return snap.Tickers, nil
}

func normalize(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// GetMarkPrice — unary.
//
// It subscribes like any other client rather than reading some cached global:
// one code path means a unary read can never be served a value the stream
// would not have sent.
func (s *Service) GetMarkPrice(ctx context.Context, req *pb.GetMarkPriceRequest) (*pb.GetMarkPriceResponse, error) {
	sym := normalize(req.GetSymbol())
	if sym == "" {
		return nil, status.Error(codes.InvalidArgument, "symbol is required")
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, unaryWait)
		defer cancel()
	}

	ch, last, release := s.src.Subscribe()
	defer release()

	for last == nil {
		select {
		case <-ctx.Done():
			// Caller hung up or ran out of deadline. FromContextError maps
			// Canceled -> CANCELLED and DeadlineExceeded -> DEADLINE_EXCEEDED
			// rather than flattening both into UNKNOWN.
			return nil, status.FromContextError(ctx.Err()).Err()
		case <-s.done:
			return nil, status.Error(codes.Unavailable, "server is shutting down")
		case b, ok := <-ch:
			if !ok {
				return nil, status.Error(codes.Unavailable, "price feed closed")
			}
			last = b
		}
	}

	rows, err := decodeSnapshot(last)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "decode hub snapshot: %v", err)
	}
	for _, row := range rows {
		if row.Symbol != sym {
			continue
		}
		// The symbol is in the universe but the row carries no price: both the
		// mark fetch and the kline fallback failed upstream. That is a
		// different fact from "no such symbol" and gets a different code, so a
		// caller can tell a typo from an outage.
		if row.Price <= 0 {
			return nil, status.Errorf(codes.Unavailable, "%s: no price available yet", sym)
		}
		return &pb.GetMarkPriceResponse{Price: s.toProto(row)}, nil
	}
	return nil, status.Errorf(codes.NotFound, "unknown symbol %q", sym)
}

// StreamMarkPrices — server-streaming.
func (s *Service) StreamMarkPrices(req *pb.StreamMarkPricesRequest, stream grpc.ServerStreamingServer[pb.MarkPrice]) error {
	// Empty request = every symbol the hub carries. Explicit symbols are
	// filtered server-side so a client that wants BTC is not billed for the
	// other ten.
	want := make(map[string]bool, len(req.GetSymbols()))
	for _, raw := range req.GetSymbols() {
		if sym := normalize(raw); sym != "" {
			want[sym] = true
		}
	}

	ctx := stream.Context()
	ch, last, release := s.src.Subscribe()
	defer release()

	// Per-symbol last-sent price. The hub broadcasts a whole snapshot whenever
	// ANY symbol moved, so without this a BTC tick would also re-send ten
	// unchanged rows. Dropping unchanged values is safe for the same reason
	// the hub's drop-oldest is safe: this feed is last-value-wins.
	sent := make(map[string]float64, len(want))
	validated := false

	send := func(b []byte) error {
		rows, err := decodeSnapshot(b)
		if err != nil {
			return status.Errorf(codes.Internal, "decode hub snapshot: %v", err)
		}
		// Validate the requested symbols against the first snapshot actually
		// seen — not against `last`, which is nil on a cold hub. Doing it here
		// means a typo fails the same way whether or not the hub was warm.
		if !validated {
			validated = true
			if err := checkKnown(want, rows); err != nil {
				return err
			}
		}
		now := timestamppb.New(s.now())
		for _, row := range rows {
			if row.Price <= 0 {
				continue
			}
			if len(want) > 0 && !want[row.Symbol] {
				continue
			}
			if prev, ok := sent[row.Symbol]; ok && prev == row.Price {
				continue
			}
			sent[row.Symbol] = row.Price
			msg := s.toProto(row)
			msg.AsOf = now
			// Send blocks on HTTP/2 flow control when the client stops
			// reading. That is contained: this goroutine stalls, its hub
			// channel fills, and the hub discards the stale pending snapshot
			// for THIS subscriber only. A slow gRPC client cannot slow the
			// browser tabs down.
			if err := stream.Send(msg); err != nil {
				return err
			}
		}
		return nil
	}

	// Replay the newest snapshot so a client paints immediately instead of
	// waiting for the next price move — the same courtesy the SSE handler
	// does, and the reason subscribe() returns `last` at all.
	if last != nil {
		if err := send(last); err != nil {
			return err
		}
	}

	for {
		select {
		case <-ctx.Done():
			// The client disconnected, cancelled, or hit its deadline. gRPC
			// cancels the stream's context for all three, which is the ONLY
			// disconnect signal this handler gets — there is no "client gone"
			// callback. Returning runs the deferred release, and when this was
			// the hub's last subscriber the hub cancels its loops and closes
			// the upstream websocket.
			return status.FromContextError(ctx.Err()).Err()
		case <-s.done:
			return status.Error(codes.Unavailable, "server is shutting down")
		case b, ok := <-ch:
			if !ok {
				return status.Error(codes.Unavailable, "price feed closed")
			}
			if err := send(b); err != nil {
				return err
			}
		}
	}
}

// checkKnown fails a stream whose requested symbols are not in the universe,
// instead of leaving it open and silent forever.
func checkKnown(want map[string]bool, rows []tickerRow) error {
	if len(want) == 0 {
		return nil
	}
	have := make(map[string]bool, len(rows))
	for _, row := range rows {
		have[row.Symbol] = true
	}
	var missing []string
	for sym := range want {
		if !have[sym] {
			missing = append(missing, sym)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return status.Errorf(codes.NotFound, "unknown symbol(s): %s", strings.Join(missing, ", "))
}

func (s *Service) toProto(row tickerRow) *pb.MarkPrice {
	return &pb.MarkPrice{
		Symbol: row.Symbol,
		Price:  row.Price,
		Live:   row.Live,
		AsOf:   timestamppb.New(s.now()),
	}
}

// NewServer builds the grpc.Server with the service registered.
//
// Reflection is ON. This service is read-only public market data and binds
// wherever GRPC_BIND points (loopback or the tailnet, same as the web UI), so
// the schema is not a secret; having it on is what makes `grpcurl` work
// without shipping .proto files to the machine doing the debugging, which on
// a VPS at 2am is the whole difference. If this ever faced the open internet
// the call would flip, and the reason would be that a reflection endpoint
// hands an attacker the full method list for free.
func NewServer(svc *Service) *grpc.Server {
	gs := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    keepaliveTime,
			Timeout: keepaliveTimeout,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             keepaliveMinClient,
			PermitWithoutStream: true,
		}),
	)
	pb.RegisterMarketDataServer(gs, svc)
	reflection.Register(gs)
	return gs
}

// Serve runs the server on lis and blocks until ctx is cancelled or the
// listener fails.
//
// The LISTENER is the caller's, not ours. That is what lets cmd/web decide
// that a bind failure means "run without gRPC" instead of "take the web UI
// down with it", and it means the bufconn tests exercise this same path.
//
// Shutdown is deliberately two-phase:
//
//  1. Drain() — every open StreamMarkPrices returns UNAVAILABLE. Without this
//     step GracefulStop would wait forever, because a streaming RPC with a
//     healthy client never ends by itself.
//  2. GracefulStop() — finishes the unary calls already in flight, flushes the
//     GOAWAY, then returns. Stop() instead would cut every connection mid-frame
//     and show up at the client as an unexplained transport error. On a feed
//     whose clients are long-lived, that difference is the entire point.
//
// GracefulStop is still bounded: a client stuck in flow control can keep an
// in-flight Send from completing, and a shutdown that can hang is a shutdown
// systemd will SIGKILL anyway. After shutdownGrace, Stop() takes the hard path.
func Serve(ctx context.Context, lis net.Listener, src PriceSource, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	svc := NewService(src)
	gs := NewServer(svc)

	go func() {
		<-ctx.Done()
		logf("grpc: draining streams, then GracefulStop (grace %s)", shutdownGrace)
		svc.Drain()
		stopped := make(chan struct{})
		go func() {
			gs.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
			logf("grpc: stopped gracefully")
		case <-time.After(shutdownGrace):
			logf("grpc: grace period expired - forcing Stop()")
			gs.Stop()
		}
	}()

	logf("grpc: MarketData listening on %s (reflection on)", lis.Addr())
	return gs.Serve(lis)
}
