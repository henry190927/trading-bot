// Ops control panel: live daemon status, lifecycle commands, config edit,
// and an SSE log tail. Designed for mobile use over Tailscale — gives the
// trader full daemon control without having to SSH from the iPhone.
//
// Security model is unchanged from the rest of the app: Tailscale-only +
// ufw + interface-bound socket. Sudoers on the VPS scopes `ubuntu` to
// systemctl{start,stop,restart,status} trading-bot and journalctl -u
// trading-bot — nothing else. .env edits go through validation here so
// a typo can't write garbage into /opt/trading/.env.
package main

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

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

type opsStatus struct {
	Active   bool   `json:"active"`
	State    string `json:"state"` // "active" | "inactive" | "failed" | "unknown"
	TF       string `json:"tf"`
	MinScore int    `json:"min_score"`
	EnvOK    bool   `json:"env_ok"` // false if envPath not readable
}

// readDaemonStatus checks the running systemd unit + the .env file. Used by
// both the HTML page render and the JSON polling endpoint.
func readDaemonStatus() opsStatus {
	st := opsStatus{State: "unknown"}

	out, _ := exec.Command("sudo", "systemctl", "is-active", "trading-bot").CombinedOutput()
	state := strings.TrimSpace(string(out))
	st.State = state
	st.Active = state == "active"

	data, err := os.ReadFile(envPath())
	if err != nil {
		return st
	}
	st.EnvOK = true
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "TRADING_TF="):
			st.TF = strings.TrimPrefix(line, "TRADING_TF=")
		case strings.HasPrefix(line, "TRADING_MIN_SCORE="):
			if v, err := strconv.Atoi(strings.TrimPrefix(line, "TRADING_MIN_SCORE=")); err == nil {
				st.MinScore = v
			}
		}
	}
	return st
}

// updateEnvConfig rewrites TRADING_TF and TRADING_MIN_SCORE in the .env file
// in place. Leaves all other lines untouched. Returns an error if validation
// fails or the file can't be written.
func updateEnvConfig(tf string, minScore int) error {
	if tf != "" && !contains(validTFs, tf) {
		return fmt.Errorf("invalid tf %q (allowed: %s)", tf, strings.Join(validTFs, ", "))
	}
	if minScore != 0 && (minScore < 1 || minScore > 8) {
		return fmt.Errorf("invalid min_score %d (allowed: 1-8)", minScore)
	}

	path := envPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read env: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if tf != "" && strings.HasPrefix(line, "TRADING_TF=") {
			lines[i] = "TRADING_TF=" + tf
		}
		if minScore != 0 && strings.HasPrefix(line, "TRADING_MIN_SCORE=") {
			lines[i] = "TRADING_MIN_SCORE=" + strconv.Itoa(minScore)
		}
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0644)
}

// systemctlAction runs `sudo systemctl <action> trading-bot`. Action must be
// in the sudoers-allowed set (start | stop | restart).
func systemctlAction(action string) error {
	switch action {
	case "start", "stop", "restart":
	default:
		return fmt.Errorf("invalid action %q", action)
	}
	cmd := exec.Command("sudo", "systemctl", action, "trading-bot")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %v (%s)", action, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// handleOpsPage renders the control panel HTML shell. Live status + logs
// arrive via /ops/status (JSON polled by JS) and /ops/logs (SSE stream).
func (s *server) handleOpsPage(c *gin.Context) {
	st := readDaemonStatus()
	c.HTML(http.StatusOK, "ops.html", gin.H{
		"Status":   st,
		"ValidTFs": validTFs,
	})
}

func (s *server) handleOpsStatus(c *gin.Context) {
	c.JSON(http.StatusOK, readDaemonStatus())
}

func (s *server) handleOpsStart(c *gin.Context) {
	if err := systemctlAction("start"); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	time.Sleep(500 * time.Millisecond) // let systemd settle
	c.JSON(http.StatusOK, readDaemonStatus())
}

func (s *server) handleOpsStop(c *gin.Context) {
	if err := systemctlAction("stop"); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	time.Sleep(500 * time.Millisecond)
	c.JSON(http.StatusOK, readDaemonStatus())
}

func (s *server) handleOpsRestart(c *gin.Context) {
	if err := systemctlAction("restart"); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	time.Sleep(500 * time.Millisecond)
	c.JSON(http.StatusOK, readDaemonStatus())
}

// handleOpsConfig accepts TF and/or min_score POST fields, updates .env, then
// restarts the daemon so the new values take effect on the next scan.
func (s *server) handleOpsConfig(c *gin.Context) {
	tf := strings.TrimSpace(c.PostForm("tf"))
	scoreStr := strings.TrimSpace(c.PostForm("min_score"))

	var score int
	if scoreStr != "" {
		v, err := strconv.Atoi(scoreStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "min_score not an integer"})
			return
		}
		score = v
	}
	if tf == "" && score == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "nothing to update — provide tf and/or min_score"})
		return
	}
	if err := updateEnvConfig(tf, score); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Restart so the daemon picks up the new env vars. Only restart if it's
	// already running — config change while stopped just updates the file.
	st := readDaemonStatus()
	if st.Active {
		if err := systemctlAction("restart"); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error(), "config_saved": true})
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	c.JSON(http.StatusOK, readDaemonStatus())
}

// handleOpsLogs streams journalctl -u trading-bot -f over Server-Sent Events.
// Initial backlog: last 50 lines. Then follows new entries in real time.
// Closes cleanly when the client disconnects (Gin's request context cancels).
func (s *server) handleOpsLogs(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache, no-transform")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no") // disable proxy buffering if any

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.String(http.StatusInternalServerError, "streaming unsupported")
		return
	}

	ctx := c.Request.Context()
	cmd := exec.CommandContext(ctx, "sudo", "journalctl",
		"-u", "trading-bot",
		"-n", "50",
		"-f",
		"--no-pager",
		"--output=short-iso")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(c.Writer, "data: pipe error: %s\n\n", err.Error())
		flusher.Flush()
		return
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(c.Writer, "data: start error: %s\n\n", err.Error())
		flusher.Flush()
		return
	}
	defer func() { _ = cmd.Wait() }()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 4096), 1024*1024) // up to 1MB per line
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		// SSE format: "data: <line>\n\n". Newlines within the line must be
		// split into multiple "data:" lines, but journalctl produces one
		// log entry per line so this is safe.
		fmt.Fprintf(c.Writer, "data: %s\n\n", scanner.Text())
		flusher.Flush()
	}
	// scanner.Err() typically returns "signal: killed" when context cancels;
	// not worth surfacing to the client.
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
