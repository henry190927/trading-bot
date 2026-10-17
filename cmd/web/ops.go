// Ops control panel: live daemon status, lifecycle commands, config edit,
// and an SSE log tail. Designed for mobile use over Tailscale — gives the
// trader full daemon control without having to SSH from the iPhone.
//
// Security model: Tailscale-only + ufw + interface-bound socket, and
// sudoers on the VPS grants `ubuntu` exactly `systemctl start|stop|restart`
// on the three trading units — nothing else. Everything else this file runs
// is unprivileged: `systemctl is-active` / `status` are read-only D-Bus
// queries, and journal reads come from adm group membership. That matters,
// because the narrow grant is only a real boundary if nothing here needs
// more than it. .env edits go through validation so a typo can't write
// garbage into /opt/trading/.env.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/henry190927/trading-bot/zone"

	"github.com/gin-gonic/gin"
)

// envPath is the on-VPS path to the daemon .env file. Override via
// TRADING_ENV_PATH for local dev (where /opt/trading/.env doesn't exist).
func envPath() string {
	if p := os.Getenv("TRADING_ENV_PATH"); p != "" {
		return p
	}
	return "/opt/trading/.env"
}

// validTFs mirrors BingX's accepted interval set, restricted to values
// the strategy can sensibly run on.
var validTFs = []string{"5m", "15m", "30m", "1h", "2h", "4h", "1d"}

// allowedServices is the strict allowlist for any /ops/* action.
// Sudoers on the VPS is scoped to this unit; anything else would
// silently fail at the systemctl call but we reject early for a
// cleaner error.
var allowedServices = []string{"trading-monitor"}

type opsStatus struct {
	Service string `json:"service"`
	Active  bool   `json:"active"`
	State   string `json:"state"` // "active" | "inactive" | "failed" | "unknown"

	// Monitor-specific config read from .env. Populated only when
	// readDaemonStatus("trading-monitor") is called.
	MonitorTFs      []string `json:"monitor_tfs,omitempty"`
	MonitorMinScore int      `json:"monitor_min_score,omitempty"`
	MonitorMinTFs   int      `json:"monitor_min_tfs,omitempty"`
	MonitorMinRatio float64  `json:"monitor_min_ratio"` // 0 = disabled (always emitted so UI can distinguish "off" from "unset")
}

// readDaemonStatus checks the running systemd unit + the monitor's
// MONITOR_* config in .env. Defaults baked into the binary apply when an
// env var is missing.
func readDaemonStatus(service string) opsStatus {
	st := opsStatus{Service: service, State: "unknown"}

	// Unprivileged: is-active is a read-only D-Bus query. Keeping sudo here
	// meant the status card depended on a blanket NOPASSWD grant.
	//
	// Bounded: this runs inline in a page render, and a wedged systemd would
	// otherwise hang the request with no upper limit. 5s is far above the
	// milliseconds a D-Bus read takes.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "systemctl", "is-active", service).CombinedOutput()
	state := strings.TrimSpace(string(out))
	st.State = state
	st.Active = state == "active"

	if service != "trading-monitor" {
		return st
	}
	data, err := os.ReadFile(envPath())
	if err != nil {
		return st
	}
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "MONITOR_TFS="):
			raw := strings.TrimPrefix(line, "MONITOR_TFS=")
			for _, t := range strings.Split(raw, ",") {
				if t = strings.TrimSpace(t); t != "" {
					st.MonitorTFs = append(st.MonitorTFs, t)
				}
			}
		case strings.HasPrefix(line, "MONITOR_MIN_SCORE="):
			if v, err := strconv.Atoi(strings.TrimPrefix(line, "MONITOR_MIN_SCORE=")); err == nil {
				st.MonitorMinScore = v
			}
		case strings.HasPrefix(line, "MONITOR_MIN_TFS="):
			if v, err := strconv.Atoi(strings.TrimPrefix(line, "MONITOR_MIN_TFS=")); err == nil {
				st.MonitorMinTFs = v
			}
		case strings.HasPrefix(line, "MONITOR_MIN_RATIO="):
			if v, err := strconv.ParseFloat(strings.TrimPrefix(line, "MONITOR_MIN_RATIO="), 64); err == nil {
				st.MonitorMinRatio = v
			}
		}
	}

	// Fall back to baked-in defaults (matches cmd/monitor's flag defaults)
	// so the UI shows current values when .env hasn't been touched yet.
	if service == "trading-monitor" {
		if len(st.MonitorTFs) == 0 {
			st.MonitorTFs = []string{"30m", "1h", "2h", "4h"}
		}
		if st.MonitorMinScore == 0 {
			st.MonitorMinScore = 3
		}
		if st.MonitorMinTFs == 0 {
			st.MonitorMinTFs = 2
		}
	}
	return st
}

