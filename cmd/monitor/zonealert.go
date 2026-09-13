package main

// Zone-alert channel — a fast (25s) live-price-vs-zone watcher that pings
// ntfy when price ENTERS a fade zone. Built for pivot-zone-fade entries so
// trading isn't screen-tethered.
//
// GENERAL / automatic by default (like the engine + confluence monitors):
// every ~2min it derives, for each symbol × TF, the pivot-zone-fade band
// from signal.AnalyzeStructure (shared package `zone`). Live price is then
// checked against those zones every 25s. Manual zones in zones.json are
// ALSO honoured. Fires on LIVE intrabar price so a 5m wick into the zone
// isn't missed by the closed-bar confluence scan. See project_ntfy_price_alerts.
//
// A zone may opt into CLOSE CONFIRMATION with `"confirm": "close-above"` (or
// close-below / close-in) — see zone.Zone.Confirm. Live-touch stays the
// default because it is correct for a fade band: you want the wick into the
// zone. It is wrong for a breakout gate, which is what added the mode —
// 2026-09-02 a BTC wick tagged 77413 inside the 77390–77476 cluster and the
// bar closed back at 77132, so the push announced a gate that never opened.

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/notify"
	"github.com/henry190927/trading-bot/zone"
)

const (
	zonePollEvery   = 25 * time.Second
	zoneStructEvery = 2 * time.Minute // structure only changes on bar close
	// closeGrace gives BingX time to publish a just-closed bar before the
	// cache treats itself as stale — same reasoning as the daemon's
	// --fetch-delay. A close-confirmed alert being 15s late is irrelevant;
	// hammering Klines at every bar boundary is not.
	closeGrace = 15 * time.Second
)

func dirWord(dir string) string {
	switch strings.ToUpper(dir) {
	case "SHORT":
		return "空"
	case "LONG":
		return "多"
	}
	return "觀察"
}

// closeCache holds the last CLOSED bar per (symbol, TF) for close-confirmed
// zones. bingx.Klines already drops the forming bar (dropForming), so the
// final element IS the last close — no trimming here, and none wanted:
// confirming on a forming bar is exactly the bug this cache serves to fix.
//
// The refetch window is derived from the bar itself (CloseTime-OpenTime), so
// one Klines call happens per bar close rather than one per 25s poll.
type closeCache struct {
	ctx    context.Context
	client *bingx.Client
	bars   map[string]market.Candle
}

func newCloseCache(ctx context.Context, client *bingx.Client) *closeCache {
	return &closeCache{ctx: ctx, client: client, bars: map[string]market.Candle{}}
}

func (c *closeCache) lastClosed(sym market.Symbol, tf market.Timeframe) (market.Candle, bool) {
	key := string(sym) + "|" + string(tf)
	if b, ok := c.bars[key]; ok {
		dur := b.CloseTime.Sub(b.OpenTime) + time.Millisecond
		// Still the newest close until the NEXT bar has closed and settled.
		if dur > 0 && time.Now().Before(b.CloseTime.Add(dur).Add(closeGrace)) {
			return b, true
		}
	}
	cs, err := c.client.Klines(c.ctx, sym, tf, 5)
	if err != nil || len(cs) == 0 {
		// Serve the stale bar rather than going silent on a transient error;
		// its window has expired so the next cycle retries.
		b, ok := c.bars[key]
		return b, ok
	}
	b := cs[len(cs)-1]
	c.bars[key] = b
	return b, true
}

