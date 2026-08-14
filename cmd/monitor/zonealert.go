package main

// Zone-alert channel — a fast (25s) live-price-vs-zone watcher that pings
// ntfy when price ENTERS a user-defined band. Built for pivot-zone-fade
// entries (and any level touch) so trading isn't screen-tethered.
//
// Deliberately separate from the confluence scan: that fires on CLOSED
// bars (engine signals, canonical). This fires on LIVE intrabar price, so
// a 5m wick into the zone doesn't get missed. Zones live in a JSON file
// re-read every cycle — edit it live, no restart. See the design note in
// project_ntfy_price_alerts. v1 = manual zones; v2 (later) auto-derives
// them from signal.AnalyzeStructure (confirmed trend's pivot zone).

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"myFirstGo/trading-bot/bingx"
	"myFirstGo/trading-bot/market"
	"myFirstGo/trading-bot/notify"
)

const (
	zoneDefaultPath = "/opt/trading/zones.json"
	zonePollEvery   = 25 * time.Second
)

// Zone is one price band to watch. Edit zones.json live; the watcher
// re-reads it each cycle.
type Zone struct {
	Symbol  string  `json:"symbol"` // BTC / ETH / XAU / XAG
	Lo      float64 `json:"lo"`
	Hi      float64 `json:"hi"`
	Dir     string  `json:"dir"`  // short / long / watch — colours the alert
	Note    string  `json:"note"` // free text shown in the push
	Enabled *bool   `json:"enabled,omitempty"`
}

var zoneShortToSym = map[string]market.Symbol{
	"BTC": market.BTCUSDT, "ETH": market.ETHUSDT,
	"XAU": market.XAUUSDT, "XAG": market.XAGUSDT,
}

func zonesPath() string {
	if p := os.Getenv("ZONES_PATH"); strings.TrimSpace(p) != "" {
		return p
	}
	return zoneDefaultPath
}

func loadZones() []Zone {
	b, err := os.ReadFile(zonesPath())
	if err != nil {
		return nil // no file = no zones (not an error)
	}
	var zs []Zone
	if err := json.Unmarshal(b, &zs); err != nil {
		log.Printf("zonealert: bad zones.json: %v", err)
		return nil
	}
	return zs
}

func zoneKey(z Zone) string {
	return fmt.Sprintf("%s|%.4f|%.4f|%s", strings.ToUpper(z.Symbol), z.Lo, z.Hi, z.Dir)
}

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
// transitions, debounced per zone (an exit re-arms it, so a fresh re-entry
// fires again).
func runZoneAlerts(ctx context.Context, client *bingx.Client) {
	topic := os.Getenv("NTFY_TOPIC")
	if topic == "" {
		log.Printf("zonealert: NTFY_TOPIC unset — zone alerts disabled")
		return
	}
	n := notify.NewNtfy(os.Getenv("NTFY_SERVER"), topic)
	inside := map[string]bool{} // zoneKey → currently inside
	log.Printf("zonealert: up — polling %s every %s", zonesPath(), zonePollEvery)

	tick := time.NewTicker(zonePollEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		zones := loadZones()
		if len(zones) == 0 {
			continue
		}
		prices := map[string]float64{} // one fetch per symbol per cycle
		for _, z := range zones {
			if z.Enabled != nil && !*z.Enabled {
				continue
			}
			short := strings.ToUpper(z.Symbol)
			sym, ok := zoneShortToSym[short]
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

			key := zoneKey(z)
			in := px >= z.Lo && px <= z.Hi
			if in && !inside[key] {
				dir := strings.ToUpper(z.Dir)
				title := fmt.Sprintf("%s 進%s區 %.4g–%.4g", short, dirWord(dir), z.Lo, z.Hi)
				body := fmt.Sprintf("%s 現價 %.4g 進入 %.4g–%.4g（%s）\n%s\n→ 來判斷 reject / 進場",
					short, px, z.Lo, z.Hi, dir, z.Note)
				tag := "eyes"
				switch dir {
				case "SHORT":
					tag = "chart_with_downwards_trend"
				case "LONG":
					tag = "chart_with_upwards_trend"
				}
				if err := n.Push(ctx, title, body, tag); err != nil {
					log.Printf("zonealert: push failed: %v", err)
				} else {
					log.Printf("zonealert: fired %s px=%.4g zone=%.4g-%.4g", short, px, z.Lo, z.Hi)
				}
			}
			inside[key] = in
		}
	}
}