// zoneOnlyActive reports whether MONITOR_ZONE_ONLY=1 is set in .env, i.e.
// whether cmd/monitor short-circuits before the confluence loop.
func zoneOnlyActive() bool {
	data, err := os.ReadFile(envPath())
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MONITOR_ZONE_ONLY=") {
			return strings.TrimSpace(strings.TrimPrefix(line, "MONITOR_ZONE_ONLY=")) == "1"
		}
	}
	return false
}

// parseService extracts and validates the ?service= query param.
func parseService(c *gin.Context) (string, error) {
	svc := c.DefaultQuery("service", "trading-monitor")
	if !contains(allowedServices, svc) {
		return "", fmt.Errorf("service must be one of: %s", strings.Join(allowedServices, ", "))
	}
	return svc, nil
}

// systemctlAction runs `sudo systemctl <action> <service>`. Both
// arguments must be in their respective allowlists. The VPS sudoers scope
// enforces the same set independently — this is defence in depth, not the
// only gate — but rejecting early gives a clean error instead of a sudo
// permission failure.
func systemctlAction(service, action string) error {
	switch action {
	case "start", "stop", "restart":
	default:
		return fmt.Errorf("invalid action %q", action)
	}
	if !contains(allowedServices, service) {
		return fmt.Errorf("invalid service %q", service)
	}
	// 30s, not the status path's 5s: a restart legitimately takes seconds
	// while the unit stops and comes back.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sudo", "systemctl", action, service).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s %s: %w (%s)", action, service, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// handleOpsPage renders the control panel HTML shell. The monitor's status
// is rendered at first paint and refreshed by polling /ops/status. Service
// restarts and raw logs are not on this page: they are an SSH job
// (scripts/ops), so the web service needs no host privileges for them.
func (s *server) handleOpsPage(c *gin.Context) {
	// MONITOR_ZONE_ONLY=1 makes cmd/monitor return before the confluence loop,
	// which is the ONLY consumer of MONITOR_TFS / MIN_SCORE / MIN_TFS /
	// MIN_RATIO. The form below therefore writes .env and restarts a live
	// daemon for zero behavioural change — so the page needs to know.
	zoneOnly := false
	if data, err := os.ReadFile(envPath()); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "MONITOR_ZONE_ONLY=") {
				zoneOnly = strings.TrimSpace(strings.TrimPrefix(line, "MONITOR_ZONE_ONLY=")) == "1"
			}
		}
	}
	c.HTML(http.StatusOK, "ops.html", gin.H{
		"MonitorZoneOnly":  zoneOnly,
		"Monitor":          readDaemonStatus("trading-monitor"),
		"MonitorTFOptions": []string{"5m", "15m", "30m", "1h", "2h", "4h"},
		"Zone":             zone.ReadConfig(),
		"ZoneTFList":       zone.ReadConfig().TFList(),
		"ZoneTFOptions":    []string{"15m", "30m", "1h", "2h", "4h"},
	})
}

func (s *server) handleOpsStatus(c *gin.Context) {
	svc, err := parseService(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, readDaemonStatus(svc))
}

