// wsprobe — dump raw BingX swap-market websocket frames for one channel.
//
// bingx/ws.go carried three not-implemented stubs plus two protocol notes
// (gzip-compressed frames; a TEXT "Ping" that must be answered with "Pong",
// not a protocol ping). Those notes are worth trusting but not worth guessing
// past: the field names and the exact shape of a markPrice/ticker push decide
// how the hub's source is written, and reading them off the wire beats reading
// them off documentation. Same reason cmd/acct prints raw position payloads.
package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/henry190927/trading-bot/bingx"
)

func main() {
	channel := flag.String("channel", "BTC-USDT@markPrice", "comma-separated BingX dataTypes; multiple tests whether ONE connection can carry N subscriptions (decides 1 vs 11 sockets)")
	seconds := flag.Int("seconds", 20, "how long to listen")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*seconds)*time.Second)
	defer cancel()

	c, resp, err := websocket.DefaultDialer.DialContext(ctx, bingx.HostWSSwap, nil)
	if err != nil {
		status := ""
		if resp != nil {
			status = fmt.Sprintf(" (http %s)", resp.Status)
		}
		fmt.Fprintf(os.Stderr, "dial %s: %v%s\n", bingx.HostWSSwap, err, status)
		os.Exit(1)
	}
	defer func() { _ = c.Close() }()
	fmt.Printf("connected %s\n", bingx.HostWSSwap)

	for i, ch := range strings.Split(*channel, ",") {
		ch = strings.TrimSpace(ch)
		if ch == "" {
			continue
		}
		sub := map[string]any{"id": fmt.Sprintf("probe-%d", i+1), "reqType": "sub", "dataType": ch}
		if err := c.WriteJSON(sub); err != nil {
			fmt.Fprintf(os.Stderr, "subscribe %s: %v\n", ch, err)
			os.Exit(1)
		}
		fmt.Printf("sent sub %s\n", ch)
	}
	fmt.Println()

	n := 0
	perType := map[string]int{}
	report := func() {
		fmt.Printf("\ndone — %d frame(s)\n", n)
		for k, v := range perType {
			fmt.Printf("  %-28s %d frames\n", k, v)
		}
	}
	for {
		if ctx.Err() != nil {
			report()
			return
		}
		_ = c.SetReadDeadline(time.Now().Add(time.Duration(*seconds) * time.Second))
		mt, data, err := c.ReadMessage()
		if err != nil {
			fmt.Printf("\nread ended: %v\n", err)
			report()
			return
		}
		body := data
		// Frames may be gzipped; detect by magic rather than assuming, so a
		// plaintext control frame still prints.
		if len(data) > 2 && data[0] == 0x1f && data[1] == 0x8b {
			zr, zerr := gzip.NewReader(bytesReader(data))
			if zerr == nil {
				if out, rerr := io.ReadAll(zr); rerr == nil {
					body = out
				}
				zr.Close()
			}
		}
		s := string(body)
		n++
		kind := "binary"
		if mt == websocket.TextMessage {
			kind = "text"
		}
		fmt.Printf("[%02d %s %dB] %s\n", n, kind, len(data), truncate(s, 220))
		var probe struct {
			DataType string `json:"dataType"`
		}
		if json.Unmarshal(body, &probe) == nil && probe.DataType != "" {
			perType[probe.DataType]++
		}

		// The documented keepalive: a TEXT "Ping" must be answered "Pong".
		if s == "Ping" {
			if err := c.WriteMessage(websocket.TextMessage, []byte("Pong")); err != nil {
				fmt.Fprintf(os.Stderr, "pong: %v\n", err)
				return
			}
			fmt.Println("     -> replied Pong")
			continue
		}
		// Pretty-print the first data push so the field names are legible.
		if n <= 2 {
			var m map[string]any
			if json.Unmarshal(body, &m) == nil {
				b, _ := json.MarshalIndent(m, "     ", "  ")
				fmt.Printf("     %s\n", b)
			}
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

type br struct {
	b []byte
	i int
}

func bytesReader(b []byte) io.Reader { return &br{b: b} }
func (r *br) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}
