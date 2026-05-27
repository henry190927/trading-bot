package notify

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"myFirstGo/trading/signal"
)

// Mac posts a macOS Notification Center banner via osascript. Banner
// includes symbol + side + score in the title, entry/stop in the subtitle,
// and the first confluence reason in the body. Plays the "Glass" sound.
//
// First time it runs, macOS will prompt to allow notifications from Script
// Editor (osascript runs under that bundle). Grant once and you're done.
//
// No-op on non-darwin platforms, so it's safe to include unconditionally.
type Mac struct {
	// Sound is the macOS sound name (e.g. "Glass", "Hero", "Sosumi", "Ping").
	// Empty = silent. Default if zero-valued: "Glass".
	Sound string
}

func (m Mac) Notify(_ context.Context, sig signal.Signal, _ signal.Context) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	sound := m.Sound
	if sound == "" {
		sound = "Glass"
	}

	title := fmt.Sprintf("%s %s score=%d", sig.Symbol, sig.Side, sig.Score)
	subtitle := ""
	if sig.Plan.Entry != 0 {
		subtitle = fmt.Sprintf("entry %.4f  stop %.4f", sig.Plan.Entry, sig.Plan.StopLoss)
	}
	body := "Open terminal for full plan"
	if len(sig.Reasons) > 0 {
		body = sig.Reasons[0]
	}

	script := fmt.Sprintf(
		`display notification "%s" with title "%s" subtitle "%s" sound name "%s"`,
		escape(body), escape(title), escape(subtitle), escape(sound),
	)
	cmd := exec.Command("osascript", "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("osascript: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// escape sanitizes a string for embedding inside an AppleScript double-quoted
// literal. Backslashes first, then double quotes.
func escape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}
