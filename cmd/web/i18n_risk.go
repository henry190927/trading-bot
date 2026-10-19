package main

import (
	"sort"
	"strings"

	"github.com/henry190927/trading-bot/risk"
)

// riskText renders a risk verdict message in the request's language: the
// dictionary entry risk.<code> with the message's values, then the open
// positions it names. risk.EnglishFormat is the English the logs and pushes
// print; TestRiskMessagesTranslated keeps every code here with the same
// values in the same order.
func riskText(tr func(string, ...any) string, m risk.Msg) string {
	s := tr("risk."+m.Code, m.Args...)
	if len(m.Legs) > 0 {
		legs := append([]string(nil), m.Legs...)
		sort.Strings(legs)
		s += tr("risk.legs", strings.Join(legs, ", "))
	}
	return s
}

// riskWarnings renders every warning of a verdict; never nil, so the JSON is
// always an array.
func riskWarnings(tr func(string, ...any) string, v risk.Verdict) []string {
	out := make([]string, 0, len(v.WarningMsgs))
	for _, m := range v.WarningMsgs {
		out = append(out, riskText(tr, m))
	}
	return out
}