// runZoneAlerts polls live mark price and pushes ntfy on outside→inside
// transitions, debounced per zone (an exit re-arms it). Close-confirmed zones
// use the same transition semantics against the last closed bar, so they fire
// once when the condition first holds and re-arm when it stops holding.
func runZoneAlerts(ctx context.Context, client *bingx.Client) {
	topic := os.Getenv("NTFY_TOPIC")
	if topic == "" {
		log.Printf("zonealert: NTFY_TOPIC unset — zone alerts disabled")
		return
	}
	n := notify.NewNtfy(os.Getenv("NTFY_SERVER"), topic)
	inside := map[string]bool{}
	warned := map[string]bool{} // misconfig log-once, so a bad zone can't spam every 25s
	cc := newCloseCache(ctx, client)
	var autoZones []zone.Zone
	var lastStruct time.Time
	var lastTFs string
	log.Printf("zonealert: up — poll %s, config=%s, manual=%s", zonePollEvery, zone.ConfigPath(), zone.Path())

	tick := time.NewTicker(zonePollEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		// Live config — re-read each cycle so /ops toggles apply without
		// a restart. Disabled → no fetches, no alerts (loop keeps running
		// so re-enabling is live too).
		cfg := zone.ReadConfig()
		if !cfg.Enabled {
			continue
		}
		if cfg.Auto {
			tfKey := cfg.TFs
			if time.Since(lastStruct) >= zoneStructEvery || tfKey != lastTFs {
				autoZones = zone.ComputeAuto(ctx, client, cfg.TFList())
				lastStruct = time.Now()
				lastTFs = tfKey
			}
		} else {
			autoZones = nil
		}

		zones := append(zone.LoadManual(), autoZones...)
		if len(zones) == 0 {
			continue
		}
		prices := map[string]float64{} // one live-price fetch per symbol per cycle
		livePx := func(short string, sym market.Symbol) (float64, bool) {
			if px, seen := prices[short]; seen {
				return px, px > 0
			}
			fr, err := client.FundingRate(ctx, sym)
			if err != nil || fr.MarkPrice <= 0 {
				prices[short] = 0
				return 0, false
			}
			prices[short] = fr.MarkPrice
			return fr.MarkPrice, true
		}

		for _, z := range zones {
			if z.Enabled != nil && !*z.Enabled {
				continue
			}
			short := strings.ToUpper(strings.TrimSpace(z.Symbol))
			key := zone.Key(z)

			// One gate for every reason a zone cannot be evaluated — unknown
			// symbol, inverted band, or a bad confirm mode. A misconfigured
			// zone is SKIPPED, never degraded to a touch alert: a band that
			// reads as close-confirmed while firing on wicks is the exact
			// defect this mode was added to remove. /ops calls the same
			// TriggerFault, so its armed list cannot disagree with the daemon.
			if fault := zone.TriggerFault(z); fault != "" {
				if !warned[key] {
					warned[key] = true
					log.Printf("zonealert: SKIP %s %s %s–%s — %s",
						short, z.TF, zone.FmtPrice(z.Lo), zone.FmtPrice(z.Hi), fault)
				}
				continue
			}
			sym := zone.ShortToSym[short] // TriggerFault guarantees this resolves

			var in bool
			var bar market.Candle

			if z.NeedsClose() {
				b, got := cc.lastClosed(sym, market.Timeframe(z.TF))
				if !got {
					continue
				}
				fired, _ := zone.EvalConfirm(z, b.Close)
				in, bar = fired, b
			} else {
				px, got := livePx(short, sym)
				if !got {
					continue
				}
				in = px >= z.Lo && px <= z.Hi
			}

			if in && !inside[key] {
				px, _ := livePx(short, sym) // for the push body; 0 if unavailable
				title, body, tags := zoneAlertText(z, short, px, bar)
				if err := n.Push(ctx, title, body, tags); err != nil {
					log.Printf("zonealert: push failed: %v", err)
				} else {
					log.Printf("zonealert: fired %s %s %s–%s dir=%s confirm=%q px=%s close=%s",
						short, z.TF, zone.FmtPrice(z.Lo), zone.FmtPrice(z.Hi),
						strings.ToUpper(z.Dir), z.Confirm, zone.FmtPrice(px), zone.FmtPrice(bar.Close))
				}
			}
			inside[key] = in
		}
	}
}

var taipei = time.FixedZone("Asia/Taipei", 8*3600)

// zoneAlertText builds the push. A close-confirmed alert must NOT read like a
// touch alert: it names the rule that fired it and quotes the closing bar, so
// the notification can be trusted without opening the chart. bar is the zero
// Candle for touch-mode zones.
func zoneAlertText(z zone.Zone, short string, px float64, bar market.Candle) (title, body, tags string) {
	dir := strings.ToUpper(z.Dir)
	tfTag := ""
	if z.TF != "" {
		tfTag = " " + z.TF
	}
	// ntfy can't colour arbitrary text — encode direction/zone as coloured
	// emoji: 🔴 short, 🟢 long, 🟡 the 樞紐區 band. The coloured-circle tag
	// also renders before the notification title.
	dirEmoji, tags := "🟡", "eyes"
	switch dir {
	case "SHORT":
		dirEmoji, tags = "🔴", "red_circle,chart_with_downwards_trend"
	case "LONG":
		dirEmoji, tags = "🟢", "green_circle,chart_with_upwards_trend"
	}
	pxStr := "n/a"
	if px > 0 {
		pxStr = zone.FmtPrice(px)
	}

	if z.NeedsClose() {
		// The confirming bar's own close time — not "now" — because that is
		// the fact being reported. A 25s poll can surface it up to a poll
		// late, and the reader needs to know which bar it was.
		when := bar.CloseTime.In(taipei).Format("01/02 15:04")
		mode := zone.ConfirmWord(z.Confirm)
		edge := zone.FmtPrice(z.Hi)
		if strings.EqualFold(strings.TrimSpace(z.Confirm), zone.ConfirmCloseBelow) {
			edge = zone.FmtPrice(z.Lo)
		}
		title = fmt.Sprintf("%s%s %s %s ✅收盤確認", short, tfTag, mode, edge)
		body = fmt.Sprintf("%s %s%s ｜ %s\n📊 %s 收 %s（帶 %s–%s）\n現價 %s\n%s\n→ 已收盤確認,非影線",
			dirEmoji, short, tfTag, mode,
			when, zone.FmtPrice(bar.Close), zone.FmtPrice(z.Lo), zone.FmtPrice(z.Hi),
			pxStr, z.Note)
		return title, body, tags
	}

	// Timestamp the push: the zone is a SNAPSHOT of the structure at send
	// time (directional-trend only). The live chart recomputes on each load
	// and also shows neutral-trend zones, so the two can differ — the stamp
	// makes clear this is a point-in-time value.
	snap := time.Now().In(taipei).Format("01/02 15:04")
	title = fmt.Sprintf("%s%s 進%s區 %s–%s", short, tfTag, dirWord(dir), zone.FmtPrice(z.Lo), zone.FmtPrice(z.Hi))
	body = fmt.Sprintf("%s %s%s ｜ 現價 %s\n🟡 樞紐區 %s–%s（%s %s）\n%s\n⏱ %s 快照（方向性結構,圖表即時值可能不同）\n→ 判斷 reject / 進場",
		dirEmoji, short, tfTag, pxStr, zone.FmtPrice(z.Lo), zone.FmtPrice(z.Hi), dirEmoji, dir, z.Note, snap)
	return title, body, tags
}
