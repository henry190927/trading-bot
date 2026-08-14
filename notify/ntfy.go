package notify

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"myFirstGo/trading-bot/signal"
)

// Ntfy pushes alerts via ntfy.sh — free, no account, topic-based.
//
// Setup:
//  1. Install ntfy iOS / Android app (or any client).
//  2. Pick a hard-to-guess topic name (e.g. a UUID). Anyone with the topic
//     can read your alerts, so don't use "trading-bot" or your username.
//  3. Subscribe to that topic in the app.
//  4. Set NTFY_TOPIC in trading-bot/.env to the same value.
//
// Optional NTFY_SERVER overrides the default https://ntfy.sh (use if you
// self-host your own ntfy server).
type Ntfy struct {
	Server string
	Topic  string
	HTTP   *http.Client
}

func NewNtfy(server, topic string) *Ntfy {
	if server == "" {
		server = "https://ntfy.sh"
	}
	return &Ntfy{
		Server: strings.TrimRight(server, "/"),
		Topic:  topic,
		HTTP:   &http.Client{Timeout: 10 * time.Second},
	}
}

// Push sends a plain alert with a custom title/body/tags — for alerts that
// don't map to a signal.Signal (e.g. price-zone touches). Same topic/server
// and high priority as Notify.
func (n *Ntfy) Push(ctx context.Context, title, body, tags string) error {
	if n.Topic == "" {
		return nil
	}
	url := n.Server + "/" + n.Topic
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("ntfy push: build req: %w", err)
	}
	req.Header.Set("Title", title)
	req.Header.Set("Priority", "4")
	if tags != "" {
		req.Header.Set("Tags", tags)
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp, err := n.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy push: do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ntfy push: http %d: %s", resp.StatusCode, raw)
	}
	return nil
}

func (n *Ntfy) Notify(ctx context.Context, sig signal.Signal, ctxInfo signal.Context) error {
	if n.Topic == "" {
		return nil
	}
	body := formatAlert(sig, ctxInfo)
	url := n.Server + "/" + n.Topic

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("ntfy: build req: %w", err)
	}

	// Rich headers: title, priority (4 = high → bypasses iOS Focus modes),
	// tags for emoji decoration, and the click action drops you into the
	// raw stdout log if the user has it open.
	title := fmt.Sprintf("%s %s score=%d", sig.Symbol, sig.Side, sig.Score)
	req.Header.Set("Title", title)
	req.Header.Set("Priority", "4")
	tag := "chart_with_upwards_trend"
	if sig.Side == signal.Short {
		tag = "chart_with_downwards_trend"
	}
	req.Header.Set("Tags", tag+",money_with_wings")
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")

	resp, err := n.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy: do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ntfy: http %d: %s", resp.StatusCode, raw)
	}
	return nil
}
