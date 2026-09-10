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
	"context"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"myFirstGo/trading-bot/market"
	"myFirstGo/trading-bot/signal"

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

	// Strategy tagging (auto, server-side at record time). Strategy = which
	// engine is ACTIVE for this (symbol,TF) per signal.StrategyFor ("mr" /
	// "struct-momentum"). StratFire = whether that active strategy produced a
	// non-Flat signal on the last closed bar at record time; StratSide = its
	// side. Purpose: filter StructMomentum forward-log rows to ACTUAL fires —
	// a neutral-structure snapshot where SM's regime gate sits out records
	// StratFire=false and must not count as an SM data point.
	Strategy  string `json:"strategy,omitempty"`
	StratFire bool   `json:"strat_fire,omitempty"`
	StratSide string `json:"strat_side,omitempty"`

	Opened bool   `json:"opened"` // did you actually open a trade on it?
	Note   string `json:"note"`

	// Outcome — backfilled by /api/setups/refresh. CLOSE-based: did the
	// setup's read resolve to the win direction (a close through target)
	// before a close through stop. Measures "was the direction right".
	Outcome      string    `json:"outcome"` // "" open | win | loss | expired | skip
	OutcomeAt    time.Time `json:"outcome_at,omitempty"`
	OutcomePrice float64   `json:"outcome_price,omitempty"`
	Bars         int       `json:"bars,omitempty"`

	// RealOutcome — PATH-DEPENDENT: walks intrabar high/low and reports
	// which of stop/target was touched FIRST after the entry filled (same
	// bar → stop first, the conservative backtest convention). Answers "if
	// actually traded this entry/stop, would it have hit TP or SL first" —
	// which Outcome misses when a wick sweeps the stop before price later
	// reaches target. The gap between Outcome and RealOutcome = the
	// execution / stop-placement quality signal (e.g. read right, stop too
	// tight). "no-fill" = entry never reached within the window.
	RealOutcome      string    `json:"real_outcome,omitempty"` // "" | win | loss | no-fill | expired
	RealOutcomeAt    time.Time `json:"real_outcome_at,omitempty"`
	RealOutcomePrice float64   `json:"real_outcome_price,omitempty"`
	RealBars         int       `json:"real_bars,omitempty"`
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
// the first resolution IN THE SETUP'S DIRECTION using its own stop /
// target (which the recorder assembled dir-correctly) — not the raw
// structure leg levels, which can point the wrong way for a CHoCH
// reversal setup. A close through the target = "win"; a close through
// the stop = "loss"; >= expireBars with neither = "expired"; empty =
// still open (or no target/stop and never stopped).
// filledOn is the SINGLE definition of "the resting order got taken": a bar
// whose range spans the entry. Shared by both classifiers on purpose.
//
// classifyOutcome had no fill test at all — it walked bars asking only whether
// a CLOSE had passed stop or target. For a limit that never filled that
// fabricates a verdict, and on 2026-09-10 three of 52 setups carried one:
// id=40 SNDK read "win" on a long at 1512 that price left behind at 1545 and
// never returned to, and id=20/id=27 read "loss" the same way. The page
// invites the reader to treat the Hit-rate / Real-hit-rate GAP as execution
// quality; 1.5 of the 9.2pp gap was instead one column counting trades that
// never happened. With the gate shared the gap is purely close-vs-wick, which
// is the only thing it ever claimed to measure.
func filledOn(c market.Candle, entry float64) bool {
	return entry > 0 && c.Low <= entry && entry <= c.High
}

