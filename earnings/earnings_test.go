package earnings

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad time %q: %v", s, err)
	}
	return tm.UTC()
}

// sampleJSON: SNDK amc 2026-08-27T20:05Z (120/960) and NVDA amc
// 2026-08-28T20:20Z (default 0/0 → 120/960).
const sampleJSON = `{
  "updated_utc": "2026-08-18T00:00:00Z",
  "events": [
    {"symbol":"SNDK","datetime_utc":"2026-08-27T20:05:00Z","when":"amc","blackout_before_min":120,"blackout_after_min":960},
    {"symbol":"NVDA","datetime_utc":"2026-08-28T20:20:00Z","when":"amc"}
  ]
}`

func loadSample(t *testing.T) *Calendar {
	t.Helper()
	c := NewCalendar()
	if err := c.LoadJSON([]byte(sampleJSON)); err != nil {
		t.Fatalf("LoadJSON: %v", err)
	}
	return c
}

func TestWindowMath(t *testing.T) {
	// SNDK: start 27T18:05, end 28T12:05 (16h after 20:05).
	e := Event{DatetimeUTC: mustTime(t, "2026-08-27T20:05:00Z"), BeforeMinutes: 120, AfterMinutes: 960}
	start, end := e.Window()
	if want := mustTime(t, "2026-08-27T18:05:00Z"); !start.Equal(want) {
		t.Errorf("start = %v, want %v", start, want)
	}
	if want := mustTime(t, "2026-08-28T12:05:00Z"); !end.Equal(want) {
		t.Errorf("end = %v, want %v", end, want)
	}
}

func TestContains(t *testing.T) {
	e := Event{DatetimeUTC: mustTime(t, "2026-08-27T20:05:00Z"), BeforeMinutes: 120, AfterMinutes: 960}
	cases := []struct {
		at   string
		want bool
	}{
		{"2026-08-27T18:05:00Z", true},  // inclusive start
		{"2026-08-27T18:04:59Z", false}, // 1s before start
		{"2026-08-27T19:00:00Z", true},  // mid
		{"2026-08-28T12:04:59Z", true},  // 1s before end
		{"2026-08-28T12:05:00Z", false}, // exclusive end
		{"2026-08-28T13:00:00Z", false}, // after
	}
	for _, tc := range cases {
		if got := e.Contains(mustTime(t, tc.at)); got != tc.want {
			t.Errorf("Contains(%s) = %v, want %v", tc.at, got, tc.want)
		}
	}
}

func TestApplyDefaults(t *testing.T) {
	cases := []struct {
		when                 string
		before, after        int
		wantBefore, wantAfter int
	}{
		{"amc", 0, 0, defAMCBefore, defAMCAfter},   // 120/960
		{"bmo", 0, 0, defBMOBefore, defBMOAfter},   // 720/240
		{"", 0, 0, defBefore, defAfter},            // 120/240
		{"amc", 30, 45, 30, 45},                    // explicit both kept
		{"amc", 0, 500, defAMCBefore, 500},         // partial: before filled, after kept
	}
	for _, tc := range cases {
		e := Event{When: tc.when, BeforeMinutes: tc.before, AfterMinutes: tc.after}
		e.applyDefaults()
		if e.BeforeMinutes != tc.wantBefore || e.AfterMinutes != tc.wantAfter {
			t.Errorf("applyDefaults(when=%q,%d/%d) = %d/%d, want %d/%d",
				tc.when, tc.before, tc.after, e.BeforeMinutes, e.AfterMinutes, tc.wantBefore, tc.wantAfter)
		}
	}
}

