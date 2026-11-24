package venue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned by Lookup for an ID no registered venue lists.
var ErrNotFound = errors.New("instrument not found")

// Registry dispatches an ID to its venue and holds the catalog.
//
// The catalog is the last SUCCESSFUL Instruments result per venue: a failed
// refresh keeps the previous list rather than emptying it, and the lists are
// mirrored to a file on disk so a restart while the exchange is unreachable
// still has a catalog (design §6).
type Registry struct {
	cachePath string // "" = no disk mirror
	logf      func(string, ...any)

	mu      sync.RWMutex
	venues  map[string]MarketData
	lists   map[string]venueList // venue → last good list
	byID    map[ID]Instrument
	ordered []Instrument // all instruments, search order with an empty query
}

type venueList struct {
	FetchedAt   time.Time    `json:"fetched_at"`
	Instruments []Instrument `json:"instruments"`
}

type cacheFile struct {
	SavedAt time.Time            `json:"saved_at"`
	Venues  map[string]venueList `json:"venues"`
}

// NewRegistry returns an empty registry. cachePath may be "" (no disk mirror);
// logf may be nil.
func NewRegistry(cachePath string, logf func(string, ...any)) *Registry {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Registry{
		cachePath: cachePath,
		logf:      logf,
		venues:    map[string]MarketData{},
		lists:     map[string]venueList{},
		byID:      map[ID]Instrument{},
	}
}

// Register adds a venue. Call before Refresh / LoadCache.
func (r *Registry) Register(md MarketData) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.venues[strings.ToLower(md.Venue())] = md
}

// Run loads the disk cache, refreshes every venue, then refreshes again every
// `every` until ctx ends. Meant to be started with `go`.
func (r *Registry) Run(ctx context.Context, every time.Duration) {
	if err := r.LoadCache(); err != nil {
		r.logf("[venue] cache: %v", err)
	}
	r.Refresh(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Refresh(ctx)
		}
	}
}

// Refresh fetches every venue's instruments. A venue that fails keeps its
// previous list. Returns the per-venue errors joined (nil when all worked).
func (r *Registry) Refresh(ctx context.Context) error {
	r.mu.RLock()
	venues := make([]MarketData, 0, len(r.venues))
	for _, md := range r.venues {
		venues = append(venues, md)
	}
	r.mu.RUnlock()

	var errs []error
	changed := false
	for _, md := range venues {
		fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		list, err := md.Instruments(fctx)
		cancel()
		if err == nil && len(list) == 0 {
			// An empty list from a venue that had one is almost certainly an
			// upstream fault, not every market closing at once.
			err = errors.New("empty instrument list")
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", md.Venue(), err))
			r.logf("[venue] refresh %s failed, keeping previous list: %v", md.Venue(), err)
			continue
		}
		r.mu.Lock()
		r.lists[strings.ToLower(md.Venue())] = venueList{FetchedAt: time.Now().UTC(), Instruments: list}
		r.mu.Unlock()
		changed = true
		r.logf("[venue] %s: %d instruments", md.Venue(), len(list))
	}
	if changed {
		r.rebuild()
		if err := r.saveCache(); err != nil {
			r.logf("[venue] save cache: %v", err)
		}
	}
	return errors.Join(errs...)
}

