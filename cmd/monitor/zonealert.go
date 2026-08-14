package main

// Zone-alert channel — a fast (25s) live-price-vs-zone watcher that pings
// ntfy when price ENTERS a fade zone. Built for pivot-zone-fade entries so
// trading isn't screen-tethered.
//
// GENERAL / automatic by default (like the engine + confluence monitors):
// every ~2min it derives, for each symbol × TF, the pivot-zone-fade band
// straight from signal.AnalyzeStructure — a CONFIRMED trend (LH-LL/HH-HL)
// with an active 樞紐區 → fade zone, direction = trend direction. Live
// price is then checked against those zones every 25s. Manual zones in
// /opt/trading/zones.json are ALSO honoured (pin a specific level).
//
// Deliberately separate from the confluence scan: that fires on CLOSED
// bars (engine signals). This fires on LIVE intrabar price so a 5m wick
// into the zone isn't missed. See project_ntfy_price_alerts.

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
	"myFirstGo/trading-bot/signal"
)

const (
	zoneDefaultPath = "/opt/trading/zones.json"
	zonePollEvery   = 25 * time.Second
	zoneStructEvery = 2 * time.Minute // structure only changes on bar close
)

// Zone is one price band to watch. Manual zones come from zones.json;
// auto zones are derived from structure each cycle.
type Zone struct {
	Symbol  string  `json:"symbol"` // BTC / ETH / XAU / XAG
	Lo      float64 `json:"lo"`
	Hi      float64 `json:"hi"`
	Dir     string  `json:"dir"`  // short / long / watch
	Note    string  `json:"note"` // free text shown in the push
	TF      string  `json:"tf,omitempty"`
	Enabled *bool   `json:"enabled,omitempty"`
}

var zoneShortToSym = map[string]market.Symbol{
	"BTC": market.BTCUSDT, "ETH": market.ETHUSDT,
	"XAU": market.XAUUSDT, "XAG": market.XAGUSDT,
}

var zoneSymToShort = map[market.Symbol]string{
	market.BTCUSDT: "BTC", market.ETHUSDT: "ETH",
	market.XAUUSDT: "XAU", market.XAGUSDT: "XAG",
}

func zonesPath() string {
	if p := os.Getenv("ZONES_PATH"); strings.TrimSpace(p) != "" {
		return p
	}
	return zoneDefaultPath
}

// zoneAutoEnabled — auto structure zones on unless ZONE_AUTO=off.
func zoneAutoEnabled() bool {
	return strings.ToLower(strings.TrimSpace(os.Getenv("ZONE_AUTO"))) != "off"
}

// zoneAutoTFs — which TFs to derive structure zones on (default 1h,2h).
func zoneAutoTFs() []market.Timeframe {
	raw := os.Getenv("ZONE_TFS")
	if strings.TrimSpace(raw) == "" {
		raw = "1h,2h"
	}
	var out []market.Timeframe
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, market.Timeframe(p))
		}
	}
	return out
}

func loadManualZones() []Zone {
	b, err := os.ReadFile(zonesPath())
	if err != nil {
		return nil
	}
	var zs []Zone
	if err := json.Unmarshal(b, &zs); err != nil {
		log.Printf("zonealert: bad zones.json: %v", err)
		return nil
	}
	return zs
}

// computeAutoZones derives one zone per (symbol, TF) in a CONFIRMED trend
// with an active pivot zone. Trade dir = trend dir (downtrend → fade short,
// uptrend → fade long).
func computeAutoZones(ctx context.Context, client *bingx.Client, tfs []market.Timeframe) []Zone {
	var zs []Zone
	for _, sym := range market.All() {
		short := zoneSymToShort[sym]
		for _, tf := range tfs {
			candles, err := client.Klines(ctx, sym, tf, 250)
			if err != nil || len(candles) < 60 {
				continue
			}
			st := signal.AnalyzeStructure(candles, 2)
			if st.Zone == nil {
				continue
			}
			var dir string
			switch st.Trend {
			case signal.StructDowntrend:
				dir = "short"
			case signal.StructUptrend:
				dir = "long"
			default:
				continue // only fade in a confirmed trend
			}
			lo, hi := st.Zone.Lo, st.Zone.Hi
			if hi < lo {
				lo, hi = hi, lo
			}
			note := fmt.Sprintf("auto %s %s 樞紐區 fade｜stop %.4g target %.4g",
				string(tf), trendWord(st.Trend), st.Zone.Invalidate, st.Zone.Target)
			zs = append(zs, Zone{Symbol: short, Lo: lo, Hi: hi, Dir: dir, Note: note, TF: string(tf)})
		}
	}
	return zs
}

func trendWord(t signal.TrendStructure) string {
	switch t {
	case signal.StructDowntrend:
		return "LH-LL 下跌"
	case signal.StructUptrend:
		return "HH-HL 上升"
	}
	return "neutral"
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

func zoneKey(z Zone) string {
	return fmt.Sprintf("%s|%s|%.4f|%.4f|%s", strings.ToUpper(z.Symbol), z.TF, z.Lo, z.Hi, z.Dir)
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
	autoOn := zoneAutoEnabled()
	tfs := zoneAutoTFs()
	var autoZones []Zone
	var lastStruct time.Time
	log.Printf("zonealert: up — poll %s, auto=%v tfs=%v, manual=%s", zonePollEvery, autoOn, tfs, zonesPath())

	tick := time.NewTicker(zonePollEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-tick.C:
			if autoOn && t.Sub(lastStruct) >= zoneStructEvery {
				autoZones = computeAutoZones(ctx, client, tfs)
				lastStruct = t
			}
		}

		zones := append(loadManualZones(), autoZones...)
		if len(zones) == 0 {
			continue
		}
		prices := map[string]float64{} // one live-price fetch per symbol per cycle
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
				tfTag := ""
				if z.TF != "" {
					tfTag = " " + z.TF
				}
				title := fmt.Sprintf("%s%s 進%s區 %.4g–%.4g", short, tfTag, dirWord(dir), z.Lo, z.Hi)
				body := fmt.Sprintf("%s%s 現價 %.4g 進入 %.4g–%.4g（%s）\n%s\n→ 來判斷 reject / 進場",
					short, tfTag, px, z.Lo, z.Hi, dir, z.Note)
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
					log.Printf("zonealert: fired %s%s px=%.4g zone=%.4g-%.4g dir=%s", short, tfTag, px, z.Lo, z.Hi, dir)
				}
			}
			inside[key] = in
		}
	}
}