func TestActiveAt(t *testing.T) {
	c := loadSample(t)
	inside := mustTime(t, "2026-08-27T19:00:00Z") // inside SNDK window only
	cases := []struct {
		name string
		sym  string
		at   time.Time
		want string // expected event symbol, "" = nil
	}{
		{"sndk bare inside", "SNDK", inside, "SNDK"},
		{"sndk synthetic inside", "NCSKSNDK2USD-USDT", inside, "SNDK"},
		{"sndk lowercase synthetic", "ncsksndk2usd-usdt", inside, "SNDK"},
		{"nvda not yet", "NVDA", inside, ""},
		{"btc never", "BTC-USDT", inside, ""},
		{"metal synthetic never", "NCCOGOLD2USD-USDT", inside, ""},
		{"sndk after window", "SNDK", mustTime(t, "2026-08-28T13:00:00Z"), ""},
		{"nvda inside its own window", "NVDA", mustTime(t, "2026-08-28T19:00:00Z"), "NVDA"},
	}
	for _, tc := range cases {
		got := c.ActiveAt(tc.sym, tc.at)
		if tc.want == "" {
			if got != nil {
				t.Errorf("%s: ActiveAt = %+v, want nil", tc.name, got)
			}
			continue
		}
		if got == nil || got.Symbol != tc.want {
			t.Errorf("%s: ActiveAt = %v, want symbol %q", tc.name, got, tc.want)
		}
	}
}

func TestNextUpcoming(t *testing.T) {
	c := loadSample(t)
	// SNDK window starts 27T18:05; NVDA window starts 28T18:20.
	before := mustTime(t, "2026-08-27T00:00:00Z")

	if e := c.NextUpcoming("SNDK", before, 7*24*time.Hour); e == nil || e.Symbol != "SNDK" {
		t.Errorf("NextUpcoming SNDK from 27T00:00 = %v, want SNDK", e)
	}
	// Once inside the SNDK window, its start is no longer "upcoming".
	if e := c.NextUpcoming("SNDK", mustTime(t, "2026-08-27T19:00:00Z"), 7*24*time.Hour); e != nil {
		t.Errorf("NextUpcoming SNDK while inside window = %v, want nil", e)
	}
	// NVDA start 28T18:20 is >1 day out; a 1-day lookahead from 27T00:00 misses it.
	if e := c.NextUpcoming("NVDA", before, 24*time.Hour); e != nil {
		t.Errorf("NextUpcoming NVDA within 1d = %v, want nil (too far)", e)
	}
	if e := c.NextUpcoming("NVDA", before, 7*24*time.Hour); e == nil || e.Symbol != "NVDA" {
		t.Errorf("NextUpcoming NVDA within 7d = %v, want NVDA", e)
	}
	if e := c.NextUpcoming("BTC-USDT", before, 30*24*time.Hour); e != nil {
		t.Errorf("NextUpcoming BTC = %v, want nil", e)
	}
}

func TestNextUpcomingPicksSoonest(t *testing.T) {
	c := NewCalendar()
	js := `{"events":[
      {"symbol":"SNDK","datetime_utc":"2026-09-10T20:05:00Z","when":"amc","blackout_before_min":120,"blackout_after_min":960},
      {"symbol":"SNDK","datetime_utc":"2026-08-27T20:05:00Z","when":"amc","blackout_before_min":120,"blackout_after_min":960}
    ]}`
	if err := c.LoadJSON([]byte(js)); err != nil {
		t.Fatalf("LoadJSON: %v", err)
	}
	// From far before both, 30d lookahead sees both → must pick the earlier (Aug 27).
	e := c.NextUpcoming("SNDK", mustTime(t, "2026-08-20T00:00:00Z"), 30*24*time.Hour)
	if e == nil || !e.DatetimeUTC.Equal(mustTime(t, "2026-08-27T20:05:00Z")) {
		t.Errorf("NextUpcoming soonest = %v, want 2026-08-27T20:05Z", e)
	}
}

func TestSymbolMapping(t *testing.T) {
	cases := []struct {
		sym       string
		isStock   bool
		ticker    string
	}{
		{"NCSKSNDK2USD-USDT", true, "SNDK"},
		{"ncsknvda2usd-usdt", true, "NVDA"},
		{"BTC-USDT", false, ""},
		{"ETH-USDT", false, ""},
		{"NCCOGOLD2USD-USDT", false, ""}, // metal synthetic, not NCSK
		{"NCSK2USD-USDT", false, ""},     // empty ticker guard
	}
	for _, tc := range cases {
		if got := IsStockSymbol(tc.sym); got != tc.isStock {
			t.Errorf("IsStockSymbol(%q) = %v, want %v", tc.sym, got, tc.isStock)
		}
		if got := UnderlyingTicker(tc.sym); got != tc.ticker {
			t.Errorf("UnderlyingTicker(%q) = %q, want %q", tc.sym, got, tc.ticker)
		}
	}
}

