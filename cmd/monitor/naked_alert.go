package main

// Rendering for the recurring unprotected-position alarm.
//
// This used to be one fixed string. A naked BTC long produced 36 byte-identical
// pushes over nine hours on 2026-09-18, and a naked XRP long produced 132 over
// six on 2026-09-21. Detection fired five seconds after the fill both times and
// no push ever failed, so neither incident was a monitoring failure — every
// notification after the first simply carried nothing the previous one had not,
// and a tray full of identical rows is a tray you stop reading.
//
// So everything here either CHANGES between pushes or says why the alarm
// exists. Elapsed time and nag count go in the title, because the title is the
// line a phone shows collapsed: two adjacent notifications that differ only
// below the fold are two identical notifications.
//
// Degrades rather than fails. Mark price and equity are separate reads that can
// time out, and an alarm suppressed because a secondary lookup failed would be
// the worst possible outcome — the lines they feed are dropped, the alarm still
// goes.

import (
	"fmt"
	"strings"
	"time"
)

type nakedAlert struct {
	Short, Side string
	Qty, Entry  float64
	Mark        float64 // 0 = unreadable
	Leverage    int
	MarginMode  string
	Equity      float64 // 0 = unreadable
	Naked       time.Duration
	Nag         int  // 1 on the first alarm
	Journalled  bool // a row exists, but with no analysis stop
}

// ref is the price to measure exposure at: the mark when we have it, the entry
// otherwise. Using entry when the position has run is how a 4,764u alarm keeps
// reporting 4,764u after exposure has grown.
func (a nakedAlert) ref() float64 {
	if a.Mark > 0 {
		return a.Mark
	}
	return a.Entry
}

func (a nakedAlert) notional() float64 { return a.Qty * a.ref() }

func (a nakedAlert) unrealised() float64 {
	if a.Mark <= 0 {
		return 0
	}
	d := (a.Mark - a.Entry) * a.Qty
	if a.Side == "short" {
		return -d
	}
	return d
}

// killFrac is the adverse move that exhausts equity under cross margin. An
// estimate — it ignores maintenance margin, so the real liquidation is slightly
// nearer — but it matched NEAR's actual 2026-09-18 liquidation to within 0.06%,
// and the number's job is to convey scale, not to be a trigger.
func (a nakedAlert) killFrac() float64 {
	n := a.notional()
	if a.Equity <= 0 || n <= 0 {
		return 0
	}
	return a.Equity / n
}

func (a nakedAlert) killPrice() float64 {
	f := a.killFrac()
	if f == 0 {
		return 0
	}
	if a.Side == "short" {
		return a.ref() * (1 + f)
	}
	return a.ref() * (1 - f)
}

// shortDur renders 6h12m / 45m / 30s — no zero-padding, no days, because a
// position naked long enough for days to matter has other problems.
func shortDur(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	h, m := int(d.Hours()), int(d.Minutes())%60
	if h == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh%02dm", h, m)
}

// Title is the collapsed line. Elapsed time and the count live here so two
// consecutive pushes never look the same.
func (a nakedAlert) Title() string {
	return fmt.Sprintf("🚨 %s %s 裸倉 %s · 第 %d 次",
		a.Short, sideZH(a.Side), shortDur(a.Naked), a.Nag)
}

func (a nakedAlert) Body() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%g @ %g", a.Qty, a.Entry)
	if a.Mark > 0 {
		fmt.Fprintf(&b, " → mark %g", a.Mark)
	}
	fmt.Fprintf(&b, "\n名目 %.0fu · %dx · %s", a.notional(), a.Leverage, a.MarginMode)
	if a.Mark > 0 {
		fmt.Fprintf(&b, " · 浮動 %+.2fu", a.unrealised())
	}
	if f := a.killFrac(); f > 0 {
		fmt.Fprintf(&b, "\n距爆倉 %.1f%% (約 %.4f) · 帳戶槓桿 %.1fx",
			f*100, a.killPrice(), a.notional()/a.Equity)
	}
	fmt.Fprintf(&b, "\n已裸 %s,交易所沒有任何保護性停損", shortDur(a.Naked))
	if a.Journalled {
		b.WriteString("\n\njournal 有這筆,但 stop = 0,所以逐筆守衛會跳過它 —— 這裡是它唯一的監控")
	} else {
		b.WriteString("\n\n這張單不在 journal 裡,所以 /ops/verify 的逐筆檢查看不到它")
	}
	return b.String()
}
