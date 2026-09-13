package main

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/bracket"
	"github.com/henry190927/trading-bot/journal"

	"github.com/gin-gonic/gin"
)

// verifyTrade cross-checks one open journal trade against the LIVE exchange —
// the "don't trust local bookkeeping, look at BingX" tool. Flags when the
// journal thinks there's a stop/position but the exchange disagrees.
type verifyTrade struct {
	ID                    int
	Symbol, Side          string
	Entry, Stop, TP1, TP2 float64
	HasPosition           bool
	PosQty, PosEntry      float64
	Orders                []bingx.OpenOrder
	HasStop, HasTP        bool
	Warn                  string

	// SessionWarn is the cash-open-bar advisory for stock synthetics (see
	// session_guard.go). Kept separate from Warn, and counted separately,
	// because Warn means "the exchange disagrees with the journal" — fix it
	// now — while this means "this stop sits inside one bar's ordinary
	// noise", a sizing judgement the desk may have made on purpose.
	SessionWarn string
}

// orphanPos is a position the EXCHANGE holds that no open journal trade claims.
//
// This page iterated the journal and nothing else, so a position placed
// outside it produced zero rows — and zero rows rendered the GREEN
// "all trades protected & matched" badge. On 2026-09-10 that badge was showing
// while a BTC long (0.1202 @ 78,264.5, 125x cross, 9,407u notional against
// 258.52u of equity) sat naked for five hours. The page's own subtitle says
// "trust the exchange, not local bookkeeping"; the LIST of what to check came
// from local bookkeeping.
type orphanPos struct {
	Symbol, Side string
	Qty, Entry   float64
	Notional     float64
	Leverage     int
	MarginMode   string
	HasStop      bool
	Orders       []bingx.OpenOrder
	ReadErr      string
}

// exchangeOrphans enumerates from the EXCHANGE and uses the journal only to
// EXCLUDE what the per-trade rows above already cover. That inversion is the
// point: read-only is not the same as not-authoritative-for-what-exists.
func (s *server) exchangeOrphans(ctx context.Context, trades []journal.Trade) (out []orphanPos, nakedN int) {
	poss, err := s.client.AllPositions(ctx)
	if err != nil {
		return nil, 0
	}
	claimed := map[string]bool{}
	for _, t := range trades {
		if t.IsOpen() && !t.IsNoFill() {
			claimed[strings.ToUpper(t.Symbol)+"|"+strings.ToLower(t.Side)] = true
		}
	}
	for _, p := range poss {
		n := p.Notional()
		if n <= 0 {
			continue
		}
		short := shortOrRaw(p.Symbol)
		if claimed[strings.ToUpper(short)+"|"+strings.ToLower(p.Side)] {
			continue
		}
		o := orphanPos{
			Symbol: short, Side: p.Side, Qty: p.Quantity, Entry: p.EntryPrice,
			Notional: n, Leverage: p.Leverage, MarginMode: p.MarginMode,
		}
		ords, oerr := s.client.OpenOrders(ctx, p.Symbol)
		if oerr != nil {
			// Not "no orders" — an unread order book must never render as
			// naked, which is the direction that cries wolf.
			o.ReadErr = oerr.Error()
		} else {
			o.Orders = ords
			for _, ord := range ords {
				if bracket.Protects(ord, &p) {
					o.HasStop = true
					break
				}
			}
			if !o.HasStop {
				nakedN++
			}
		}
		out = append(out, o)
	}
	return out, nakedN
}

