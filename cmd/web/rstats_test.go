package main

import (
	"math"
	"testing"
	"time"

	"myFirstGo/trading-bot/autotrade"
)

func d(day, hour int) time.Time {
	return time.Date(2026, 9, day, hour, 59, 0, 0, time.UTC)
}

func pos(status autotrade.OutcomeStatus, netR, unrealR float64, exit time.Time, fired time.Time) autotrade.Position {
	return autotrade.Position{
		Fire:    autotrade.PaperFire{Time: fired, Symbol: "ETH", Strategy: "engine", Side: "long"},
		Outcome: autotrade.Outcome{Status: status, NetR: netR, UnrealR: unrealR, ExitAt: exit},
	}
}

// The real 2026-09-02 book, in exit order: -1, +2, -1, -1, +1, +1, -1.
// Every expected number below is traced by hand first, per the standing rule
// that my "expected" is the thing most likely to be wrong.
func todaysPositions() []autotrade.Position {
	return []autotrade.Position{
		pos(autotrade.OutStop, -1, 0, d(2, 0), d(2, 0)),
		pos(autotrade.OutTP, +2, 0, d(2, 3), d(2, 2)),
		pos(autotrade.OutStop, -1, 0, d(2, 8), d(2, 3)),
		pos(autotrade.OutStop, -1, 0, d(2, 8), d(2, 7)), // same exit hour, later fire
		pos(autotrade.OutTP, +1, 0, d(2, 9), d(1, 19)),
		pos(autotrade.OutTP, +1, 0, d(2, 10), d(1, 19)),
		pos(autotrade.OutStop, -1, 0, d(2, 9), d(0, 6)),
	}
}

func TestRTradesFromPositions(t *testing.T) {
	ps := append(todaysPositions(),
		pos(autotrade.OutOpen, 0, +0.43, time.Time{}, d(2, 16)),
		pos(autotrade.OutOpen, 0, +1.11, time.Time{}, d(2, 12)),
		pos(autotrade.OutNoFill, 0, 0, time.Time{}, d(2, 5)),
		pos(autotrade.OutPending, 0, 0, time.Time{}, d(2, 15)),
		// Settled but no exit time — the kline window no longer reaches it.
		pos(autotrade.OutStop, -1, 0, time.Time{}, d(2, 1)),
	)

	rts, unscoreable := rTradesFromPositions(ps)
	if len(rts) != 7 {
		t.Fatalf("realized positions = %d, want 7 (open/pending/no-fill excluded)", len(rts))
	}
	if unscoreable != 1 {
		t.Errorf("unscoreable = %d, want 1 — a settled position with no exit must be REPORTED, not dropped", unscoreable)
	}
	var sum float64
	for _, r := range rts {
		sum += r.R
	}
	// -1 +2 -1 -1 +1 +1 -1 = 0
	if math.Abs(sum) > 1e-9 {
		t.Errorf("sum = %+.2f, want +0.00", sum)
	}

	openR, openN := openUnrealR(ps)
	if openN != 2 {
		t.Errorf("open count = %d, want 2 (pending is not filled, so it floats no R)", openN)
	}
	if math.Abs(openR-1.54) > 1e-9 { // 0.43 + 1.11
		t.Errorf("open unrealized = %+.2f, want +1.54", openR)
	}
}

func TestBuildEquityCurveFromPositions(t *testing.T) {
	rts, _ := rTradesFromPositions(todaysPositions())
	eq := buildEquityCurve(rts)

	if !eq.HasData {
		t.Fatal("want HasData")
	}
	// 7 trades + the (0,0) origin.
	if len(eq.Points) != 8 {
		t.Fatalf("points = %d, want 8 (origin + 7)", len(eq.Points))
	}
	if eq.Points[0].R != 0 {
		t.Errorf("origin R = %v, want 0", eq.Points[0].R)
	}

	// Exit order, with ties broken by INPUT order (SliceStable; rTrade carries
	// no fire time, so there is nothing else to sort on — and DedupFires gives
	// a deterministic input order, which is what makes this reproducible).
	//
	// The two 09:59 exits arrive as +1 then -1, so:
	//   -1 -> -1 | +2 -> +1 | -1 -> 0 | -1 -> -1 | +1 -> 0 | -1 -> -1 | +1 -> 0
	want := []float64{0, -1, 1, 0, -1, 0, -1, 0}
	for i, w := range want {
		if math.Abs(eq.Points[i].R-w) > 1e-9 {
			t.Errorf("point[%d] cum = %+.2f, want %+.2f (full: %v)", i, eq.Points[i].R, w, cums(eq))
		}
	}
	if math.Abs(eq.CurrentR) > 1e-9 {
		t.Errorf("CurrentR = %+.2f, want +0.00 — must equal the breaker's todayR for this set", eq.CurrentR)
	}
	if math.Abs(eq.PeakR-1) > 1e-9 {
		t.Errorf("PeakR = %+.2f, want +1.00", eq.PeakR)
	}
	// Peak +1 at index 2; the curve ends at 0, so it sits 1R below its peak.
	if math.Abs(eq.Drawdown-1) > 1e-9 {
		t.Errorf("Drawdown = %+.2f, want 1.00 (peak +1 → current 0)", eq.Drawdown)
	}
	if eq.Trend != "flat" {
		t.Errorf("Trend = %q, want flat at exactly 0R", eq.Trend)
	}
	if eq.LinePath == "" || eq.AreaPath == "" {
		t.Error("SVG paths must be built server-side")
	}
	// Every point must land inside the viewBox, or the SVG clips.
	for i, p := range eq.Points {
		if p.X < -0.01 || p.X > float64(eq.ViewBoxW)+0.01 || p.Y < -0.01 || p.Y > float64(eq.ViewBoxH)+0.01 {
			t.Errorf("point[%d] (%.2f,%.2f) outside %dx%d viewBox", i, p.X, p.Y, eq.ViewBoxW, eq.ViewBoxH)
		}
	}
}