// handleOpsZones reports the zone-fade alert config (read from .env, so it
// matches what the monitor daemon actually uses) plus the currently-armed
// zones — auto ones derived live from AnalyzeStructure (same shared `zone`
// package the monitor fires from) plus any manual pins. Lets /ops show what
// the zone-alert channel is watching right now.
func (s *server) handleOpsZones(c *gin.Context) {
	cfg := zone.ReadConfig()
	ntfyOn := false
	if data, err := os.ReadFile(envPath()); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "NTFY_TOPIC=") {
				// presence only — never expose the topic value
				ntfyOn = strings.TrimSpace(strings.TrimPrefix(line, "NTFY_TOPIC=")) != ""
			}
		}
	}
	// "armed" must mean ARMED. runZoneAlerts skips any zone with an explicit
	// enabled:false, so listing those here overstated the channel — 13 rows
	// shown while 2 could fire. Retired zones are counted, not hidden.
	// A zone the daemon would refuse to evaluate (bad `confirm`) must not be
	// listed as plain ARMED either — zone.TriggerFault is the same check
	// runZoneAlerts uses, so the row carries the daemon's own verdict.
	type armedZone struct {
		zone.Zone
		Trigger string `json:"trigger"` // human label for the fire rule
		Fault   string `json:"fault,omitempty"`
	}
	wrap := func(z zone.Zone) armedZone {
		return armedZone{Zone: z, Trigger: zone.ConfirmWord(z.Confirm), Fault: zone.TriggerFault(z)}
	}
	armed := []armedZone{}
	var retired, faulted int
	for _, z := range zone.LoadManual() {
		if z.Enabled != nil && !*z.Enabled {
			retired++
			continue
		}
		a := wrap(z)
		if a.Fault != "" {
			faulted++
		}
		armed = append(armed, a)
	}
	if cfg.Enabled && cfg.Auto && s.client != nil {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
		defer cancel()
		for _, z := range zone.ComputeAuto(ctx, s.client, cfg.TFList()) {
			armed = append(armed, wrap(z))
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"enabled": cfg.Enabled,
		"auto":    cfg.Auto,
		"tfs":     cfg.TFs,
		"ntfy":    ntfyOn,
		"poll":    "25s",
		"armed":   armed,
		"retired": retired,
		"faulted": faulted,
	})
}

// handleOpsZoneConfig persists the zone-alert config. The monitor re-reads
// it each cycle, so changes apply live (no restart). Form sends enabled/auto
// as explicit "1"/"0" plus a tfs csv.
func (s *server) handleOpsZoneConfig(c *gin.Context) {
	// Merge with the current config — Start/Stop send only `enabled`, the
	// config form sends `auto` + `tfs[]`, so absent fields stay untouched.
	cfg := zone.ReadConfig()
	if v := c.PostForm("enabled"); v != "" {
		cfg.Enabled = v == "1"
	}
	if v := c.PostForm("auto"); v != "" {
		cfg.Auto = v == "1"
	}
	if tfArr := c.PostFormArray("tfs"); len(tfArr) > 0 {
		for _, p := range tfArr {
			if !contains(validTFs, strings.TrimSpace(p)) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tf: " + p})
				return
			}
		}
		cfg.TFs = strings.Join(tfArr, ",")
	}
	if strings.TrimSpace(cfg.TFs) == "" {
		cfg.TFs = "1h,2h"
	}
	if err := zone.WriteConfig(cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "enabled": cfg.Enabled, "auto": cfg.Auto, "tfs": cfg.TFs})
}

// handleOpsZoneLogs returns the most recent zone-alert events from the
// monitor's event log (zone.EventsPath). It used to grep the monitor's
// journal, which needed the web process in the adm group and only ever saw
// the last few hundred journal lines.
func (s *server) handleOpsZoneLogs(c *gin.Context) {
	events, skipped, err := zone.RecentEvents(zone.EventsPath(), 40)
	if err != nil && !os.IsNotExist(err) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	tpe := time.FixedZone("Asia/Taipei", 8*3600)
	lines := make([]string, 0, len(events))
	for _, e := range events {
		lines = append(lines, e.At.In(tpe).Format("01-02 15:04:05")+"  "+e.Text)
	}
	c.JSON(http.StatusOK, gin.H{"lines": lines, "skipped": skipped})
}

// handleOpsConfig accepts monitor config changes and rewrites the .env file,
// then restarts the monitor so the new values take effect: MONITOR_TFS +
// MONITOR_MIN_SCORE + MONITOR_MIN_TFS + MONITOR_MIN_RATIO.
func (s *server) handleOpsConfig(c *gin.Context) {
	svc := c.DefaultQuery("service", "trading-monitor")
	switch svc {
	case "trading-monitor":
		s.opsConfigMonitor(c)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid service"})
	}
}