// handleVerifyExchange lists every open journal trade with its ACTUAL BingX
// position + resting orders, flagging mismatches (open trade but no position,
// or a position with no protective stop), THEN lists any exchange position the
// journal does not know about. Runs on the VPS = whitelisted IP.
func (s *server) handleVerifyExchange(c *gin.Context) {
	rows := []verifyTrade{}
	orphans := []orphanPos{}
	warnN, sessionWarnN, orphanNakedN := 0, 0, 0
	if s.client != nil {
		trades, _ := journal.ReadAll("")
		ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
		defer cancel()
		for _, t := range trades {
			if !t.IsOpen() {
				continue
			}
			sym, err := resolveWebSymbol(t.Symbol)
			if err != nil {
				continue
			}
			vt := verifyTrade{ID: t.ID, Symbol: t.Symbol, Side: t.Side, Entry: t.Entry, Stop: t.Stop, TP1: t.TP1, TP2: t.TP2}
			pos, perr := s.client.FindOpenPosition(ctx, sym, t.Side)
			if perr != nil {
				pos = nil
			}
			if pos != nil {
				vt.HasPosition = true
				vt.PosQty, vt.PosEntry = pos.Quantity, pos.EntryPrice
			}
			var ords []bingx.OpenOrder
			if got, oerr := s.client.OpenOrders(ctx, sym); oerr == nil {
				ords = got
				vt.Orders = got
				for _, o := range got {
					// A TP either says so in its type, or is a reduce-only
					// LIMIT. reduceOnly is the discriminator that matters:
					// without it a plain LIMIT matched the ENTRY order, so
					// every resting pending trade reported "🎯 tp live" on
					// the strength of its own unfilled entry.
					up := strings.ToUpper(o.Type)
					if strings.Contains(up, "TAKE_PROFIT") || (up == "LIMIT" && o.ReduceOnly) {
						vt.HasTP = true
					}
				}
			}

			vt.HasStop, vt.Warn = classifyVerify(t, pos, ords)
			if vt.Warn != "" {
				warnN++
			}
			// Cash-open-bar advisory: only meaningful once there IS a stop to
			// measure, and measured against the exchange's average fill when
			// we have it — the distance that matters is the position's, not
			// the journal's intended entry.
			if vt.Stop > 0 {
				ref := vt.PosEntry
				if ref <= 0 {
					ref = vt.Entry
				}
				if w := s.sessionStopWarning(ctx, sym, ref, vt.Stop, time.Now()); w != "" {
					vt.SessionWarn = w
					sessionWarnN++
				}
			}
			rows = append(rows, vt)
		}
		orphans, orphanNakedN = s.exchangeOrphans(ctx, trades)
	}
	c.HTML(http.StatusOK, "verify_exchange.html", gin.H{
		"Rows":         rows,
		"Orphans":      orphans,
		"OrphanNakedN": orphanNakedN,
		"WarnN":        warnN,
		"SessionWarnN": sessionWarnN,
		"NoClient":     s.client == nil,
		"UpdatedUTC":   time.Now().In(time.FixedZone("Asia/Taipei", 8*3600)).Format("2006-01-02 15:04:05 UTC+8"),
	})
}

// classifyVerify decides the stop badge and the warning banner for one row.
//
// Extracted so the handler and its tests run the SAME code. The account is
// usually flat or holding one position, so the interesting rows — a
// non-reduce-only STOP, a hedge-mode stop, a journal naming an order the
// exchange no longer has — cannot be produced on demand against the live API.
// A test that reimplemented this switch would drift from it silently, which is
// how /ops/verify shipped four defects that only appeared once a real row
// existed.
//
// "Protected" comes from package bracket, shared with the monitor daemon's
// naked-position guard, so this page and the alarm that wakes you up cannot
// disagree about what counts as a stop. The check here used to be
// strings.Contains(type, "STOP") alone — wrong in the dangerous direction,
// because a STOP that is not reduce-only (and so could OPEN a position) and
// one belonging to the opposite book in hedge mode both rendered as
// "🛡️ stop live" on the one surface meant to be trusted over the journal.
func classifyVerify(t journal.Trade, pos *bingx.Position, ords []bingx.OpenOrder) (hasStop bool, warn string) {
	d := bracket.Decide(t, pos, ords, bracket.ModeAlert)
	hasStop = d.State == bracket.StateProtected
	switch {
	case d.State == bracket.StateNaked:
		if d.GhostStopID != "" {
			return hasStop, "⚠ journal 記有停損單 " + d.GhostStopID + ",但交易所沒有這張掛單 — 裸單!"
		}
		return hasStop, "⚠ 有部位但交易所沒有停損單 — 裸單!"
	case d.State == bracket.StateStale:
		return hasStop, "⚠ journal 記為已成交,但交易所沒有部位(可能已平/已停損)"
	case pos != nil && !hasStop:
		// Decide returns skip for a trade with no journal stop, and a naked
		// position on such a trade would otherwise render clean. #62 was
		// exactly this row: 100x, no stop recorded, none on the exchange, and
		// it gave back 97% of the position's margin.
		return hasStop, "⚠ 有部位但交易所沒有停損單 — 裸單!(journal 也沒記停損價)"
	case pos == nil && !t.FilledAt.IsZero():
		// The stale-journal warning has to survive the skip states too.
		return hasStop, "⚠ journal 記為已成交,但交易所沒有部位(可能已平/已停損)"
	}
	return hasStop, ""
}
