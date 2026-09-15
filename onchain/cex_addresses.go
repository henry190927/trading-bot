package onchain

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// CEXAddress is one row of the "known CEX deposit/hot wallet" registry.
// Address is lowercase 0x-prefixed for consistent lookup.
type CEXAddress struct {
	Chain    Chain  `json:"chain"`
	Address  string `json:"address"`
	Exchange string `json:"exchange"` // "Binance" / "BingX" / etc
	Type     string `json:"type"`     // "hot" / "cold"
}

//go:embed cex_addresses_seed.json
var seedJSON []byte

// CEXRegistry is a mutex-guarded lookup of (chain, address) -> exchange
// name. Populated from the embedded seed on startup, optionally refreshed
// from CEX_ADDRESSES_URL every registryRefreshTTL.
//
// The lookup map keys are "chain|lower(address)" so a single map covers
// all chains without nested indirection.
type CEXRegistry struct {
	mu        sync.RWMutex
	entries   map[string]CEXAddress // key = "chain|lower_addr"
	lastLoad  time.Time
	lastError string
	source    string // "embed" | "remote"
}

const registryRefreshTTL = 24 * time.Hour

// NewCEXRegistry constructs the registry from the embedded seed and, if
// CEX_ADDRESSES_URL is set, kicks off a background refresh from that URL.
// Returns immediately with the seed loaded — remote fetch runs async so
// it doesn't block startup.
func NewCEXRegistry() *CEXRegistry {
	r := &CEXRegistry{
		entries: map[string]CEXAddress{},
		source:  "embed",
	}
	r.loadFromBytes(seedJSON, "embed")

	if url := strings.TrimSpace(os.Getenv("CEX_ADDRESSES_URL")); url != "" {
		go r.refreshLoop(url)
	}
	return r
}

func (r *CEXRegistry) loadFromBytes(b []byte, source string) {
	var list []CEXAddress
	if err := json.Unmarshal(b, &list); err != nil {
		r.mu.Lock()
		r.lastError = fmt.Sprintf("parse %s failed: %v", source, err)
		r.mu.Unlock()
		return
	}
	next := make(map[string]CEXAddress, len(list))
	for _, e := range list {
		if e.Chain == "" || e.Address == "" {
			continue
		}
		key := string(e.Chain) + "|" + strings.ToLower(e.Address)
		e.Address = strings.ToLower(e.Address)
		next[key] = e
	}
	r.mu.Lock()
	r.entries = next
	r.lastLoad = time.Now()
	r.source = source
	r.lastError = ""
	r.mu.Unlock()
	log.Printf("[onchain/cex] loaded %d addresses from %s", len(next), source)
}

// refreshLoop pulls from the remote URL immediately + on registryRefreshTTL
// ticks. Failures fall back silently to whatever's already in memory.
func (r *CEXRegistry) refreshLoop(url string) {
	fetch := func() {
		client := &http.Client{Timeout: 30 * time.Second}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			r.mu.Lock()
			r.lastError = fmt.Sprintf("remote fetch %s: %v", url, err)
			r.mu.Unlock()
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			r.mu.Lock()
			r.lastError = fmt.Sprintf("remote fetch %s: %v", url, err)
			r.mu.Unlock()
			log.Printf("[onchain/cex] remote fetch failed: %v", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			r.mu.Lock()
			r.lastError = fmt.Sprintf("remote fetch %s: http %d", url, resp.StatusCode)
			r.mu.Unlock()
			log.Printf("[onchain/cex] remote fetch http %d", resp.StatusCode)
			return
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			r.mu.Lock()
			r.lastError = err.Error()
			r.mu.Unlock()
			return
		}
		r.loadFromBytes(body, "remote:"+url)
	}

	fetch()
	tick := time.NewTicker(registryRefreshTTL)
	defer tick.Stop()
	for range tick.C {
		fetch()
	}
}

// Lookup returns the CEX entry (and true) if the given (chain, address)
// is a known exchange wallet, or the zero value + false otherwise.
// Address is normalized to lowercase internally.
func (r *CEXRegistry) Lookup(chain Chain, addr string) (CEXAddress, bool) {
	key := string(chain) + "|" + strings.ToLower(addr)
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[key]
	return e, ok
}

// Stats returns a small snapshot for the ops UI / logging.
func (r *CEXRegistry) Stats() (count int, lastLoad time.Time, source, lastError string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries), r.lastLoad, r.source, r.lastError
}
