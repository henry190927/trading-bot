package main

import (
	"fmt"

	"github.com/gin-gonic/gin"
)

// translator returns the request language's formatter: the dictionary entry
// for key, with args applied as fmt verbs. A missing key returns the key.
func (s *server) translator(lang string) func(key string, args ...any) string {
	d := s.dicts[lang]
	return func(key string, args ...any) string {
		f, ok := d[key]
		if !ok {
			return key
		}
		if len(args) == 0 {
			return f
		}
		return fmt.Sprintf(f, args...)
	}
}

// localizeBias rewrites the human-readable labels of a /api/chart/bias
// response in the request's language, from the codes and numbers that sit
// beside them. The compute* functions and their tests keep producing the
// Chinese labels; this is the one place the language is chosen.
//
// It returns a copy and never mutates resp: resp is the cached value, shared
// by every request in both languages.
func localizeBias(resp gin.H, tr func(string, ...any) string) gin.H {
	out := gin.H{}
	for k, v := range resp {
		out[k] = v
	}
	if a, ok := resp["alignment"].(gin.H); ok {
		out["alignment"] = localizeAlignment(a, tr)
	}
	if r, ok := resp["range"].(gin.H); ok {
		c := copyH(r)
		isRange, _ := r["isRange"].(bool)
		switch zone, _ := r["zone"].(string); {
		case !isRange:
			c["action"] = tr("range.trend")
		case zone == "bottom":
			c["action"] = tr("range.edgeLong")
		case zone == "top":
			c["action"] = tr("range.edgeShort")
		default:
			c["action"] = tr("range.middle")
		}
		out["range"] = c
	}
	if tfs, ok := resp["tfs"].([]gin.H); ok {
		lt := make([]gin.H, len(tfs))
		for i, t := range tfs {
			lt[i] = localizeTFRow(t, tr)
		}
		out["tfs"] = lt
	}
	if o, ok := resp["oi"].(gin.H); ok {
		c := copyH(o)
		if q, _ := o["quadrant"].(string); q != "" {
			c["reading"] = tr("oi.q." + q)
		}
		out["oi"] = c
	}
	return out
}

func copyH(h gin.H) gin.H {
	c := gin.H{}
	for k, v := range h {
		c[k] = v
	}
	return c
}

func localizeAlignment(a gin.H, tr func(string, ...any) string) gin.H {
	c := copyH(a)
	word := func(sum int) string {
		switch {
		case sum > 0:
			return tr("bias.word.long")
		case sum < 0:
			return tr("bias.word.short")
		}
		return tr("bias.word.neutral")
	}
	sign := func(v int) int {
		switch {
		case v > 0:
			return 1
		case v < 0:
			return -1
		}
		return 0
	}
	htf, _ := a["htfSum"].(int)
	ltf, _ := a["ltfSum"].(int)
	agree, _ := a["agree"].(int)
	against, _ := a["against"].(int)
	n, _ := a["tfCount"].(int)
	veto, _ := a["oneHourDissents"].(bool)
	state, _ := a["state"].(string)
	breadth := tr("bias.breadth", agree, n)
	if against > 0 {
		breadth = tr("bias.breadthAgainst", agree, n, against)
	}
	if veto {
		breadth += tr("bias.oneHourVeto")
	}
	dom := sign(htf + ltf)
	switch {
	case state == "conflict":
		c["label"] = tr("bias.conflict", word(sign(htf)), word(sign(ltf)))
	case state == "mixed":
		c["label"] = tr("bias.mixed")
	case len(state) > 8 && state[:8] == "aligned-":
		c["label"] = tr("bias.aligned", word(dom)) + breadth
	case len(state) > 5 && state[:5] == "weak-":
		c["label"] = tr("bias.weak", word(dom)) + breadth
	}
	return c
}

func localizeTFRow(t gin.H, tr func(string, ...any) string) gin.H {
	c := copyH(t)
	if _, ok := t["pocArrow"]; !ok {
		return c // an "na" row (the TF failed to load) has no POC to describe
	}
	arrow, _ := t["pocArrow"].(string)
	drift, _ := t["pocDrift"].(float64)
	stacked, _ := t["pocStacked"].(bool)
	days, _ := t["pocSpanDays"].(float64)
	hours, _ := t["pocSpanHours"].(float64)
	span := ""
	switch {
	case days > 0:
		span = tr("bias.spanDays", days)
	case hours > 0:
		span = tr("bias.spanHours", hours)
	}
	label := fmt.Sprintf("POC%s %s %+.1f%%", span, arrow, drift*100)
	if stacked {
		label += " stacked"
	} else {
		label += tr("bias.notStacked")
	}
	c["poc"] = label
	return c
}