// opsConfigMonitor — monitor-specific config: TFs (multi-checkbox),
// min-score, min-tfs. Persisted to .env as MONITOR_TFS / MONITOR_MIN_SCORE
// / MONITOR_MIN_TFS, then trading-monitor.service is restarted (if active)
// so it re-reads them at startup.
func (s *server) opsConfigMonitor(c *gin.Context) {
	monitorTFs := []string{"5m", "15m", "30m", "1h", "2h", "4h"}

	// PostFormArray returns all checkbox values for name="tfs".
	selectedTFs := c.PostFormArray("tfs")
	for _, tf := range selectedTFs {
		if !contains(monitorTFs, tf) {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid TF %q", tf)})
			return
		}
	}
	scoreStr := strings.TrimSpace(c.PostForm("min_score"))
	tfsStr := strings.TrimSpace(c.PostForm("min_tfs"))
	ratioStr := strings.TrimSpace(c.PostForm("min_ratio"))

	var score, mtfs int
	var ratioPtr *float64
	if scoreStr != "" {
		v, err := strconv.Atoi(scoreStr)
		if err != nil || v < 1 || v > 8 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "min_score must be 1-8"})
			return
		}
		score = v
	}
	if tfsStr != "" {
		v, err := strconv.Atoi(tfsStr)
		if err != nil || v < 1 || v > 5 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "min_tfs must be 1-5"})
			return
		}
		mtfs = v
	}
	if ratioStr != "" {
		v, err := strconv.ParseFloat(ratioStr, 64)
		if err != nil || v < 0 || v > 10 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "min_ratio must be 0-10 (0 disables)"})
			return
		}
		ratioPtr = &v
	}
	if len(selectedTFs) == 0 && score == 0 && mtfs == 0 && ratioPtr == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "nothing to update"})
		return
	}
	// Cross-validate: can't require more TFs than we're monitoring.
	if mtfs > 0 && len(selectedTFs) > 0 && mtfs > len(selectedTFs) {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("min_tfs (%d) cannot exceed number of selected TFs (%d)", mtfs, len(selectedTFs))})
		return
	}

	// Refuse rather than restart a live daemon for nothing. The UI disables the
	// button too, but the endpoint is the real gate — a stale tab must not be
	// able to bounce autoexec/zonealert for a config that cannot apply.
	if zoneOnlyActive() {
		c.JSON(http.StatusConflict, gin.H{
			"error": "MONITOR_ZONE_ONLY=1 — 這些參數只有 confluence 掃描迴圈會讀,而該迴圈目前未啟動。改動不會生效,且 Apply 會白白重啟 monitor。先取消 MONITOR_ZONE_ONLY 再設定。",
		})
		return
	}
	if err := updateMonitorEnvConfig(selectedTFs, score, mtfs, ratioPtr); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	st := readDaemonStatus("trading-monitor")
	if st.Active {
		if err := systemctlAction("trading-monitor", "restart"); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error(), "config_saved": true})
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	c.JSON(http.StatusOK, readDaemonStatus("trading-monitor"))
}

// updateMonitorEnvConfig rewrites MONITOR_TFS / MONITOR_MIN_SCORE /
// MONITOR_MIN_TFS / MONITOR_MIN_RATIO in the .env file. Missing/zero ints
// are skipped; a nil minRatio means "don't touch", a non-nil value
// (including 0 to disable) is written.
// Lines that don't exist yet are appended; existing lines are replaced.
func updateMonitorEnvConfig(tfs []string, minScore, minTFs int, minRatio *float64) error {
	path := envPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read env: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	want := map[string]string{}
	if len(tfs) > 0 {
		want["MONITOR_TFS"] = strings.Join(tfs, ",")
	}
	if minScore > 0 {
		want["MONITOR_MIN_SCORE"] = strconv.Itoa(minScore)
	}
	if minTFs > 0 {
		want["MONITOR_MIN_TFS"] = strconv.Itoa(minTFs)
	}
	if minRatio != nil {
		want["MONITOR_MIN_RATIO"] = strconv.FormatFloat(*minRatio, 'f', -1, 64)
	}
	seen := map[string]bool{}
	for i, line := range lines {
		for k, v := range want {
			if strings.HasPrefix(line, k+"=") {
				lines[i] = k + "=" + v
				seen[k] = true
			}
		}
	}
	for k, v := range want {
		if !seen[k] {
			lines = append(lines, k+"="+v)
		}
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0644)
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