// LoadCache reads the disk mirror. Venues not registered are ignored, and a
// venue's list already fetched this run is not overwritten. A missing file is
// not an error.
func (r *Registry) LoadCache() error {
	if r.cachePath == "" {
		return nil
	}
	b, err := os.ReadFile(r.cachePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var cf cacheFile
	if err := json.Unmarshal(b, &cf); err != nil {
		return fmt.Errorf("decode %s: %w", r.cachePath, err)
	}
	r.mu.Lock()
	for v, vl := range cf.Venues {
		md, ok := r.venues[v]
		if !ok {
			continue
		}
		if _, have := r.lists[v]; have {
			continue
		}
		if t, ok := md.(Tradability); ok {
			for i := range vl.Instruments {
				vl.Instruments[i].Tradable = t.IsTradable(vl.Instruments[i].Code)
			}
		}
		r.lists[v] = vl
	}
	r.mu.Unlock()
	r.rebuild()
	return nil
}

func (r *Registry) saveCache() error {
	if r.cachePath == "" {
		return nil
	}
	r.mu.RLock()
	cf := cacheFile{SavedAt: time.Now().UTC(), Venues: maps.Clone(r.lists)}
	r.mu.RUnlock()
	b, err := json.Marshal(cf)
	if err != nil {
		return err
	}
	// Write-then-rename so a crash mid-write never leaves a truncated cache
	// that the next start would fail to decode.
	tmp, err := os.CreateTemp(filepath.Dir(r.cachePath), ".instruments-*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o664); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), r.cachePath)
}

// rebuild recomputes the ID index and the default order from r.lists.
func (r *Registry) rebuild() {
	r.mu.Lock()
	defer r.mu.Unlock()
	byID := map[ID]Instrument{}
	var all []Instrument
	for _, vl := range r.lists {
		for _, in := range vl.Instruments {
			in.ID = MakeID(in.Venue, in.Code)
			byID[in.ID] = in
			all = append(all, in)
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return lessDefault(all[i], all[j]) })
	r.byID, r.ordered = byID, all
}

func lessDefault(a, b Instrument) bool {
	if a.Tradable != b.Tradable {
		return a.Tradable
	}
	if ra, rb := classRank[a.Class], classRank[b.Class]; ra != rb {
		return ra < rb
	}
	if a.Display != b.Display {
		return a.Display < b.Display
	}
	return a.ID < b.ID
}

// Lookup resolves an ID (any case, prefix optional — see ParseID) to its
// instrument and the venue that serves it.
func (r *Registry) Lookup(s string) (Instrument, MarketData, error) {
	id, err := ParseID(s)
	if err != nil {
		return Instrument{}, nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	md, ok := r.venues[id.Venue()]
	if !ok {
		return Instrument{}, nil, fmt.Errorf("unknown venue %q", id.Venue())
	}
	in, ok := r.byID[id]
	if !ok {
		return Instrument{}, nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return in, md, nil
}

// Len is the number of instruments in the catalog.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.ordered)
}

// Search matches q against Display, Code and Asset, case-insensitively.
// Order: tradable first, then exact matches, then asset class (crypto → US
// stock → commodity → index → FX), then prefix before substring matches.
// An empty q returns the default order. limit <= 0 means no limit.
func (r *Registry) Search(q string, limit int) []Instrument {
	q = strings.ToUpper(strings.TrimSpace(q))
	r.mu.RLock()
	defer r.mu.RUnlock()
	if q == "" {
		return clip(append([]Instrument(nil), r.ordered...), limit)
	}
	type hit struct {
		in    Instrument
		exact bool
		pre   bool
	}
	var hits []hit
	for _, in := range r.ordered {
		exact, pre, ok := false, false, false
		for _, f := range []string{in.Display, in.Code, in.Asset} {
			f = strings.ToUpper(f)
			if f == "" {
				continue
			}
			switch {
			case f == q:
				exact, pre, ok = true, true, true
			case strings.HasPrefix(f, q):
				pre, ok = true, true
			case strings.Contains(f, q):
				ok = true
			}
		}
		if ok {
			hits = append(hits, hit{in, exact, pre})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if a.in.Tradable != b.in.Tradable {
			return a.in.Tradable
		}
		if a.exact != b.exact {
			return a.exact
		}
		if ra, rb := classRank[a.in.Class], classRank[b.in.Class]; ra != rb {
			return ra < rb
		}
		if a.pre != b.pre {
			return a.pre
		}
		return false // r.ordered's order otherwise
	})
	out := make([]Instrument, len(hits))
	for i, h := range hits {
		out[i] = h.in
	}
	return clip(out, limit)
}

func clip(s []Instrument, limit int) []Instrument {
	if limit > 0 && len(s) > limit {
		return s[:limit]
	}
	return s
}
