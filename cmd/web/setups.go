package main

// Setup forward-log: record every N字 structure setup you *consider*
// (whether or not you actually open a trade), then backfill the outcome
// once the market resolves it — did price close through the BOS level
// (continuation confirmed) or through the protected/invalidation level
// (setup voided)? Accumulating these — including the ones that never
// triggered — gives an honest hit-rate for discretionary structure
// reads, free of survivorship bias.

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"myFirstGo/trading-bot/market"

	"github.com/gin-gonic/gin"
)

// Setup is one recorded structure hypothesis + its (later) outcome.
type Setup struct {
	ID         int       `json:"id"`
	RecordedAt time.Time `json:"recorded_at"`
	Symbol     string    `json:"symbol"`
	TF         string    `json:"tf"`

	// Snapshot at record time.
	Price     float64 `json:"price"`
	Dir       string  `json:"dir"` // long / short / neutral
	Lean      string  `json:"lean"`
	Trend     string  `json:"trend"`
	Event     string  `json:"event"`
	InZone    bool    `json:"in_zone"`
	Forming   bool    `json:"forming"`
	Entry     float64 `json:"entry"`
	Stop      float64 `json:"stop"`
	Target    float64 `json:"target"`
	BOSLevel  float64 `json:"bos_level"`
	Protected float64 `json:"protected"`

	EngineSide    string `json:"engine_side"`
	EngineScore   int    `json:"engine_score"`
	EngineVerdict string `json:"engine_verdict"`

	Opened bool   `json:"opened"` // did you actually open a trade on it?
	Note   string `json:"note"`

	// Outcome — backfilled by /api/setups/refresh.
	Outcome      string    `json:"outcome"` // "" open | bos | choch | expired
	OutcomeAt    time.Time `json:"outcome_at,omitempty"`
	OutcomePrice float64   `json:"outcome_price,omitempty"`
	Bars         int       `json:"bars,omitempty"`
}

// setupsPath is the on-disk JSONL store (one Setup per line).
func setupsPath() string {
	if p := os.Getenv("SETUPS_PATH"); p != "" {
		return p
	}
	return "/opt/trading/setups.jsonl"
}

func readSetups() ([]Setup, error) {
	f, err := os.Open(setupsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Setup
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var s Setup
		if err := json.Unmarshal(line, &s); err == nil {
			out = append(out, s)
		}
	}
	return out, sc.Err()
}

func writeSetups(all []Setup) error {
	tmp := setupsPath() + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, s := range all {
		b, _ := json.Marshal(s)
		w.Write(b)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, setupsPath())
}

func appendSetup(s Setup) (int, error) {
	all, err := readSetups()
	if err != nil {
		return 0, err
	}
	maxID := 0
	for _, x := range all {
		if x.ID > maxID {
			maxID = x.ID
		}
	}
	s.ID = maxID + 1
	all = append(all, s)
	if err := writeSetups(all); err != nil {
		return 0, err
	}
	return s.ID, nil
}

// classifyOutcome scans closed bars recorded AFTER the setup and returns
// the first structural resolution: a close through BOSLevel in the
// setup direction = "bos" (continuation confirmed); a close through
// Protected against it = "choch" (voided). >= expireBars with neither =
// "expired". Empty = still open.
func classifyOutcome(su Setup, closed []market.Candle) (string, time.Time, float64, int) {
	const expireBars = 20
	n := 0
	for _, c := range closed {
		if !c.CloseTime.After(su.RecordedAt) {
			continue
		}
		n++
		if su.Dir == "short" {
			if su.BOSLevel > 0 && c.Close < su.BOSLevel {
				return "bos", c.CloseTime, c.Close, n
			}
			if su.Protected > 0 && c.Close > su.Protected {
				return "choch", c.CloseTime, c.Close, n
			}
		} else { // long / neutral default
			if su.BOSLevel > 0 && c.Close > su.BOSLevel {
				return "bos", c.CloseTime, c.Close, n
			}
			if su.Protected > 0 && c.Close < su.Protected {
				return "choch", c.CloseTime, c.Close, n
			}
		}
	}
	if n >= expireBars {
		return "expired", time.Time{}, 0, n
	}
	return "", time.Time{}, 0, n
}