func classifyOutcome(su Setup, closed []market.Candle) (string, time.Time, float64, int) {
	const expireBars = 20
	const winR = 2.0 // R-based win when no explicit target: closed through +2R
	var risk float64
	if su.Entry > 0 && su.Stop > 0 {
		risk = math.Abs(su.Entry - su.Stop)
	}
	if su.Entry <= 0 {
		// Without an entry neither classifier can say anything; matching
		// classifyRealOutcome's guard keeps the two populations identical.
		return "", time.Time{}, 0, 0
	}
	n := 0
	filled := false
	for _, c := range closed {
		if !c.CloseTime.After(su.RecordedAt) {
			continue
		}
		n++
		if !filled {
			if !filledOn(c, su.Entry) {
				continue
			}
			filled = true
		}
		if su.Dir == "short" {
			if su.Stop > 0 && c.Close >= su.Stop {
				return "loss", c.CloseTime, c.Close, n
			}
			if su.Target > 0 && c.Close <= su.Target {
				return "win", c.CloseTime, c.Close, n
			}
			if su.Target == 0 && risk > 0 && c.Close <= su.Entry-winR*risk {
				return "win", c.CloseTime, c.Close, n
			}
		} else { // long / neutral
			if su.Stop > 0 && c.Close <= su.Stop {
				return "loss", c.CloseTime, c.Close, n
			}
			if su.Target > 0 && c.Close >= su.Target {
				return "win", c.CloseTime, c.Close, n
			}
			if su.Target == 0 && risk > 0 && c.Close >= su.Entry+winR*risk {
				return "win", c.CloseTime, c.Close, n
			}
		}
	}
	if !filled && n >= expireBars {
		return "no-fill", time.Time{}, 0, n
	}
	if n >= expireBars {
		return "expired", time.Time{}, 0, n
	}
	return "", time.Time{}, 0, n
}

