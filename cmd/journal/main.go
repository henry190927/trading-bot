// Command journal records opened/closed live trades to a CSV log, so live
// outcomes can be reconciled against backtest expectations.
//
// All data-layer logic lives in package trading-bot/journal. This main only
// handles CLI parsing, output formatting, and color.
package main

import (
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/henry190927/trading-bot/ansi"
	"github.com/henry190927/trading-bot/journal"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "open":
		err = cmdOpen(args)
	case "close":
		err = cmdClose(args)
	case "list":
		err = cmdList(args)
	case "stats":
		err = cmdStats(args)
	case "update":
		err = cmdUpdate(args)
	case "delete":
		err = cmdDelete(args)
	case "anchors":
		err = cmdAnchors()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  journal open [--at <spec>] [--score <val>] SYMBOL SIDE ENTRY STOP TP1 TP2 ANCHOR TF [notes...]
    e.g. journal open --score=3 XAG short 77.49 78.16 76.81 76.14 sweep-high 15m,1h "RSI div"
         journal open --score=v7.5 BTC long 75000 74500 75500 76000 sweep-low 1h "validate confirmed"

  journal close ID|SYMBOL OUTCOME EXIT_PRICE [notes...]
    OUTCOME: tp1, tp2, stop, manual, timeout

  journal list [N]                  show last N entries (default 20)
  journal stats                     aggregate WR / avgR / by-symbol / by-tf / by-score / by-anchor

  journal update ID field=value ...
    editable: opened_at, analyzed_at, closed_at, side, tf, score, entry, stop,
              tp1, tp2, anchor, open_notes, exit_price, outcome, close_notes
    r_realized auto-recomputed if closed.

  journal delete ID
  journal anchors                   print recommended ANCHOR labels

Options (before positional args):
  --at <spec>      analyzed_at. Spec: now | -2h/-30m/-1d | YYYY-MM-DD HH:MM |
                                       YYYY-MM-DDTHH:MM[:SS][Z|+08:00]
  --score <val>    score from analyze/validate (e.g. "3", "v7.5", "3,v7.5", "manual")

Storage path: $JOURNAL_PATH (default ./journal.csv)`)
}

// --- open ----------------------------------------------------------------

func cmdOpen(args []string) error {
	atSpec, args := extractFlag(args, "at")
	scoreFlag, args := extractFlag(args, "score")
	if len(args) < 8 {
		usage()
		return errors.New("journal open requires SYMBOL SIDE ENTRY STOP TP1 TP2 ANCHOR TF [notes...]")
	}
	now := time.Now().UTC()
	analyzedAt, err := journal.ParseTimeSpec(atSpec, now)
	if err != nil {
		return err
	}
	symbol := strings.ToUpper(args[0])
	side := strings.ToLower(args[1])
	if side != "long" && side != "short" {
		return fmt.Errorf("side must be long or short, got %q", side)
	}
	entry, err := strconv.ParseFloat(args[2], 64)
	if err != nil {
		return fmt.Errorf("entry: %w", err)
	}
	stop, err := strconv.ParseFloat(args[3], 64)
	if err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	tp1, err := strconv.ParseFloat(args[4], 64)
	if err != nil {
		return fmt.Errorf("tp1: %w", err)
	}
	tp2, err := strconv.ParseFloat(args[5], 64)
	if err != nil {
		return fmt.Errorf("tp2: %w", err)
	}
	anchor := args[6]
	tf := args[7]
	notes := strings.Join(args[8:], " ")

	trades, err := journal.ReadAll("")
	if err != nil {
		return err
	}
	t := journal.Trade{
		ID:         journal.NextID(trades),
		OpenedAt:   now,
		AnalyzedAt: analyzedAt,
		Symbol:     symbol,
		Side:       side,
		TF:         tf,
		Score:      scoreFlag,
		Entry:      entry,
		Stop:       stop,
		TP1:        tp1,
		TP2:        tp2,
		Anchor:     anchor,
		OpenNotes:  notes,
	}
	trades = append(trades, t)
	if err := journal.WriteAll("", trades); err != nil {
		return err
	}
	fmt.Printf("opened #%d  %s %s @ %.4f  stop=%.4f  TP1=%.4f  TP2=%.4f  tf=%s\n",
		t.ID, ansi.Wrap(t.Symbol, ansi.BoldC), colorSide(side), t.Entry, t.Stop, t.TP1, t.TP2, t.TF)
	return nil
}

// --- close ---------------------------------------------------------------

func cmdClose(args []string) error {
	if len(args) < 3 {
		usage()
		return errors.New("journal close requires ID|SYMBOL OUTCOME EXIT_PRICE [notes...]")
	}
	idOrSym := args[0]
	outcome := strings.ToLower(args[1])
	exit, err := strconv.ParseFloat(args[2], 64)
	if err != nil {
		return fmt.Errorf("exit_price: %w", err)
	}
	notes := strings.Join(args[3:], " ")
	if !journal.IsCloseOutcome(outcome) {
		return fmt.Errorf("outcome must be %s, got %q", journal.OutcomeList(journal.CloseOutcomes), outcome)
	}

	trades, err := journal.ReadAll("")
	if err != nil {
		return err
	}
	var idx int
	if id, err := strconv.Atoi(idOrSym); err == nil {
		idx = journal.FindByID(trades, id)
		if idx >= 0 && !trades[idx].IsOpen() {
			idx = -1
		}
	} else {
		idx = journal.FindOpenBySymbol(trades, idOrSym)
	}
	if idx < 0 {
		return fmt.Errorf("no open trade matching %q", idOrSym)
	}
	t := &trades[idx]
	t.ClosedAt = time.Now().UTC()
	t.ExitPrice = exit
	t.Outcome = outcome
	t.CloseNotes = notes
	t.RRealized = journal.RealizedR(*t, exit)

	if err := journal.WriteAll("", trades); err != nil {
		return err
	}
	rCol := ansi.Green
	if t.RRealized < 0 {
		rCol = ansi.Red
	}
	fmt.Printf("closed #%d  %s %s @ %.4f → %s (%s)  exit=%.4f\n",
		t.ID, ansi.Wrap(t.Symbol, ansi.BoldC), colorSide(t.Side), t.Entry,
		ansi.Wrap(outcome, ansi.Bold), ansi.Wrap(fmt.Sprintf("%+.2fR", t.RRealized), rCol), exit)
	return nil
}

// --- update + delete -----------------------------------------------------

func cmdUpdate(args []string) error {
	if len(args) < 2 {
		usage()
		return errors.New("journal update requires ID and at least one field=value pair")
	}
	id, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("invalid ID %q: %w", args[0], err)
	}
	trades, err := journal.ReadAll("")
	if err != nil {
		return err
	}
	idx := journal.FindByID(trades, id)
	if idx < 0 {
		return fmt.Errorf("no trade with id %d", id)
	}
	t := &trades[idx]
	changes := make([]string, 0, len(args)-1)
	for _, kv := range args[1:] {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("expected field=value, got %q", kv)
		}
		field := strings.ToLower(parts[0])
		val := parts[1]
		if err := journal.ApplyUpdate(t, field, val); err != nil {
			return err
		}
		changes = append(changes, fmt.Sprintf("%s=%s", field, val))
	}
	if !t.IsOpen() {
		t.RRealized = journal.RealizedR(*t, t.ExitPrice)
	}
	if err := journal.WriteAll("", trades); err != nil {
		return err
	}
	fmt.Printf("updated #%d %s: %s\n", t.ID, ansi.Wrap(t.Symbol, ansi.BoldC), strings.Join(changes, ", "))
	if !t.IsOpen() {
		fmt.Printf("  realized R recomputed: %+.2fR\n", t.RRealized)
	}
	return nil
}

func cmdDelete(args []string) error {
	if len(args) < 1 {
		usage()
		return errors.New("journal delete requires ID")
	}
	id, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("invalid ID %q: %w", args[0], err)
	}
	trades, err := journal.ReadAll("")
	if err != nil {
		return err
	}
	out := make([]journal.Trade, 0, len(trades))
	deleted := false
	for _, t := range trades {
		if t.ID == id {
			deleted = true
			continue
		}
		out = append(out, t)
	}
	if !deleted {
		return fmt.Errorf("no trade with id %d", id)
	}
	if err := journal.WriteAll("", out); err != nil {
		return err
	}
	fmt.Printf("deleted #%d\n", id)
	return nil
}

// --- list + stats --------------------------------------------------------

func cmdList(args []string) error {
	n := 20
	if len(args) > 0 {
		if v, err := strconv.Atoi(args[0]); err == nil && v > 0 {
			n = v
		}
	}
	trades, err := journal.ReadAll("")
	if err != nil {
		return err
	}
	if len(trades) == 0 {
		fmt.Println("(no trades recorded)")
		return nil
	}
	journal.SortByOpenedDesc(trades)
	if len(trades) > n {
		trades = trades[:n]
	}

	hdr := fmt.Sprintf("%-4s %-17s %-17s %-17s %-6s %-5s %-8s %-7s %-10s %-10s %-10s %-10s %-10s %-8s %-8s %s",
		"ID", "ANALYZED", "OPENED", "CLOSED", "SYM", "SIDE", "TF", "SCORE", "ENTRY", "STOP", "TP1", "TP2", "EXIT", "R", "STATUS", "ANCHOR")
	fmt.Println(ansi.Wrap(hdr, ansi.Bold))
	fmt.Println(strings.Repeat("-", 190))
	for _, t := range trades {
		opened := t.OpenedAt.Local().Format("2006-01-02 15:04")
		analyzed := opened
		if !t.AnalyzedAt.IsZero() {
			analyzed = t.AnalyzedAt.Local().Format("2006-01-02 15:04")
		}
		closedStr := "-"
		status := "open"
		rStr := "-"
		exit := "-"
		if !t.IsOpen() {
			closedStr = t.ClosedAt.Local().Format("2006-01-02 15:04")
			status = t.Outcome
			rStr = fmt.Sprintf("%+.2f", t.RRealized)
			exit = fmt.Sprintf("%.4f", t.ExitPrice)
		}
		rCol := ansi.Dim
		switch {
		case t.RRealized > 0:
			rCol = ansi.Green
		case t.RRealized < 0:
			rCol = ansi.Red
		}
		statCol := ansi.Dim
		switch status {
		case "tp1", "tp2":
			statCol = ansi.BoldG
		case "stop", "manual":
			statCol = ansi.Red
		case "liquidated":
			statCol = ansi.BoldR
		case "open":
			statCol = ansi.Yellow
		}
		score := t.Score
		if score == "" {
			score = "-"
		}
		fmt.Printf("%-4d %-17s %-17s %-17s %-6s %s %-8s %-7s %-10s %-10s %-10s %-10s %-10s %s %s %s\n",
			t.ID, analyzed, opened, closedStr, t.Symbol,
			ansi.PadR(colorSide(t.Side), 5), t.TF, score,
			fmt.Sprintf("%.4f", t.Entry),
			fmt.Sprintf("%.4f", t.Stop),
			fmt.Sprintf("%.4f", t.TP1),
			fmt.Sprintf("%.4f", t.TP2),
			exit,
			ansi.PadR(ansi.Wrap(rStr, rCol), 8),
			ansi.PadR(ansi.Wrap(status, statCol), 8),
			t.Anchor)
	}
	return nil
}

func cmdStats(_ []string) error {
	trades, err := journal.ReadAll("")
	if err != nil {
		return err
	}
	closed := make([]journal.Trade, 0)
	for _, t := range trades {
		if !t.IsOpen() {
			closed = append(closed, t)
		}
	}
	fmt.Println(ansi.Wrap("=== Live journal stats ===", ansi.BoldC))
	fmt.Printf("Total trades: %d  (closed: %d, open: %d)\n", len(trades), len(closed), len(trades)-len(closed))
	if len(closed) == 0 {
		return nil
	}
	printGroup("ALL", closed)

	for _, g := range []struct {
		label string
		key   func(journal.Trade) string
	}{
		{"By symbol", func(t journal.Trade) string { return t.Symbol }},
		{"By timeframe analyzed", func(t journal.Trade) string { return t.TF }},
		{"By score", func(t journal.Trade) string {
			if t.Score == "" {
				return "(none)"
			}
			return t.Score
		}},
		{"By anchor", func(t journal.Trade) string { return t.Anchor }},
	} {
		fmt.Println(ansi.Wrap("\n"+g.label+":", ansi.Bold))
		groups := groupBy(closed, g.key)
		keys := make([]string, 0, len(groups))
		for k := range groups {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			printGroup(k, groups[k])
		}
	}
	return nil
}

func groupBy(trades []journal.Trade, key func(journal.Trade) string) map[string][]journal.Trade {
	m := map[string][]journal.Trade{}
	for _, t := range trades {
		m[key(t)] = append(m[key(t)], t)
	}
	return m
}

func printGroup(label string, ts []journal.Trade) {
	var totalR, bestR, worstR float64
	wins := 0
	bestR = -math.MaxFloat64
	worstR = math.MaxFloat64
	n := 0
	for _, t := range ts {
		// Skip trades with no R (no stop recorded). RealizedR returns 0 for
		// them, and summing that zero states they broke even.
		if !t.HasR() {
			continue
		}
		n++
		totalR += t.RRealized
		if t.RRealized > 0 {
			wins++
		}
		if t.RRealized > bestR {
			bestR = t.RRealized
		}
		if t.RRealized < worstR {
			worstR = t.RRealized
		}
	}
	if n == 0 {
		fmt.Printf("%s: %d trades, none with a recorded stop — no R to report\n", label, len(ts))
		return
	}
	wr := float64(wins) / float64(n) * 100
	avgR := totalR / float64(n)
	avgCol := ansi.Dim
	switch {
	case avgR > 0:
		avgCol = ansi.Green
	case avgR < 0:
		avgCol = ansi.Red
	}
	// n, not len(ts): the count shown has to be the denominator the WR and
	// avgR beside it were actually divided by, or the line silently disagrees
	// with itself the moment one trade has no stop.
	note := ""
	if skipped := len(ts) - n; skipped > 0 {
		note = fmt.Sprintf("  (+%d no-stop)", skipped)
	}
	fmt.Printf("  %-20s n=%-3d  WR=%5.1f%%  avgR=%s  totalR=%+6.2f  best=%+.2f worst=%+.2f%s\n",
		label, n, wr,
		ansi.Wrap(fmt.Sprintf("%+5.2f", avgR), avgCol),
		totalR, bestR, worstR, note)
}

// --- anchors helper ------------------------------------------------------

func cmdAnchors() error {
	fmt.Println(`Recommended ANCHOR values (free-form; jstats groups by exact string):

  sweep-low        Long after a low-side liquidity grab + reclaim
  sweep-high       Short after a high-side grab + reclaim
  fib-uptrend      Long at fib 0.618 in an uptrend
  fib-downtrend    Short at fib 0.618 in a downtrend
  boll-lower       Mean-reversion long at the lower Bollinger band
  boll-upper       Mean-reversion short at the upper Bollinger band
  hvn-support      Bounce off a non-POC HVN from above (long)
  hvn-resistance   Rejection at a non-POC HVN from below (short)
  daily-open       Reaction at the daily UTC open
  weekly-open      Reaction at the weekly UTC open (Monday 00:00 UTC)
  discretionary    Entry not tied to a specific strategy signal
  manual           Same as discretionary

Use lowercase, hyphen-separated. Mixed case / typos create separate groups in jstats.`)
	return nil
}

// --- helpers --------------------------------------------------------------

func extractFlag(args []string, name string) (string, []string) {
	out := make([]string, 0, len(args))
	val := ""
	prefix := "--" + name + "="
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, prefix) {
			val = strings.TrimPrefix(a, prefix)
			continue
		}
		if a == "--"+name && i+1 < len(args) {
			val = args[i+1]
			i++
			continue
		}
		out = append(out, a)
	}
	return val, out
}

func colorSide(s string) string {
	switch s {
	case "long":
		return ansi.Wrap("LONG", ansi.BoldG)
	case "short":
		return ansi.Wrap("SHORT", ansi.BoldR)
	}
	return ansi.Wrap(strings.ToUpper(s), ansi.Dim)
}
