// Package marketdata merges quote streams from every broker into one
// canonical quote cache with a staleness machine: LIVE on WS, POLL
// when flipped to REST fallback, STALE when everything is quiet.
package marketdata

import (
	"strings"
	"sync"
	"time"

	"github.com/Abhijeetsng97/terminalTrade/internal/core"
)

// FeedState is the freshness of a feed.
type FeedState string

const (
	Live  FeedState = "LIVE"
	Poll  FeedState = "POLL"
	Stale FeedState = "STALE"
)

// quote is the cached entry.
type quote struct {
	core.Quote
}

// Cache is the canonical quote cache. WS push updates mark live;
// pollers use the same Set but the cache state machine flips per
// broker feed. Get returns the freshest quote and its state.
type Cache struct {
	mu      sync.RWMutex
	entries map[string]*quote
	// feed state per broker
	feeds map[core.Broker]FeedState
	// lastTick per broker
	lastTick map[core.Broker]time.Time
	// overall state
	state FeedState
	// page-visible cadence for staleness detection
	staleAfter time.Duration
	clock      func() time.Time
}

func NewCache() *Cache {
	return &Cache{
		entries:   make(map[string]*quote),
		feeds:     make(map[core.Broker]FeedState),
		lastTick:  make(map[core.Broker]time.Time),
		state:     Stale,
		staleAfter: 15 * time.Second,
		clock:      time.Now,
	}
}

// Set stores a quote. live=true when it came from a WS push.
func (c *Cache) Set(q core.Quote, broker core.Broker, live bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cur, ok := c.entries[q.InstrumentKey]
	if ok && cur.At.After(q.At) {
		return // older tick, ignore
	}
	c.entries[q.InstrumentKey] = &quote{Quote: q}
	if live {
		c.feeds[broker] = Live
		c.lastTick[broker] = q.At
		if c.state != Live {
			c.state = Live
		}
	}
}

// PollTick records that the REST fallback produced a value for this
// broker (feeds flipped to POLL).
func (c *Cache) PollTick(broker core.Broker) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.feeds[broker] == Live {
		c.feeds[broker] = Poll
	}
	if c.state == Live {
		c.state = Poll
	}
	c.lastTick[broker] = c.clock()
}

// MarkWSDown flips a broker's feed to POLL (REST fallback active).
func (c *Cache) MarkWSDown(broker core.Broker) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.feeds[broker] = Poll
	if c.state == Live {
		c.state = Poll
	}
}

// MarkWSUp restores LIVE on reconnect (ticks must confirm).
func (c *Cache) MarkWSUp(broker core.Broker) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.feeds[broker] = Live
}

// State returns the overall freshness, demoting to STALE if no tick
// for any live broker within staleAfter.
func (c *Cache) State() (FeedState, time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	now := c.clock()
	var newest time.Time
	for _, t := range c.lastTick {
		if t.After(newest) {
			newest = t
		}
	}
	if c.state == Live && now.Sub(newest) > c.staleAfter {
		return Stale, newest
	}
	if c.state == Poll && now.Sub(newest) > c.staleAfter {
		return Stale, newest
	}
	return c.state, newest
}

// LTP is the QuoteSource implementation for the engine.
func (c *Cache) LTP(key string) (float64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	q, ok := c.entries[key]
	if !ok || q.LTP <= 0 {
		return 0, false
	}
	return q.LTP, true
}

// Get returns a full quote.
func (c *Cache) Get(key string) (core.Quote, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	q, ok := c.entries[key]
	return q.Quote, ok
}

// FeedStateOf reports per-broker state.
func (c *Cache) FeedStateOf(b core.Broker) FeedState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.feeds[b]
}

// StartPoller runs the REST fallback for a broker at the given
// interval while the poller is running (chain 10s, positions 1s).
func (c *Cache) StartPoller(b core.Broker, interval time.Duration, fetch func() []core.Quote) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				qs := fetch()
				c.PollTick(b)
				for _, q := range qs {
					c.Set(q, b, false)
				}
			}
		}
	}()
	return func() { close(done) }
}

// String for logging.
func (s FeedState) String() string { return string(s) }

var _ = strings.TrimSpace