// classifyRealOutcome is the PATH-DEPENDENT twin of classifyOutcome: it
// walks intrabar high/low and reports which of stop/target was touched
// FIRST after the entry filled — the outcome an actual resting order would
// have seen. Needs entry + stop. Fill = a bar whose range spans the entry.
// Same-bar stop AND target → stop first (conservative). Never fills within
// the window → "no-fill".
func classifyRealOutcome(su Setup, closed []market.Candle) (string, time.Time, float64, int) {
	const expireBars = 20
	if su.Entry <= 0 || su.Stop <= 0 {
		return "", time.Time{}, 0, 0
	}
	filled := false
	n := 0
	for _, c := range closed {
		if !c.CloseTime.After(su.RecordedAt) {
			continue
		}
		n++
		if !filled {
			if !filledOn(c, su.Entry) {
				continue
			}
			filled = true // price traded through the entry this bar
		}
		// First-touch after fill (incl. the fill bar). Check stop before
		// target so a bar that spans both resolves to the stop.
		if su.Dir == "short" {
			if c.High >= su.Stop {
				return "loss", c.CloseTime, su.Stop, n
			}
			if su.Target > 0 && c.Low <= su.Target {
				return "win", c.CloseTime, su.Target, n
			}
		} else { // long / neutral
			if c.Low <= su.Stop {
				return "loss", c.CloseTime, su.Stop, n
			}
			if su.Target > 0 && c.High >= su.Target {
				return "win", c.CloseTime, su.Target, n
			}
		}
	}
	if !filled && n >= expireBars {
		return "no-fill", time.Time{}, 0, n
	}
	if filled && n >= expireBars {
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

	// Auto-tag the ACTIVE strategy for this (symbol,TF) and whether it actually
	// fires now — server-side + authoritative (don't trust the client). Lets
	// the forward-log distinguish a real StructMomentum fire from a neutral
	// structure snapshot the strategy would sit out. Best-effort: on any fetch
	// error we leave the tags empty rather than fail the record.
	if sym, rerr := resolveWebSymbol(in.Symbol); rerr == nil && s.client != nil {
		tf := market.Timeframe(in.TF)
		in.Strategy = signal.StrategyFor(sym, tf).String()
		fctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
		if cs, kerr := s.client.Klines(fctx, sym, tf, 250); kerr == nil && len(cs) >= 60 {
			sig := signal.Evaluate(signal.Inputs{Symbol: sym, Timeframe: tf, Candles: cs})
			in.StratFire = sig.Side != signal.Flat
			in.StratSide = sig.Side.String()
		}
		cancel()
	}

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
		if all[i].Outcome == "skip" {
			continue // skip is a terminal user decision — never auto-judge it
		}
		sym, err := resolveWebSymbol(all[i].Symbol)
		if err != nil {
			continue
		}
		tf := market.Timeframe(all[i].TF)
		// Fetch the setup's OWN forward window (recorded → +~25 bars) rather
		// than the recent 200, so an OLD setup resolves on the bars that
		// actually followed it — required for the path-dependent RealOutcome
		// (which of stop/target got wicked first) and also fixes stale
		// re-judging of old Outcomes on unrelated recent price.
		start := all[i].RecordedAt
		end := start.Add(25 * tfDurationSeconds(tf))
		if end.After(now) {
			end = now
		}
		cs, err := s.client.KlinesRange(ctx, sym, tf, start, end)
		if err != nil || len(cs) == 0 {
			continue
		}
		// Keep only closed bars.
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
		// Path-dependent twin — resolves independently of Outcome.
		roc, rat, rpx, rbars := classifyRealOutcome(all[i], closed)
		if roc != "" {
			all[i].RealOutcome, all[i].RealOutcomeAt, all[i].RealOutcomePrice, all[i].RealBars = roc, rat, rpx, rbars
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
	Total, Open, Win, Loss, Expired, Skip int
	HitRate                               float64 // win / (win+loss), close-based
	// Path-dependent (real) tallies — what an actual entry/stop would have
	// seen. RealNoFill = entry never reached. The gap RealHitRate vs
	// HitRate = the execution / stop-quality signal.
	RealWin, RealLoss, RealNoFill int
	RealHitRate                   float64 // realWin / (realWin+realLoss)
}

func rollupSetups(all []Setup) setupStats {
	var st setupStats
	st.Total = len(all)
	for _, s := range all {
		switch s.Outcome {
		case "":
			st.Open++
		case "win":
			st.Win++
		case "loss":
			st.Loss++
		case "expired":
			st.Expired++
		case "skip":
			st.Skip++
		}
		switch s.RealOutcome {
		case "win":
			st.RealWin++
		case "loss":
			st.RealLoss++
		case "no-fill":
			st.RealNoFill++
		}
	}
	if d := st.Win + st.Loss; d > 0 {
		st.HitRate = float64(st.Win) / float64(d) * 100
	}
	if d := st.RealWin + st.RealLoss; d > 0 {
		st.RealHitRate = float64(st.RealWin) / float64(d) * 100
	}
	return st
}

// engineStance classifies whether the engine agreed with the setup's
// direction at record time: agree (same side), against (opposite), or
// flat (engine had no directional read).
func engineStance(s Setup) string {
	es := strings.ToUpper(strings.TrimSpace(s.EngineSide))
	if es == "" || es == "FLAT" {
		return "flat"
	}
	if es == strings.ToUpper(s.Dir) {
		return "agree"
	}
	return "against"
}

// regimeOf buckets a setup by market regime from its recorded trend:
// a confirmed HH-HL / LH-LL sequence = "trend", anything else = "neutral".
func regimeOf(s Setup) string {
	if strings.Contains(s.Trend, "uptrend") || strings.Contains(s.Trend, "downtrend") {
		return "trend"
	}
	return "neutral"
}

// SliceCell is one engine-stance × regime bucket's win/loss tally.
type SliceCell struct {
	Win, Loss, N int
	HitRate      float64 // win / (win+loss)
}

// regimeRollup answers "when the engine disagrees with N-struct, does
// following N-struct pay off — and does it depend on regime?" It tallies
// resolved (win/loss) setups into a 3×2 grid: stance {agree,against,flat}
// × regime {neutral,trend}.
func regimeRollup(all []Setup) map[string]map[string]SliceCell {
	out := map[string]map[string]SliceCell{
		"agree":   {"neutral": {}, "trend": {}},
		"against": {"neutral": {}, "trend": {}},
		"flat":    {"neutral": {}, "trend": {}},
	}
	for _, s := range all {
		st, rg := engineStance(s), regimeOf(s)
		c := out[st][rg]
		switch s.Outcome {
		case "win":
			c.Win++
			c.N++
		case "loss":
			c.Loss++
			c.N++
		}
		out[st][rg] = c
	}
	for st := range out {
		for rg, c := range out[st] {
			if d := c.Win + c.Loss; d > 0 {
				c.HitRate = float64(c.Win) / float64(d) * 100
			}
			out[st][rg] = c
		}
	}
	return out
}

// handleTipsPage — GET /tips. Static reference: expandable trading-tip
// cards (bilingual via the i18n .i18n-en/.i18n-zh CSS toggle).
func (s *server) handleTipsPage(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.HTML(http.StatusOK, "tips.html", gin.H{})
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
		"Regime": regimeRollup(all),
	})
}