func cums(eq equityCurve) []float64 {
	out := make([]float64, len(eq.Points))
	for i, p := range eq.Points {
		out[i] = p.R
	}
	return out
}

func TestBuildEquityCurveEmpty(t *testing.T) {
	if eq := buildEquityCurve(nil); eq.HasData {
		t.Error("no trades must yield HasData=false, not a one-point line")
	}
	if eq := buildEquityCurve([]rTrade{}); eq.HasData {
		t.Error("empty slice must yield HasData=false")
	}
}

func TestBuildRHistogramFromPositions(t *testing.T) {
	rts, _ := rTradesFromPositions(todaysPositions())
	bs := buildRHistogram(rts)

	// 4 stops at -1.0 and 2 wins at +1.0 and 1 at +2.0.
	find := func(label string) rBucket {
		for _, b := range bs {
			if b.Label == label {
				return b
			}
		}
		t.Fatalf("bucket %q missing from %v", label, labels(bs))
		return rBucket{}
	}
	if got := find("-1").Count; got != 4 {
		t.Errorf("-1R bucket = %d, want 4", got)
	}
	if got := find("+1").Count; got != 2 {
		t.Errorf("+1R bucket = %d, want 2", got)
	}
	if got := find("+2").Count; got != 1 {
		t.Errorf("+2R bucket = %d, want 1", got)
	}
	total := 0
	for _, b := range bs {
		total += b.Count
	}
	if total != 7 {
		t.Errorf("bucket total = %d, want 7 — every realized trade must land somewhere", total)
	}
	// The tallest bucket must be scaled to full height, or the bars mean nothing.
	var maxH float64
	for _, b := range bs {
		if b.PctHeight > maxH {
			maxH = b.PctHeight
		}
	}
	if math.Abs(maxH-100) > 1e-9 {
		t.Errorf("tallest bar = %.1f%%, want 100%%", maxH)
	}
}

func labels(bs []rBucket) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Label
	}
	return out
}

func TestBuildDailyCalendarFromPositions(t *testing.T) {
	rts, _ := rTradesFromPositions(todaysPositions())
	grid := buildDailyCalendar(rts, 42)
	if len(grid) == 0 {
		t.Fatal("want a grid")
	}
	// Every row is a full week, or the weekday columns misalign.
	for i, row := range grid {
		if len(row) != 7 {
			t.Errorf("row %d has %d cells, want 7", i, len(row))
		}
	}
	// All seven trades closed on 09-02 or 09-01 UTC; in UTC+8 that spreads
	// across 09-02 and 09-03. Assert on the TOTAL rather than a fixed cell so
	// the test doesn't encode the runner's timezone.
	var sum float64
	n := 0
	for _, row := range grid {
		for _, c := range row {
			sum += c.TotalR
			n += c.Count
		}
	}
	if n != 7 {
		t.Errorf("calendar counted %d trades, want 7", n)
	}
	if math.Abs(sum) > 1e-9 {
		t.Errorf("calendar total R = %+.2f, want +0.00", sum)
	}
	// Intensity must be normalised, never above full saturation.
	for _, row := range grid {
		for _, c := range row {
			if c.Intensity < 0 || c.Intensity > 100.01 {
				t.Errorf("cell %s intensity %.1f out of 0-100", c.Date.Format("01-02"), c.Intensity)
			}
		}
	}
}

// Bucket labels must name the bucket a value actually lands in. The -1R stop
// is the design outcome, so it is the one label that must be right.
func TestRHistogramLabelsMatchContents(t *testing.T) {
	at := func(r float64) string {
		for _, b := range buildRHistogram([]rTrade{{R: r, Closed: d(2, 1)}}) {
			if b.Count == 1 {
				return b.Label
			}
		}
		return "(none)"
	}
	for _, tc := range []struct {
		r    float64
		want string
	}{
		{-1.00, "-1"},   // the design stop
		{-1.25, "-1.5"}, // in [-1.5,-1), so the "-1.5" bucket
		{-0.75, "-1"},   // in [-1,-0.5), so the "-1" bucket
		{-0.50, "-0.5"},
		{-0.25, "-0.5"},
		{0.00, "0"},
		{0.50, "+0.5"},
		{1.00, "+1"},
		{2.00, "+2"},
		{5.00, ">+2.5"},
		{-9.00, "<-2.5"},
	} {
		if got := at(tc.r); got != tc.want {
			t.Errorf("R %+.2f landed in bucket %q, want %q", tc.r, got, tc.want)
		}
	}
}