// handleAPIChartSetupRecord — POST /api/chart/setup. Body is the
// snapshot the /chart client assembled from the live structure. Assigns
// an ID and appends. Outcome stays empty until a refresh resolves it.
func (s *server) handleAPIChartSetupRecord(c *gin.Context) {
	var in Setup
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	in.RecordedAt = time.Now()
	in.Outcome = ""
	id, err := appendSetup(in)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "id": id})
}

// handleAPISetupsRefresh — POST /api/setups/refresh. For every still-open
// setup, fetch that symbol/TF's recent closed bars and backfill the
// outcome. Returns how many were resolved.
func (s *server) handleAPISetupsRefresh(c *gin.Context) {
	all, err := readSetups()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ctx := c.Request.Context()
	now := time.Now()
	resolved := 0
	for i := range all {
		if all[i].Outcome != "" {
			continue
		}
		sym, err := resolveWebSymbol(all[i].Symbol)
		if err != nil {
			continue
		}
		tf := market.Timeframe(all[i].TF)
		cs, err := s.client.KlinesWithForming(ctx, sym, tf, 200)
		if err != nil || len(cs) == 0 {
			continue
		}
		// Keep only closed bars (drop the still-forming last bar).
		closed := cs[:0:0]
		for _, k := range cs {
			if k.CloseTime.Before(now) {
				closed = append(closed, k)
			}
		}
		oc, at, px, bars := classifyOutcome(all[i], closed)
		if oc != "" {
			all[i].Outcome, all[i].OutcomeAt, all[i].OutcomePrice, all[i].Bars = oc, at, px, bars
			resolved++
		}
	}
	if resolved > 0 {
		if err := writeSetups(all); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "resolved": resolved})
}

// handleSetupSkip — POST /setups/:id/skip. Marks a setup as "skip"
// (you decided NOT to take it). It's a terminal state: drops out of the
// BOS/CHoCH hit-rate and won't be touched by refresh, but stays logged
// as an observed-but-passed sample.
func (s *server) handleSetupSkip(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	all, err := readSetups()
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	for i := range all {
		if all[i].ID == id {
			all[i].Outcome = "skip"
		}
	}
	_ = writeSetups(all)
	c.Redirect(http.StatusSeeOther, "/setups")
}

// handleSetupDelete — POST /setups/:id/delete.
func (s *server) handleSetupDelete(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	all, err := readSetups()
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	out := all[:0]
	for _, x := range all {
		if x.ID != id {
			out = append(out, x)
		}
	}
	_ = writeSetups(out)
	c.Redirect(http.StatusSeeOther, "/setups")
}

// setupStats is the hit-rate rollup shown atop /setups.
type setupStats struct {
	Total, Open, BOS, CHoCH, Expired, Skip int
	HitRate                                float64 // bos / (bos+choch)
}

func rollupSetups(all []Setup) setupStats {
	var st setupStats
	st.Total = len(all)
	for _, s := range all {
		switch s.Outcome {
		case "":
			st.Open++
		case "bos":
			st.BOS++
		case "choch":
			st.CHoCH++
		case "expired":
			st.Expired++
		case "skip":
			st.Skip++
		}
	}
	if d := st.BOS + st.CHoCH; d > 0 {
		st.HitRate = float64(st.BOS) / float64(d) * 100
	}
	return st
}

// handleSetupsList — GET /setups. Newest first + hit-rate rollup.
func (s *server) handleSetupsList(c *gin.Context) {
	all, err := readSetups()
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID > all[j].ID })
	c.Header("Cache-Control", "no-store")
	c.HTML(http.StatusOK, "setups.html", gin.H{
		"Setups": all,
		"Stats":  rollupSetups(all),
	})
}
