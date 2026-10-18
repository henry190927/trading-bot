// In-app alerts: the rules, the loop that evaluates them, and the API the
// bell in every page's nav reads.
//
// This replaced the /ops "系統狀態" card and the /today daemon row. Those
// showed state to whoever happened to look; this turns state into incidents
// that are kept until someone acknowledges them (package alert, and
// docs/alerts.md for the design).
//
// The rules run HERE, in the web service, and not in the monitor, because a
// process cannot report its own death: the web watches the monitor, and the
// host's daemon-watch cron watches the web. That second leg is also why web
// down can only ever be an ntfy push — the page that would show it is the
// thing that is down.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/henry190927/trading-bot/alert"
	"github.com/henry190927/trading-bot/heartbeat"
	"github.com/henry190927/trading-bot/macro"
	"github.com/henry190927/trading-bot/monitorcfg"
	"github.com/henry190927/trading-bot/notify"
)

// alertEvery is the evaluation cadence: the heartbeat's own write interval,
// since nothing it reports can change faster. With FireAfter 2, a condition
// fires 15-30s after it becomes true.
const alertEvery = heartbeat.WriteEvery

// wsStaleAfter is how long the exchange feed may go without a mark frame
// while connected. It is a ~1 Hz feed, so 30s is many missed frames, not
// one slow one.
const wsStaleAfter = 30

// restartWindow and restartLimit define a restart loop: three monitor starts
// inside an hour is a crash (or OOM) loop, not three deploys.
const (
	restartWindow = time.Hour
	restartLimit  = 3
)

// alertInput is everything the rules look at, gathered once per round so the
// rules themselves are a pure function.
type alertInput struct {
	now      time.Time
	beat     heartbeat.Beat
	beatErr  error
	hub      hubStats
	overlay  macro.Status
	restarts int // monitor starts seen inside restartWindow
}

// loopNames gives the monitor's loop keys a human name in the alert title.
// Unknown loops fall back to the key, so a new loop still alerts.
var loopNames = map[string]string{
	"bracket":       "裸倉守衛 (bracket)",
	"confluence":    "多週期共振 (confluence)",
	"zonealert":     "樞紐區提醒 (zonealert)",
	"structalert":   "結構提醒 (structalert)",
	"autoexec":      "自動執行器 (autoexec)",
	"macrowarn":     "總經事件提醒 (macrowarn)",
	"oisample":      "OI 取樣 (oisample)",
	"binfutrefresh": "Binance 持倉快取 (binfutrefresh)",
	"blsrefresh":    "BLS 數據快取 (blsrefresh)",
}

// evaluateAlerts is the rule set. It returns what is wrong right now, and
// the key prefixes it could not judge this round (see alert.Engine.Observe).
func evaluateAlerts(in alertInput) (firing []alert.Condition, held []string) {
	// ── The monitor itself ───────────────────────────────────────────
	// Critical, because the bracket guard lives inside it: a dead monitor is
	// an unwatched book. The push for it comes from daemon-watch, not from
	// here (see alertPushes).
	down := ""
	switch {
	case errors.Is(in.beatErr, fs.ErrNotExist):
		down = "沒有心跳檔 — monitor 沒在跑,或從未啟動"
	case in.beatErr != nil:
		down = "讀不到心跳檔:" + in.beatErr.Error()
	case !in.beat.Alive(in.now):
		down = fmt.Sprintf("心跳 %ds 沒更新(超過 %ds 視為停止)",
			int(in.now.Sub(in.beat.WrittenAt)/time.Second), int(heartbeat.MaxAge/time.Second))
	}
	if down != "" {
		firing = append(firing, alert.Condition{
			Key: "monitor_down", Severity: alert.Critical,
			Title: "trading-monitor 停止回報", Detail: down + "。裸倉守衛也一起停了。",
		})
		// Inhibition: with no fresh heartbeat every loop is unknown, and
		// nine "loop stalled" alerts would only bury the one that matters.
		held = append(held, "loop_stalled:")
	} else {
		for _, name := range in.beat.Names() {
			l := in.beat.Loops[name]
			if !l.Stalled(in.now) {
				continue
			}
			sev := alert.Warning
			if name == "bracket" {
				sev = alert.Critical
			}
			label := loopNames[name]
			if label == "" {
				label = name
			}
			firing = append(firing, alert.Condition{
				Key: "loop_stalled:" + name, Severity: sev,
				Title: label + " 卡住",
				Detail: fmt.Sprintf("%ds 沒開始新一輪(應每 %ds 一次)— 行程還在,多半卡在網路呼叫",
					int(in.now.Sub(l.LastTick)/time.Second), l.EverySec),
			})
		}
	}
	if in.restarts >= restartLimit {
		firing = append(firing, alert.Condition{
			Key: "monitor_restart_loop", Severity: alert.Warning,
			Title:  "trading-monitor 反覆重啟",
			Detail: fmt.Sprintf("1 小時內啟動 %d 次 — 檢查是否 OOM(ops logs monitor)", in.restarts),
		})
	}

	// ── Exchange price feed ──────────────────────────────────────────
	// The hub is lazy: it only connects while a page is subscribed, and pages
	// are the feed's only consumer. Idle therefore CLEARS the alert rather
	// than holding it: holding left a "price feed down" incident open for as
	// long as nobody had a page open, which is exactly when it harms nothing.
	switch {
	case !in.hub.Running:
	case !in.hub.WSUp:
		firing = append(firing, alert.Condition{
			Key: "price_ws", Severity: alert.Warning,
			Title:  "交易所價格 WebSocket 斷線",
			Detail: "改用 30 秒一次的 REST 備援價格。" + wsErrSuffix(in.hub.WSLastErr),
		})
	case in.hub.WSLastAgeS > wsStaleAfter:
		firing = append(firing, alert.Condition{
			Key: "price_ws", Severity: alert.Warning,
			Title:  "交易所價格 WebSocket 沒有資料",
			Detail: fmt.Sprintf("連線中,但 %ds 沒收到報價(正常約每秒一筆)", in.hub.WSLastAgeS),
		})
	}

	// ── Runtime macro overlay ────────────────────────────────────────
	// Its failure is silent in the dangerous direction: a mistyped blackout
	// reads exactly like "no blackout".
	switch {
	case in.overlay.Err != "":
		firing = append(firing, alert.Condition{
			Key: "macro_overlay", Severity: alert.Warning,
			Title:  "macro overlay 讀取失敗",
			Detail: "沿用上一份成功載入的版本。" + in.overlay.Err,
		})
	case in.overlay.Skipped > 0:
		firing = append(firing, alert.Condition{
			Key: "macro_overlay", Severity: alert.Warning,
			Title:  fmt.Sprintf("macro overlay 有 %d 筆沒有生效", in.overlay.Skipped),
			Detail: strings.Join(in.overlay.Problems, ";"),
		})
	}
	return firing, held
}

