package main

import (
	"context"
	"net/http"
	"strings"
	"time"

	"myFirstGo/trading-bot/bingx"
	"myFirstGo/trading-bot/journal"

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

// handleVerifyExchange lists every open journal trade with its ACTUAL BingX
// position + resting orders, flagging mismatches (open trade but no position,
// or a position with no protective stop). Runs on the VPS = whitelisted IP.
func (s *server) handleVerifyExchange(c *gin.Context) {
	rows := []verifyTrade{}
	warnN, sessionWarnN := 0, 0
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
			if pos, perr := s.client.FindOpenPosition(ctx, sym, t.Side); perr == nil && pos != nil {
				vt.HasPosition = true
				vt.PosQty, vt.PosEntry = pos.Quantity, pos.EntryPrice
			}
			if ords, oerr := s.client.OpenOrders(ctx, sym); oerr == nil {
				vt.Orders = ords
				for _, o := range ords {
					up := strings.ToUpper(o.Type)
					if strings.Contains(up, "STOP") {
						vt.HasStop = true
					}
					if strings.Contains(up, "TAKE_PROFIT") || up == "LIMIT" {
						vt.HasTP = true
					}
				}
			}
			// The danger case: a filled/active trade with a position but NO stop
			// on the exchange (exactly what left #60 naked).
			if vt.HasPosition && !vt.HasStop {
				vt.Warn = "⚠ 有部位但交易所沒有停損單 — 裸單!"
				warnN++
			} else if t.FilledAt.IsZero() == false && !vt.HasPosition {
				vt.Warn = "⚠ journal 記為已成交,但交易所沒有部位(可能已平/已停損)"
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
	}
	c.HTML(http.StatusOK, "verify_exchange.html", gin.H{
		"Rows":         rows,
		"WarnN":        warnN,
		"SessionWarnN": sessionWarnN,
		"NoClient":     s.client == nil,
		"UpdatedUTC":   time.Now().In(time.FixedZone("Asia/Taipei", 8*3600)).Format("2006-01-02 15:04:05 UTC+8"),
	})
}
