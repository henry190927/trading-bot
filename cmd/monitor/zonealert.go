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

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"myFirstGo/trading-bot/bingx"
	"myFirstGo/trading-bot/notify"
	"myFirstGo/trading-bot/zone"
)

const (
	zonePollEvery   = 25 * time.Second
	zoneStructEvery = 2 * time.Minute // structure only changes on bar close
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

// runZoneAlerts polls live mark price and pushes ntfy on outside→inside
// transitions, debounced per zone (an exit re-arms it).
func runZoneAlerts(ctx context.Context, client *bingx.Client) {
	topic := os.Getenv("NTFY_TOPIC")
	if topic == "" {
		log.Printf("zonealert: NTFY_TOPIC unset — zone alerts disabled")
		return
	}
	n := notify.NewNtfy(os.Getenv("NTFY_SERVER"), topic)
	inside := map[string]bool{}
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
		for _, z := range zones {
			if z.Enabled != nil && !*z.Enabled {
				continue
			}
			short := strings.ToUpper(z.Symbol)
			sym, ok := zone.ShortToSym[short]
			if !ok || z.Hi <= z.Lo {
				continue
			}
			px, seen := prices[short]
			if !seen {
				fr, err := client.FundingRate(ctx, sym)
				if err != nil || fr.MarkPrice <= 0 {
					continue
				}
				px = fr.MarkPrice
				prices[short] = px
			}

			key := zone.Key(z)
			in := px >= z.Lo && px <= z.Hi
			if in && !inside[key] {
				dir := strings.ToUpper(z.Dir)
				tfTag := ""
				if z.TF != "" {
					tfTag = " " + z.TF
				}
				// ntfy can't colour arbitrary text — encode direction/zone as
				// coloured emoji: 🔴 short, 🟢 long, 🟡 the 樞紐區 band. The
				// coloured-circle tag also renders before the notification title.
				dirEmoji, tags := "🟡", "eyes"
				switch dir {
				case "SHORT":
					dirEmoji, tags = "🔴", "red_circle,chart_with_downwards_trend"
				case "LONG":
					dirEmoji, tags = "🟢", "green_circle,chart_with_upwards_trend"
				}
				// Timestamp the push: the zone is a SNAPSHOT of the structure at
				// send time (directional-trend only). The live chart recomputes on
				// each load and also shows neutral-trend zones, so the two can differ
				// — the stamp makes clear this is a point-in-time value.
				snap := time.Now().In(time.FixedZone("Asia/Taipei", 8*3600)).Format("01/02 15:04")
				title := fmt.Sprintf("%s%s 進%s區 %s–%s", short, tfTag, dirWord(dir), zone.FmtPrice(z.Lo), zone.FmtPrice(z.Hi))
				body := fmt.Sprintf("%s %s%s ｜ 現價 %s\n🟡 樞紐區 %s–%s（%s %s）\n%s\n⏱ %s 快照（方向性結構,圖表即時值可能不同）\n→ 判斷 reject / 進場",
					dirEmoji, short, tfTag, zone.FmtPrice(px), zone.FmtPrice(z.Lo), zone.FmtPrice(z.Hi), dirEmoji, dir, z.Note, snap)
				if err := n.Push(ctx, title, body, tags); err != nil {
					log.Printf("zonealert: push failed: %v", err)
				} else {
					log.Printf("zonealert: fired %s%s px=%s zone=%s-%s dir=%s", short, tfTag, zone.FmtPrice(px), zone.FmtPrice(z.Lo), zone.FmtPrice(z.Hi), dir)
				}
			}
			inside[key] = in
		}
	}
}