func wsErrSuffix(e string) string {
	if e == "" {
		return ""
	}
	return "最後錯誤:" + e
}

// restartTracker notices the monitor restarting by its heartbeat's
// started_at changing — no Docker socket needed, and it sees an OOM kill the
// same way it sees a deploy.
type restartTracker struct {
	last   time.Time
	starts []time.Time
}

// observe returns whether this is a restart (not the first sighting after
// the web itself started) and how many starts fall inside restartWindow.
func (r *restartTracker) observe(started, now time.Time) (restarted bool, recent int) {
	if !started.IsZero() && !started.Equal(r.last) {
		restarted = !r.last.IsZero()
		r.last = started
		if restarted {
			r.starts = append(r.starts, started)
		}
	}
	keep := r.starts[:0]
	for _, t := range r.starts {
		if now.Sub(t) <= restartWindow {
			keep = append(keep, t)
		}
	}
	r.starts = keep
	return restarted, len(r.starts)
}

// alertPushes decides which changes also go to ntfy: critical ones only, and
// not monitor_down — daemon-watch already pushes that one, from outside both
// containers, and two pushes for one outage teach you to ignore both.
func alertPushes(c alert.Change) bool {
	return c.Alert.Severity == alert.Critical && c.Alert.Key != "monitor_down" &&
		(c.Kind == "fire" || c.Kind == "resolve")
}

// startAlerts opens the log, records the web's own start, and runs the rules
// every alertEvery until ctx ends.
func (s *server) startAlerts(ctx context.Context) {
	eng, skipped, err := alert.Open(alert.Path())
	if err != nil {
		log.Printf("[alert] open %s: %v (continuing with what was read)", alert.Path(), err)
	}
	if skipped > 0 {
		log.Printf("[alert] %s: %d unreadable lines skipped", alert.Path(), skipped)
	}
	eng.Logf = log.Printf

	if topic := os.Getenv("NTFY_TOPIC"); topic != "" {
		push := notify.NewNtfy(os.Getenv("NTFY_SERVER"), topic)
		// The same runtime mute the monitor honours. A read error fails open
		// (pushes), matching the monitor.
		push.Muted = func() bool {
			cfg, err := monitorcfg.Read(monitorcfg.Path())
			return err == nil && cfg.PushMuted
		}
		eng.OnChange = func(c alert.Change) {
			if !alertPushes(c) {
				return
			}
			title := "🚨 " + c.Alert.Title
			if c.Kind == "resolve" {
				title = "✅ 已恢復:" + c.Alert.Title
			}
			go func() {
				pctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				if err := push.Push(pctx, title, c.Alert.Detail, "rotating_light"); err != nil {
					log.Printf("[alert] ntfy: %v", err)
				}
			}()
		}
	}
	s.alerts = eng
	eng.Notice("web_started", alert.Info, "trading-web 啟動", "")

	go func() {
		var rt restartTracker
		tick := time.NewTicker(alertEvery)
		defer tick.Stop()
		for {
			now := time.Now()
			in := alertInput{now: now, hub: s.hub.stats(), overlay: macro.OverlayStatus()}
			in.beat, in.beatErr = heartbeat.Read(heartbeat.Path())
			if in.beatErr == nil {
				var restarted bool
				restarted, in.restarts = rt.observe(in.beat.StartedAt, now)
				if restarted {
					eng.Notice("monitor_restarted", alert.Info, "trading-monitor 重新啟動",
						"新行程 pid "+strconv.Itoa(in.beat.PID))
				}
			}
			eng.Observe(evaluateAlerts(in))
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
}

// handleAlertsAPI — GET /api/alerts. Polled by the bell on every page.
func (s *server) handleAlertsAPI(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, s.alerts.Snapshot(100))
}

// handleAlertsAck — POST /api/alerts/ack (id=N, or id=0 / absent for all).
func (s *server) handleAlertsAck(c *gin.Context) {
	id, _ := strconv.ParseInt(c.PostForm("id"), 10, 64)
	n, err := s.alerts.Ack(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "acked": n})
}

// handleAlertsPage — GET /alerts, the full history.
func (s *server) handleAlertsPage(c *gin.Context) {
	s.page(c, http.StatusOK, "alerts.html", gin.H{})
}
