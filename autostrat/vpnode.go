package autostrat

import (
	"context"
	"fmt"

	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
)

// VPNodeParams are the "vp-node" strategy's settings: arm A2 of cmd/vpbt
// (docs/vpbt_design.md §9.4). The live rule runs DefaultVPNodeParams and
// cmd/vpbt calls VPNodeTrigger with the same struct, so the backtest
// measures the rule the daemon runs.
type VPNodeParams struct {
	Lookback    int     // closed bars in the profile, all BEFORE the signal bar
	Bins        int     // profile bins
	EntryFrac   float64 // limit sits this far from the node's far foot (0.7 = upper 30% for a long)
	ApproachATR float64 // a close within this many ATR beyond the near edge counts as approaching
	BufATR      float64 // stop buffer past the far foot, in ATR(14)
	MinR        float64 // skip a setup whose reward:risk is below this
	TargetPOC   bool    // true = next node's POC, false = its near foot
	Opts        indicator.NodeOpts
}

// DefaultVPNodeParams is the configuration fixed for the forward test on
// 2026-11-01: 1h, POC target. The rule's expiry_bars (24) is set in
// autotrade.json, not here.
func DefaultVPNodeParams() VPNodeParams {
	return VPNodeParams{
		Lookback: 200, Bins: 80, EntryFrac: 0.7, ApproachATR: 1.0,
		BufATR: 0.25, MinR: 1.5, TargetPOC: true, Opts: indicator.DefaultNodeOpts(),
	}
}

// VPNodeTrigger evaluates closed bar i of cs. The profile is built from
// cs[i-Lookback:i], so bar i never sees itself.
//
// Long (short mirrors it at the node's lower edge): the previous close was
// more than ApproachATR above node n's High, and this close lands within that
// band or inside n without breaking n.Low. A limit rests at
// n.Low + EntryFrac×width, the stop sits BufATR below n.Low, and the target
// is the next node up. Nodes are tried from the lowest; per node the long is
// tried before the short, and the first that clears MinR fires.
func VPNodeTrigger(cs []market.Candle, atr float64, i int, wantLong, wantShort bool, p VPNodeParams) Trigger {
	if i < p.Lookback || i < 1 || i >= len(cs) || atr <= 0 {
		return Trigger{}
	}
	bar, prev := cs[i], cs[i-1]
	nodes := indicator.FindNodes(indicator.BuildVolumeProfile(cs[i-p.Lookback:i], p.Bins, 1), p.Opts)

	for k, n := range nodes {
		width := n.High - n.Low
		if wantLong && k+1 < len(nodes) {
			next := nodes[k+1]
			band := n.High + p.ApproachATR*atr
			if prev.Close > band && bar.Close <= band && bar.Close >= n.Low {
				tgt := next.Low
				if p.TargetPOC {
					tgt = next.POC
				}
				if t, ok := vpBracket(p, "long", n.Low+p.EntryFrac*width, n.Low-p.BufATR*atr, tgt, n, next); ok {
					return t
				}
			}
		}
		if wantShort && k > 0 {
			next := nodes[k-1]
			band := n.Low - p.ApproachATR*atr
			if prev.Close < band && bar.Close >= band && bar.Close <= n.High {
				tgt := next.High
				if p.TargetPOC {
					tgt = next.POC
				}
				if t, ok := vpBracket(p, "short", n.High-p.EntryFrac*width, n.High+p.BufATR*atr, tgt, n, next); ok {
					return t
				}
			}
		}
	}
	return Trigger{}
}

func vpBracket(p VPNodeParams, side string, entry, stop, tgt float64, n, next indicator.Node) (Trigger, bool) {
	risk, reward := entry-stop, tgt-entry
	if side == "short" {
		risk, reward = stop-entry, entry-tgt
	}
	if risk <= 0 || reward <= 0 || reward/risk < p.MinR {
		return Trigger{}, false
	}
	return Trigger{
		Fire: true, Side: side, Market: false, Entry: entry, Stop: stop, TP: tgt,
		Why: fmt.Sprintf("volume node %.4f–%.4f approached, limit toward node %.4f–%.4f (%.2fR)",
			n.Low, n.High, next.Low, next.High, reward/risk),
		Tags: []Tag{tag("vp-node", n.Low, n.High), tag("vp-next", next.Low, next.High)},
	}, true
}

func evalVPNode(ctx context.Context, client *bingx.Client, sym market.Symbol, r autotrade.Rule) Trigger {
	p := DefaultVPNodeParams()
	cs, err := client.Klines(ctx, sym, market.Timeframe(r.TF), p.Lookback+60)
	if err != nil || len(cs) < p.Lookback+15 {
		return Trigger{}
	}
	atrs := indicator.ATR(cs, 14)
	if len(atrs) == 0 {
		return Trigger{}
	}
	return VPNodeTrigger(cs, atrs[len(atrs)-1], len(cs)-1,
		r.Side == "long" || r.Side == "auto", r.Side == "short" || r.Side == "auto", p)
}
