package autostrat

import (
	"strings"
	"testing"

	"github.com/henry190927/trading-bot/autotrade"
)

// EvalAutoTrigger's switch and autotrade.KnownStrategies must not drift. They
// live in different packages (autostrat imports autotrade, so the list cannot
// live next to the switch), and the failure mode is silent: a rule naming a
// strategy the switch does not handle falls through to an empty Trigger, loads
// fine, renders on the /ops panel, and simply never fires — indistinguishable
// from a rule whose conditions have not been met.
//
// Reads the source rather than exercising the dispatch, because every real
// branch needs a live BingX client. Crude, and it catches the only thing that
// actually goes wrong here: adding a case in one place and not the other.
func TestKnownStrategiesMatchTheDispatch(t *testing.T) {
	src := readSource(t, "eval.go")
	for _, name := range autotrade.KnownStrategies {
		if !strings.Contains(src, `case "`+name+`":`) {
			t.Errorf("KnownStrategies lists %q but EvalAutoTrigger has no case for it — "+
				"a rule naming it would load and never fire", name)
		}
	}
	// And the reverse: a case the list does not know about would make
	// UnknownStrategies warn about a strategy that actually works.
	for _, name := range casesIn(src) {
		found := false
		for _, k := range autotrade.KnownStrategies {
			if k == name {
				found = true
			}
		}
		if !found {
			t.Errorf("EvalAutoTrigger handles %q but autotrade.KnownStrategies omits it — "+
				"Load would warn that a working rule will never fire", name)
		}
	}
}

// struct-momentum is the point of the change: the book had exactly one
// continuation-capable rule out of seventeen and it was only momentum because
// strategyFor happened to agree, with nothing in autotrade.json saying so.
func TestStructMomentumIsSelectable(t *testing.T) {
	src := readSource(t, "eval.go")
	if !strings.Contains(src, `case "struct-momentum":`) {
		t.Fatal("struct-momentum must be namable in a rule, not only reachable by allowlist accident")
	}
	if !strings.Contains(src, "signal.StrategyStructMomentum)") {
		t.Error("the struct-momentum case must force the strategy, not fall back to the allowlist")
	}
	// The "engine" case must keep deferring to strategyFor, or every existing
	// rule silently changes strategy.
	if !strings.Contains(src, "signal.StrategyUnset)") {
		t.Error(`the "engine" case must pass StrategyUnset so strategyFor still decides`)
	}
}
