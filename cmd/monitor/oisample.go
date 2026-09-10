package main

// Open-interest sampler.
//
// The engine has wanted a prior OI reading since the 2026-05-28 OI-semantics
// review — annotateContextWarnings turns one into "OI up X% — shorts
// crowding, squeeze risk" or "OI down X% — long unwind driving the move, not
// new buying". Nothing ever produced it, so both warnings were unreachable
// (see package oi for the full account). This is the producer.
//
// It lives in the monitor because the monitor is the only long-lived process:
// cmd/analyze is one-shot and the web handlers are per-request, so neither can
// remember what OI was an hour ago. They read the store this writes.
//
// Sampling, not deriving. BingX publishes no long/short ratio (both
// /quote/longShortRatio spellings answer code=100400 "this api is not exist"),
// so OI change against price change is the only positioning measurement
// available from the venue. That makes the snapshot series the whole input —
// there is nothing to recompute it from after the fact, which is why this
// samples on a clock rather than on demand.

import (
	"context"
	"log"
	"os"
	"sort"
	"strconv"
	"time"

	"myFirstGo/trading-bot/bingx"
	"myFirstGo/trading-bot/market"
	"myFirstGo/trading-bot/oi"
)

const (
	// 5 minutes puts a reading within ±2.5min of any bar boundary, which is
	// well inside the tolerance readers use, and costs one cheap public GET
	// per symbol. The engine's threshold is a ±2% OI move; nothing about
	// that needs finer resolution.
	defaultOISampleInterval = 5 * time.Minute
	// Log a summary hourly rather than every tick — twelve lines an hour of
	// "sampled 14" would bury the alerts this daemon exists to surface.
	oiLogEveryTicks = 12
	// Spacing between symbols inside one cycle, so 14 requests do not leave
	// as a burst.
	oiSymbolStagger = 400 * time.Millisecond
)

// oiSampleInterval reads OI_SAMPLE_SEC, falling back to the default. Zero or
// a bad value disables sampling entirely rather than guessing: an operator who
// set it to 0 wants it off.
func oiSampleInterval() time.Duration {
	raw := os.Getenv("OI_SAMPLE_SEC")
	if raw == "" {
		return defaultOISampleInterval
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		log.Printf("oisample: OI_SAMPLE_SEC=%q unusable — using %s", raw, defaultOISampleInterval)
		return defaultOISampleInterval
	}
	return time.Duration(n) * time.Second
}

func runOISampler(ctx context.Context, client *bingx.Client) {
	every := oiSampleInterval()
	if every == 0 {
		log.Printf("oisample: OI_SAMPLE_SEC=0 — sampler disabled, OI crowding warnings stay silent")
		return
	}

	// Sorted so the log reads the same way every cycle.
	shorts := make([]string, 0, len(autoSymbols))
	for s := range autoSymbols {
		shorts = append(shorts, s)
	}
	sort.Strings(shorts)

	log.Printf("oisample: up — %d symbols every %s -> %s (retain %s)",
		len(shorts), every, oi.Path(), oi.Retain)

	if err := oi.Prune(time.Now().UTC()); err != nil {
		log.Printf("oisample: prune on start: %v", err)
	}

	tick := time.NewTicker(every)
	defer tick.Stop()
	pruneTick := time.NewTicker(6 * time.Hour)
	defer pruneTick.Stop()

	cycle := 0
	for {
		ok, failed := sampleOICycle(ctx, client, shorts)
		cycle++
		// Always report failures; report success only hourly. A silent
		// sampler that has been failing for a week would otherwise look
		// identical to one that never started.
		if failed > 0 {
			log.Printf("oisample: %d/%d sampled, %d failed", ok, len(shorts), failed)
		} else if cycle == 1 || cycle%oiLogEveryTicks == 0 {
			log.Printf("oisample: %d/%d sampled", ok, len(shorts))
		}

		select {
		case <-ctx.Done():
			return
		case <-pruneTick.C:
			if err := oi.Prune(time.Now().UTC()); err != nil {
				log.Printf("oisample: prune: %v", err)
			}
		case <-tick.C:
		}
	}
}

// sampleOICycle takes one reading per symbol. Errors are counted, not
// returned: one symbol delisting or timing out must not stop the other
// thirteen from being recorded.
func sampleOICycle(ctx context.Context, client *bingx.Client, shorts []string) (ok, failed int) {
	for _, short := range shorts {
		select {
		case <-ctx.Done():
			return ok, failed
		default:
		}
		sym := autoSymbols[short]
		v, err := readOI(ctx, client, sym)
		if err != nil {
			failed++
			continue
		}
		oi.Append(oi.Snapshot{Time: time.Now().UTC(), Symbol: string(sym), OI: v})
		ok++
		time.Sleep(oiSymbolStagger)
	}
	return ok, failed
}

func readOI(ctx context.Context, client *bingx.Client, sym market.Symbol) (float64, error) {
	c, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return client.OpenInterest(c, sym)
}
