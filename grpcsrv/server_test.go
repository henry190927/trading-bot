package grpcsrv

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/henry190927/trading-bot/proto/marketdata/v1"
)

// The generated interface is the contract; if a signature drifts this fails at
// compile time rather than at the first call.
var _ pb.MarketDataServer = (*Service)(nil)

// fakeHub is a stand-in for cmd/web's tickerHub with the same three contract
// points that matter here: a 1-slot drop-oldest buffer, a `last` snapshot
// replayed to a new subscriber, and a release func that must be called once.
// Nothing in the tests touches an exchange, a clock or a real port.
type fakeHub struct {
	mu       sync.Mutex
	subs     map[chan []byte]struct{}
	last     []byte
	nSubs    int
	nRelease int
}

func newFakeHub() *fakeHub {
	return &fakeHub{subs: map[chan []byte]struct{}{}}
}

func (f *fakeHub) Subscribe() (<-chan []byte, []byte, func()) {
	ch := make(chan []byte, 1)
	f.mu.Lock()
	f.subs[ch] = struct{}{}
	f.nSubs++
	last := f.last
	f.mu.Unlock()

	var once sync.Once
	return ch, last, func() {
		once.Do(func() {
			f.mu.Lock()
			if _, ok := f.subs[ch]; ok {
				delete(f.subs, ch)
				close(ch)
			}
			f.nRelease++
			f.mu.Unlock()
		})
	}
}

// push fans a snapshot out with the hub's drop-oldest policy.
func (f *fakeHub) push(b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last = b
	for ch := range f.subs {
		select {
		case ch <- b:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- b:
			default:
			}
		}
	}
}

func (f *fakeHub) counts() (subs, releases int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nSubs, f.nRelease
}

// snapshot builds a hub-shaped payload: snapshot("BTC", 100, "ETH", 2000).
func snapshot(pairs ...any) []byte {
	rows := make([]map[string]any, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		rows = append(rows, map[string]any{"symbol": pairs[i], "price": pairs[i+1], "live": true})
	}
	b, err := json.Marshal(map[string]any{"tickers": rows})
	if err != nil {
		panic(err)
	}
	return b
}

// newTestClient brings up the real grpc.Server over bufconn — an in-memory
// listener, so the test exercises the actual codec, stream and status plumbing
// without binding a port or racing another test for one.
func newTestClient(t *testing.T, hub PriceSource) (pb.MarketDataClient, *Service) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	svc := NewService(hub)
	svc.now = func() time.Time { return time.Unix(1700000000, 0).UTC() }
	gs := NewServer(svc)
	go func() { _ = gs.Serve(lis) }()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() {
		conn.Close()
		gs.Stop()
		lis.Close()
	})
	return pb.NewMarketDataClient(conn), svc
}

func codeOf(err error) codes.Code { return status.Code(err) }

// ── unary ────────────────────────────────────────────────────────────

func TestGetMarkPriceServesTheHubSnapshot(t *testing.T) {
	hub := newFakeHub()
	hub.push(snapshot("BTC", 64250.5, "ETH", 3120.25))
	cli, _ := newTestClient(t, hub)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Lowercase on purpose: symbols are normalized server-side.
	resp, err := cli.GetMarkPrice(ctx, &pb.GetMarkPriceRequest{Symbol: "btc"})
	if err != nil {
		t.Fatalf("GetMarkPrice: %v", err)
	}
	if got := resp.GetPrice().GetSymbol(); got != "BTC" {
		t.Errorf("symbol = %q, want BTC", got)
	}
	if got := resp.GetPrice().GetPrice(); got != 64250.5 {
		t.Errorf("price = %v, want 64250.5", got)
	}
	if !resp.GetPrice().GetLive() {
		t.Error("live = false, want true (fixture says the ws carried it)")
	}
	if got := resp.GetPrice().GetAsOf().AsTime(); !got.Equal(time.Unix(1700000000, 0).UTC()) {
		t.Errorf("as_of = %v, want the injected clock", got)
	}

	// The unary path must not leak a hub subscription.
	if subs, rel := hub.counts(); subs != rel {
		t.Errorf("subscribe/release = %d/%d, want them equal", subs, rel)
	}
}

func TestGetMarkPriceWaitsForTheFirstSnapshot(t *testing.T) {
	hub := newFakeHub() // cold: no `last` yet, exactly like a hub with no subscribers
	cli, _ := newTestClient(t, hub)

	go func() {
		time.Sleep(50 * time.Millisecond)
		hub.push(snapshot("XAG", 31.42))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := cli.GetMarkPrice(ctx, &pb.GetMarkPriceRequest{Symbol: "XAG"})
	if err != nil {
		t.Fatalf("GetMarkPrice on a cold hub: %v", err)
	}
	if resp.GetPrice().GetPrice() != 31.42 {
		t.Errorf("price = %v, want 31.42", resp.GetPrice().GetPrice())
	}
}

func TestGetMarkPriceErrorCodes(t *testing.T) {
	hub := newFakeHub()
	hub.push([]byte(`{"tickers":[{"symbol":"BTC","price":1},{"symbol":"APP"}]}`))
	cli, _ := newTestClient(t, hub)

	cases := []struct {
		name   string
		symbol string
		want   codes.Code
	}{
		{"empty symbol", "", codes.InvalidArgument},
		{"not in the universe", "DOGE", codes.NotFound},
		// A row with no price field: the symbol is real, the upstream read
		// failed. Distinct from NotFound on purpose.
		{"known but priceless", "APP", codes.Unavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := cli.GetMarkPrice(ctx, &pb.GetMarkPriceRequest{Symbol: tc.symbol})
			if got := codeOf(err); got != tc.want {
				t.Fatalf("code = %v (err %v), want %v", got, err, tc.want)
			}
		})
	}
}

