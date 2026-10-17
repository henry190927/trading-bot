package main

// Runtime push muting, shared by every loop that sends to ntfy.
//
// Muting used to mean clearing NTFY_TOPIC in .env and restarting, which
// stopped the push-only loops outright. Now it is monitorcfg.PushMuted,
// re-read on every send: loops keep running and evaluating, only the push is
// skipped, and un-muting applies on the next send without a restart.

import (
	"context"
	"log"
	"os"
	"sync"
	"time"

	"github.com/henry190927/trading-bot/heartbeat"
	"github.com/henry190927/trading-bot/monitorcfg"
	"github.com/henry190927/trading-bot/notify"
)

const mutedReason = "推播靜音 → 照跑,不推播"

var (
	muteErrMu   sync.Mutex
	lastMuteErr string
)

// pushMuted reports the current runtime mute. An unreadable config leaves
// push ON (see monitorcfg.Read) and is logged once per distinct error, not
// once per call — this runs every few seconds.
func pushMuted() bool {
	c, err := monitorcfg.Read(monitorcfg.Path())
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	muteErrMu.Lock()
	if msg != lastMuteErr {
		if msg != "" {
			log.Printf("monitorcfg: %s — push stays ON", msg)
		} else {
			log.Printf("monitorcfg: readable again")
		}
		lastMuteErr = msg
	}
	muteErrMu.Unlock()
	return c.PushMuted
}

// newNtfy is the one constructor every loop uses, so the mute cannot be
// missed by a loop that builds its own sink.
func newNtfy(topic string) *notify.Ntfy {
	n := notify.NewNtfy(os.Getenv("NTFY_SERVER"), topic)
	n.Muted = pushMuted
	return n
}

// markPushIdle reports a push-only loop as idle in the heartbeat while push
// is muted: it is still running, but nothing it finds reaches anyone.
func markPushIdle(name, activeReason string) {
	if pushMuted() {
		hb.SetIdle(name, true, mutedReason)
	} else {
		hb.SetIdle(name, false, activeReason)
	}
}

var (
	pushLoopsMu sync.Mutex
	pushLoops   = map[string]string{} // loop name -> reason shown while not muted
)

// trackPushIdle registers a push-only loop whose own cycle is minutes long,
// so watchMute keeps its heartbeat in step with the mute. Without it a 5m
// loop would read "run" for up to 5 minutes after a mute, and the /ops
// side-by-side would report a disagreement that is only lag.
//
// Only for loops with no other reason to be idle: watchMute owns their idle
// flag outright. zonealert also idles when its channel is disabled, so it
// calls markPushIdle from its own 25s cycle instead.
func trackPushIdle(name, activeReason string) {
	pushLoopsMu.Lock()
	pushLoops[name] = activeReason
	pushLoopsMu.Unlock()
	markPushIdle(name, activeReason)
}

// watchMute re-applies the mute to every tracked loop at the heartbeat's
// write cadence, so what /ops shows is never more than one write behind.
func watchMute(ctx context.Context) {
	tick := time.NewTicker(heartbeat.WriteEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		muted := pushMuted()
		pushLoopsMu.Lock()
		for name, reason := range pushLoops {
			if muted {
				hb.SetIdle(name, true, mutedReason)
			} else {
				hb.SetIdle(name, false, reason)
			}
		}
		pushLoopsMu.Unlock()
	}
}
