package finnhub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/earnings"
)

func TestSynthDatetime(t *testing.T) {
	cases := []struct {
		date, hour string
		wantTime   string // "" = expect error
		wantWhen   string
	}{
		{"2026-08-27", "amc", "2026-08-27T20:05:00Z", "amc"},
		{"2026-08-27", "bmo", "2026-08-27T11:00:00Z", "bmo"},
		{"2026-08-27", "dmh", "2026-08-27T16:00:00Z", ""}, // normalized
		{"2026-08-27", "", "2026-08-27T16:00:00Z", ""},
		{"2026-08-27", "AMC", "2026-08-27T20:05:00Z", "amc"}, // case-insensitive
		{"not-a-date", "amc", "", ""},
	}
	for _, tc := range cases {
		dt, when, err := synthDatetime(tc.date, tc.hour)
		if tc.wantTime == "" {
			if err == nil {
				t.Errorf("synthDatetime(%q,%q): want error", tc.date, tc.hour)
			}
			continue
		}
		if err != nil {
			t.Errorf("synthDatetime(%q,%q): %v", tc.date, tc.hour, err)
			continue
		}
		if got := dt.Format(time.RFC3339); got != tc.wantTime || when != tc.wantWhen {
			t.Errorf("synthDatetime(%q,%q) = %s/%q, want %s/%q", tc.date, tc.hour, got, when, tc.wantTime, tc.wantWhen)
		}
	}
}

func TestMapRowsSkipsBadDate(t *testing.T) {
	cr := calResp{}
	cr.EarningsCalendar = append(cr.EarningsCalendar,
		struct {
			Date   string `json:"date"`
			Hour   string `json:"hour"`
			Symbol string `json:"symbol"`
		}{"2026-08-27", "amc", "nvda"},
		struct {
			Date   string `json:"date"`
			Hour   string `json:"hour"`
			Symbol string `json:"symbol"`
		}{"garbage", "amc", "SNDK"},
	)
	got := mapRows(cr)
	if len(got) != 1 {
		t.Fatalf("mapRows len = %d, want 1 (bad date skipped)", len(got))
	}
	if got[0].Symbol != "NVDA" || got[0].When != "amc" || got[0].DatetimeUTC != "2026-08-27T20:05:00Z" {
		t.Errorf("mapRows[0] = %+v", got[0])
	}
	if got[0].BeforeMinutes != 0 || got[0].AfterMinutes != 0 {
		t.Errorf("windows should be 0 (filled by earnings.applyDefaults), got %d/%d", got[0].BeforeMinutes, got[0].AfterMinutes)
	}
}

func TestBuildFileSorted(t *testing.T) {
	evts := []Event{
		{Symbol: "SNDK", DatetimeUTC: "2026-09-10T20:05:00Z", When: "amc"},
		{Symbol: "NVDA", DatetimeUTC: "2026-08-27T20:05:00Z", When: "amc"},
	}
	data, err := BuildFile(evts, time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	// Round-trip: the produced file must load cleanly into the earnings provider.
	cal := earnings.NewCalendar()
	if err := cal.LoadJSON(data); err != nil {
		t.Fatalf("earnings.LoadJSON on BuildFile output: %v", err)
	}
	if cal.Len() != 2 {
		t.Fatalf("round-trip Len = %d, want 2", cal.Len())
	}
	// NVDA (earlier) must sort first; its default amc window fills in on load.
	nvda := cal.ActiveAt("NVDA", mustParse(t, "2026-08-27T21:00:00Z"))
	if nvda == nil || nvda.When != "amc" || nvda.BeforeMinutes != 120 || nvda.AfterMinutes != 960 {
		t.Errorf("round-trip NVDA = %+v, want amc 120/960 active", nvda)
	}
}

func TestFetchSymbol(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") == "" || r.URL.Query().Get("symbol") != "NVDA" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"earningsCalendar":[
			{"date":"2026-08-27","hour":"amc","symbol":"NVDA","epsEstimate":1.2},
			{"date":"2026-11-19","hour":"amc","symbol":"NVDA"}
		]}`))
	}))
	defer srv.Close()

	c := NewClient("test-token")
	c.BaseURL = srv.URL
	evts, err := c.FetchSymbol(context.Background(), "NVDA", "2026-01-01", "2026-12-31")
	if err != nil {
		t.Fatalf("FetchSymbol: %v", err)
	}
	if len(evts) != 2 {
		t.Fatalf("len = %d, want 2", len(evts))
	}
	if evts[0].Symbol != "NVDA" || evts[0].DatetimeUTC != "2026-08-27T20:05:00Z" {
		t.Errorf("evts[0] = %+v", evts[0])
	}
}

func TestFetchSymbolErrors(t *testing.T) {
	// Empty token → fail fast, no HTTP call.
	c := NewClient("")
	if _, err := c.FetchSymbol(context.Background(), "NVDA", "a", "b"); err == nil {
		t.Error("empty token should error")
	}
	// Non-200 → error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c2 := NewClient("t")
	c2.BaseURL = srv.URL
	if _, err := c2.FetchSymbol(context.Background(), "NVDA", "a", "b"); err == nil {
		t.Error("HTTP 429 should error")
	}
}

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}