func TestGetMarkPriceDeadlineOnASilentHub(t *testing.T) {
	cli, _ := newTestClient(t, newFakeHub()) // nothing is ever pushed

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := cli.GetMarkPrice(ctx, &pb.GetMarkPriceRequest{Symbol: "BTC"})
	if got := codeOf(err); got != codes.DeadlineExceeded {
		t.Fatalf("code = %v (err %v), want DeadlineExceeded", got, err)
	}
}

// ── server streaming ─────────────────────────────────────────────────

func TestStreamMarkPricesFiltersAndDeduplicates(t *testing.T) {
	hub := newFakeHub()
	hub.push(snapshot("BTC", 100, "ETH", 2000))
	cli, _ := newTestClient(t, hub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := cli.StreamMarkPrices(ctx, &pb.StreamMarkPricesRequest{Symbols: []string{"btc"}})
	if err != nil {
		t.Fatalf("StreamMarkPrices: %v", err)
	}

	// 1st: the replayed `last`, so the client paints without waiting.
	first, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv replay: %v", err)
	}
	if first.GetSymbol() != "BTC" || first.GetPrice() != 100 {
		t.Fatalf("replay = %s/%v, want BTC/100", first.GetSymbol(), first.GetPrice())
	}

	// ETH moved, BTC did not: nothing is owed to this client. Then BTC moves.
	hub.push(snapshot("BTC", 100, "ETH", 2222))
	hub.push(snapshot("BTC", 101, "ETH", 2222))

	second, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv after move: %v", err)
	}
	if second.GetSymbol() != "BTC" || second.GetPrice() != 101 {
		t.Fatalf("second = %s/%v, want BTC/101 — an ETH-only move must not be sent, "+
			"and an unchanged BTC must not be re-sent", second.GetSymbol(), second.GetPrice())
	}
}

func TestStreamMarkPricesEmptyRequestMeansEverySymbol(t *testing.T) {
	hub := newFakeHub()
	hub.push(snapshot("BTC", 100, "ETH", 2000, "XAU", 2400))
	cli, _ := newTestClient(t, hub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := cli.StreamMarkPrices(ctx, &pb.StreamMarkPricesRequest{})
	if err != nil {
		t.Fatalf("StreamMarkPrices: %v", err)
	}
	seen := map[string]float64{}
	for i := 0; i < 3; i++ {
		msg, err := stream.Recv()
		if err != nil {
			t.Fatalf("recv %d: %v", i, err)
		}
		seen[msg.GetSymbol()] = msg.GetPrice()
	}
	if len(seen) != 3 || seen["BTC"] != 100 || seen["ETH"] != 2000 || seen["XAU"] != 2400 {
		t.Fatalf("got %v, want all three symbols from one empty request", seen)
	}
}

func TestStreamMarkPricesRejectsAnUnknownSymbol(t *testing.T) {
	hub := newFakeHub()
	hub.push(snapshot("BTC", 100))
	cli, _ := newTestClient(t, hub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := cli.StreamMarkPrices(ctx, &pb.StreamMarkPricesRequest{Symbols: []string{"NOPE"}})
	if err != nil {
		t.Fatalf("StreamMarkPrices: %v", err)
	}
	// A typo must fail loudly. The alternative — an open stream that is
	// silent forever — is indistinguishable from a dead feed.
	if _, err := stream.Recv(); codeOf(err) != codes.NotFound {
		t.Fatalf("code = %v (err %v), want NotFound", codeOf(err), err)
	}
}

// A disconnecting client must give its hub subscription back, or the hub's
// refcount never reaches zero and the upstream websocket stays open forever.
func TestStreamReleasesTheHubWhenTheClientGoesAway(t *testing.T) {
	hub := newFakeHub()
	hub.push(snapshot("BTC", 100))
	cli, _ := newTestClient(t, hub)

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := cli.StreamMarkPrices(ctx, &pb.StreamMarkPricesRequest{Symbols: []string{"BTC"}})
	if err != nil {
		t.Fatalf("StreamMarkPrices: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("recv replay: %v", err)
	}
	cancel() // what a closed client connection looks like to the server

	deadline := time.Now().Add(5 * time.Second)
	for {
		subs, rel := hub.counts()
		if subs == rel && subs > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("subscribe/release = %d/%d after client cancel — the stream leaked a subscription", subs, rel)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Drain is the half of shutdown GracefulStop cannot do on its own: a healthy
// streaming client never ends the RPC, so without this the server would wait
// forever for an RPC that is working exactly as designed.
func TestDrainEndsAnOpenStream(t *testing.T) {
	hub := newFakeHub()
	hub.push(snapshot("BTC", 100))
	cli, svc := newTestClient(t, hub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := cli.StreamMarkPrices(ctx, &pb.StreamMarkPricesRequest{Symbols: []string{"BTC"}})
	if err != nil {
		t.Fatalf("StreamMarkPrices: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("recv replay: %v", err)
	}

	svc.Drain()
	_, err = stream.Recv()
	if errors.Is(err, io.EOF) {
		t.Fatal("stream ended with EOF — a drained server should say UNAVAILABLE so the client retries")
	}
	if got := codeOf(err); got != codes.Unavailable {
		t.Fatalf("code = %v (err %v), want Unavailable", got, err)
	}
}

// Drain must be safe to call more than once: SIGTERM then SIGINT, or a
// shutdown path that runs twice, must not close a closed channel.
func TestDrainIsIdempotent(t *testing.T) {
	svc := NewService(newFakeHub())
	svc.Drain()
	svc.Drain()
}
