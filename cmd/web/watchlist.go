package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/henry190927/trading-bot/journal"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/venue"
)

// Symbol search and the watchlist (docs/symbol_catalog_design.md §7, P2).
//
// The watchlist is one JSON file on the server next to the journal (§11.1),
// so a phone and a laptop see the same list and the AI / monitor can read
// it. It holds canonical Keys (viewable.go): "BTC" for a list symbol,
// "bingx:NCSKTSLA2USD-USDT" for a catalog one. It replaces the per-browser
// ☆ favourites, which a client migrates once on first load.
//
// Being on the watchlist changes what is SHOWN, never what may be traded:
// every entry is checked with resolveViewable, and trading paths do not
// read this file.

const maxWatch = 40

// watchMu serialises read-modify-write of the watchlist file.
var watchMu sync.Mutex

type watchFile struct {
	Items     []string  `json:"items"`
	UpdatedAt time.Time `json:"updated_at"`
}

// watchlistPath: WATCHLIST_PATH, or watchlist.json next to the journal.
func watchlistPath() string {
	if p := strings.TrimSpace(os.Getenv("WATCHLIST_PATH")); p != "" {
		return p
	}
	return filepath.Join(filepath.Dir(journal.DefaultPath()), "watchlist.json")
}

// readWatchKeys returns the stored keys. A missing file is an empty list.
func readWatchKeys() ([]string, error) {
	b, err := os.ReadFile(watchlistPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var wf watchFile
	if err := json.Unmarshal(b, &wf); err != nil {
		return nil, fmt.Errorf("decode watchlist: %w", err)
	}
	return wf.Items, nil
}

func writeWatchKeys(keys []string) error {
	b, err := json.MarshalIndent(watchFile{Items: keys, UpdatedAt: time.Now().UTC()}, "", "  ")
	if err != nil {
		return err
	}
	path := watchlistPath()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".watchlist-*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmp.Name(), 0o664)); err != nil {
		_ = os.Remove(tmp.Name()) // best-effort cleanup of the temp file
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// symItem is one symbol as the UI shows it.
type symItem struct {
	Key      string      `json:"key"`
	Display  string      `json:"display"`
	Class    venue.Class `json:"class,omitempty"`
	Venue    string      `json:"venue,omitempty"`
	Tradable bool        `json:"tradable"`
	// Stale marks a stored key that no longer resolves (delisted, or the
	// catalog has not loaded yet). Kept so it can still be removed.
	Stale bool `json:"stale,omitempty"`
}

// watchItems resolves the stored keys for display.
func (s *server) watchItems() ([]symItem, error) {
	keys, err := readWatchKeys()
	if err != nil {
		return nil, err
	}
	out := make([]symItem, 0, len(keys))
	for _, k := range keys {
		vs, err := s.resolveViewable(k)
		if err != nil {
			out = append(out, symItem{Key: k, Display: k, Stale: true})
			continue
		}
		out = append(out, symItem{Key: vs.Key, Display: vs.Display, Class: vs.Class, Venue: "bingx", Tradable: vs.Tradable})
	}
	return out, nil
}

// handleWatchlistGet — GET /api/watchlist
func (s *server) handleWatchlistGet(c *gin.Context) {
	items, err := s.watchItems()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// handleWatchlistPost — POST /api/watchlist {"symbol": "...", "action": "add"|"remove"}
// Answers with the whole list, so a client never has to merge.
func (s *server) handleWatchlistPost(c *gin.Context) {
	var in struct {
		Symbol string `json:"symbol"`
		Action string `json:"action"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	in.Symbol = strings.TrimSpace(in.Symbol)
	if in.Action != "add" && in.Action != "remove" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "action must be add or remove"})
		return
	}

	watchMu.Lock()
	keys, err := readWatchKeys()
	if err != nil {
		watchMu.Unlock()
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Removal matches the stored key as given first, so a stale entry that no
	// longer resolves can still be taken off.
	key := in.Symbol
	if vs, rerr := s.resolveViewable(in.Symbol); rerr == nil {
		key = vs.Key
	} else if in.Action == "add" {
		watchMu.Unlock()
		c.JSON(http.StatusBadRequest, gin.H{"error": rerr.Error()})
		return
	}
	idx := -1
	for i, k := range keys {
		if strings.EqualFold(k, key) || strings.EqualFold(k, in.Symbol) {
			idx = i
			break
		}
	}
	changed := false
	switch {
	case in.Action == "add" && idx < 0:
		if len(keys) >= maxWatch {
			watchMu.Unlock()
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("watchlist is full (%d)", maxWatch)})
			return
		}
		keys, changed = append(keys, key), true
	case in.Action == "remove" && idx >= 0:
		keys, changed = append(keys[:idx], keys[idx+1:]...), true
	}
	if changed {
		if err := writeWatchKeys(keys); err != nil {
			watchMu.Unlock()
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		// The strip reads the watchlist through the ticker snapshot; drop
		// the cached one so the next poll shows the change.
		tickerCacheMu.Lock()
		tickerCache = tickerCacheT{}
		tickerCacheMu.Unlock()
	}
	watchMu.Unlock()
	s.handleWatchlistGet(c)
}

// watchTickerKeys is the watchlist entries the ticker strip must add rows
// for: viewable, not already on the list strip, deduplicated.
func (s *server) watchTickerKeys() []viewSym {
	keys, err := readWatchKeys()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []viewSym
	for _, k := range keys {
		vs, err := s.resolveViewable(k)
		if err != nil || vs.Tradable || seen[vs.Key] {
			continue
		}
		seen[vs.Key] = true
		out = append(out, vs)
	}
	return out
}

// instrumentKey is the canonical Key of a catalog entry: a list symbol's
// short name, otherwise its venue ID.
func instrumentKey(in venue.Instrument) string {
	if in.Venue == "bingx" {
		if short := market.Short(market.Symbol(in.Code)); short != "" {
			return short
		}
	}
	return string(in.ID)
}

// handleInstruments — GET /api/instruments?q=&limit=
// The ⌘K symbol search. With no catalog loaded it still finds list symbols.
func (s *server) handleInstruments(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	limit := 20
	if v, err := strconv.Atoi(c.Query("limit")); err == nil && v > 0 {
		limit = min(v, 50)
	}
	var out []symItem
	if s.venues != nil && s.venues.Len() > 0 {
		for _, in := range s.venues.Search(q, limit) {
			out = append(out, symItem{Key: instrumentKey(in), Display: in.Display, Class: in.Class,
				Venue: in.Venue, Tradable: market.Short(market.Symbol(in.Code)) != "" && in.Venue == "bingx"})
		}
	} else {
		uq := strings.ToUpper(q)
		for _, sh := range uiSymbols {
			if uq == "" || strings.Contains(sh, uq) {
				out = append(out, symItem{Key: sh, Display: sh, Venue: "bingx", Tradable: true})
			}
			if len(out) >= limit {
				break
			}
		}
	}
	if out == nil {
		out = []symItem{}
	}
	c.JSON(http.StatusOK, gin.H{"items": out, "catalog": s.venues != nil && s.venues.Len() > 0})
}