func TestNormalizeTicker(t *testing.T) {
	cases := map[string]string{
		"NCSKSNDK2USD-USDT": "SNDK", // full synthetic
		"SNDK":              "SNDK", // bare ticker
		"sndk":              "SNDK", // bare lower
		"BTC-USDT":          "",     // pair, not a company
		"XRP-USDT":          "",
		"NCCOGOLD2USD-USDT": "",     // non-stock synthetic
		"":                  "",
	}
	for in, want := range cases {
		if got := normalizeTicker(in); got != want {
			t.Errorf("normalizeTicker(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadJSONErrors(t *testing.T) {
	c := NewCalendar()
	if err := c.LoadJSON([]byte(`{bad json`)); err == nil {
		t.Error("expected error on malformed json")
	}
	if err := c.LoadJSON([]byte(`{"events":[{"symbol":"SNDK","datetime_utc":"not-a-time"}]}`)); err == nil {
		t.Error("expected error on bad datetime")
	}
	if err := c.LoadJSON([]byte(`{"events":[{"symbol":"BTC-USDT","datetime_utc":"2026-08-27T20:05:00Z"}]}`)); err == nil {
		t.Error("expected error on non-company symbol")
	}
}

func TestMetadataAndDefaults(t *testing.T) {
	c := loadSample(t)
	if got, want := c.UpdatedAt(), mustTime(t, "2026-08-18T00:00:00Z"); !got.Equal(want) {
		t.Errorf("UpdatedAt = %v, want %v", got, want)
	}
	if c.Len() != 2 {
		t.Errorf("Len = %d, want 2", c.Len())
	}
	syms := c.Symbols()
	if len(syms) != 2 || syms[0] != "NVDA" || syms[1] != "SNDK" {
		t.Errorf("Symbols = %v, want [NVDA SNDK]", syms)
	}
	// NVDA had no explicit window → amc defaults 120/960.
	nvda := c.ActiveAt("NVDA", mustTime(t, "2026-08-28T19:00:00Z"))
	if nvda == nil || nvda.BeforeMinutes != defAMCBefore || nvda.AfterMinutes != defAMCAfter {
		t.Errorf("NVDA defaults = %v, want %d/%d", nvda, defAMCBefore, defAMCAfter)
	}
}

func TestEmptyCalendarSafe(t *testing.T) {
	c := NewCalendar()
	if c.ActiveAt("SNDK", time.Now().UTC()) != nil {
		t.Error("empty ActiveAt should be nil")
	}
	if c.NextUpcoming("SNDK", time.Now().UTC(), 24*time.Hour) != nil {
		t.Error("empty NextUpcoming should be nil")
	}
	if c.Len() != 0 || len(c.Symbols()) != 0 {
		t.Error("empty calendar Len/Symbols should be 0")
	}
}

func TestLoadFileAndMaybeReload(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "earnings.json")
	if err := os.WriteFile(p, []byte(sampleJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewCalendar()
	if err := c.LoadFile(p); err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if c.Len() != 2 {
		t.Fatalf("after LoadFile Len = %d, want 2", c.Len())
	}
	// Unchanged mtime → no reload.
	if reloaded, err := c.MaybeReload(p); err != nil || reloaded {
		t.Errorf("MaybeReload unchanged = (%v,%v), want (false,nil)", reloaded, err)
	}
	// Bump mtime → reload.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}
	if reloaded, err := c.MaybeReload(p); err != nil || !reloaded {
		t.Errorf("MaybeReload after chtimes = (%v,%v), want (true,nil)", reloaded, err)
	}

	// LoadFile error leaves previous contents intact (stale-but-present).
	if err := c.LoadFile(filepath.Join(dir, "nope.json")); err == nil {
		t.Error("expected error loading missing file")
	}
	if c.Len() != 2 {
		t.Errorf("after failed LoadFile Len = %d, want 2 (preserved)", c.Len())
	}
}
