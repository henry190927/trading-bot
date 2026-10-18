package main

import (
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/henry190927/trading-bot/market"
)

func testTranslator(t *testing.T, lang string) func(string, ...any) string {
	t.Helper()
	dicts, err := loadDicts(i18nFS)
	if err != nil {
		t.Fatal(err)
	}
	return (&server{dicts: dicts}).translator(lang)
}

// In Chinese, localizeBias must rebuild exactly what the compute functions
// print, for every alignment shape: the Chinese UI must not move.
func TestLocalizeBiasZhIsIdentity(t *testing.T) {
	zh := testTranslator(t, "zh")
	for _, sc := range [][5]int{
		{0, -1, 0, 0, 0}, {-1, -1, -1, -1, 0}, {0, 0, 0, 2, 1}, {-1, -1, -1, 1, 0},
		{0, 0, 0, 0, 0}, {1, -1, -1, 0, 0}, {1, 1, -3, -3, 0}, {0, 2, -1, 0, 1}, {1, 1, -1, 1, 1},
	} {
		a := computeAlignment(tfScores(sc[0], sc[1], sc[2], sc[3], sc[4]))
		got := localizeAlignment(a, zh)["label"]
		if got != a["label"] {
			t.Errorf("%v: localized %q, compute %q", sc, got, a["label"])
		}
	}

	candles := make([]market.Candle, 40)
	for i := range candles {
		candles[i].Low, candles[i].High = 140, 160
	}
	candles[5].Low, candles[6].High = 100, 200
	for _, price := range []float64{110, 150, 190} {
		for _, state := range []string{"mixed", "aligned-long"} {
			rg := computeRange(candles, price, state)
			out := localizeBias(gin.H{"range": rg}, zh)["range"].(gin.H)
			if out["action"] != rg["action"] {
				t.Errorf("range %v %s: localized %q, compute %q", price, state, out["action"], rg["action"])
			}
		}
	}

	for _, q := range [][2]float64{{0.05, -0.01}, {0.05, 0.01}, {0.05, 0}, {-0.05, -0.01}, {-0.05, 0.01}, {-0.05, 0}} {
		key, label := oiQuadrant(q[0], q[1])
		out := localizeBias(gin.H{"oi": gin.H{"quadrant": key, "reading": label}}, zh)["oi"].(gin.H)
		if out["reading"] != label {
			t.Errorf("oi %v: localized %q, compute %q", q, out["reading"], label)
		}
	}
}

// English carries no Chinese, and the cached response is never mutated.
func TestLocalizeBiasEnglish(t *testing.T) {
	en := testTranslator(t, "en")
	a := computeAlignment(tfScores(0, 2, -1, 0, 1))
	tf := gin.H{"tf": "1h", "pocArrow": "↘", "pocDrift": -0.036, "pocStacked": true, "pocSpanDays": 8.0, "poc": "POC/200根≈8天 ↘ -3.6% stacked"}
	resp := gin.H{"alignment": a, "tfs": []gin.H{tf, {"tf": "4h", "dir": "na"}},
		"range": gin.H{"isRange": true, "zone": "top", "action": "邊緣做空 (止損箱外)"},
		"oi":    gin.H{"quadrant": "shorts-building", "reading": "跌勢中新倉 — 空單堆積"}}
	out := localizeBias(resp, en)
	for _, s := range []any{
		out["alignment"].(gin.H)["label"], out["tfs"].([]gin.H)[0]["poc"],
		out["range"].(gin.H)["action"], out["oi"].(gin.H)["reading"],
	} {
		if str, _ := s.(string); str == "" || strings.ContainsFunc(str, func(r rune) bool { return r >= 0x4e00 && r <= 0x9fff }) {
			t.Errorf("English label %q is empty or has Chinese", s)
		}
	}
	if got := out["tfs"].([]gin.H)[0]["poc"]; got != "POC/200 bars≈8d ↘ -3.6% stacked" {
		t.Errorf("poc = %q", got)
	}
	if _, ok := out["tfs"].([]gin.H)[1]["poc"]; ok {
		t.Error("an na row gained a POC label")
	}
	if resp["oi"].(gin.H)["reading"] != "跌勢中新倉 — 空單堆積" || tf["poc"] != "POC/200根≈8天 ↘ -3.6% stacked" {
		t.Error("localizeBias mutated the cached response")
	}
}
