package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/henry190927/trading-bot/macro"
	"github.com/henry190927/trading-bot/notify"
)

// runMacroWarn pushes an ntfy heads-up BEFORE a macro-event blackout window
// starts, so a scheduled event (FOMC minutes, Jackson Hole, NFP…) never
// surprises the desk the way 8/19 did. Fires once per event, `lead` minutes
// before the blackout window opens. Reads the same curated macro calendar that
// drives the engine gate + /calendar.
func runMacroWarn(ctx context.Context) {
	topic := os.Getenv("NTFY_TOPIC")
	if topic == "" {
		log.Printf("macrowarn: NTFY_TOPIC unset — macro pre-warnings disabled")
		return
	}
	n := notify.NewNtfy(os.Getenv("NTFY_SERVER"), topic)
	const lead = 45 * time.Minute
	alerted := map[string]bool{}

	check := func() {
		now := time.Now().UTC()
		for _, e := range macro.All() {
			start, _ := e.Window()
			key := e.Name + "|" + e.DatetimeUTC.Format(time.RFC3339)
			if alerted[key] {
				continue
			}
			if start.After(now) && start.Before(now.Add(lead)) {
				mins := int(start.Sub(now).Minutes())
				body := fmt.Sprintf("%s — blackout starts in ~%dmin; engine goes quiet %dmin before / %dmin after. Don't open fresh engine trades into it.",
					e.Name, mins, e.BeforeMinutes, e.AfterMinutes)
				if err := n.Push(ctx, "⏰ Macro event soon", body, "alarm_clock,warning"); err != nil {
					log.Printf("macrowarn: push failed: %v", err)
				} else {
					alerted[key] = true
					log.Printf("macrowarn: fired %q (blackout in %dmin)", e.Name, mins)
				}
			}
		}
	}

	log.Printf("macrowarn: up — %v lead before blackout", lead)
	check()
	tick := time.NewTicker(5 * time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			check()
		}
	}
}
