package autotrade

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"time"
)

// PaperFire is one paper-mode auto-trade the executor WOULD have placed. Appended
// to a JSONL log so the /ops autotrade panel can show what the executor is doing
// without any real orders.
type PaperFire struct {
	Time   time.Time `json:"time"`
	Symbol string    `json:"symbol"`
	Side   string    `json:"side"`
	Entry  float64   `json:"entry"`
	Stop   float64   `json:"stop"`
	TP     float64   `json:"tp"`
	Qty    float64   `json:"qty"`
	Margin float64   `json:"margin"`
	Lev    int       `json:"lev"`
	Why    string    `json:"why"`
	Live   bool      `json:"live"` // false = paper, true = real order placed
}

// PaperLogPath is the on-disk JSONL store (env AUTOTRADE_PAPER_LOG or default).
func PaperLogPath() string {
	if p := strings.TrimSpace(os.Getenv("AUTOTRADE_PAPER_LOG")); p != "" {
		return p
	}
	return "/opt/trading/autotrade_paper.jsonl"
}

// AppendFire appends one fire to the paper log (best-effort; never fatal).
func AppendFire(pf PaperFire) {
	f, err := os.OpenFile(PaperLogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	b, err := json.Marshal(pf)
	if err != nil {
		return
	}
	f.Write(append(b, '\n'))
}

// ReadFires returns the most recent `limit` fires (newest first).
func ReadFires(limit int) []PaperFire {
	f, err := os.Open(PaperLogPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	var all []PaperFire
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var pf PaperFire
		if json.Unmarshal([]byte(line), &pf) == nil {
			all = append(all, pf)
		}
	}
	_ = sc.Err()
	// newest first, cap
	out := make([]PaperFire, 0, limit)
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, all[i])
	}
	return out
}
