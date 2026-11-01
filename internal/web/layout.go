package web

import (
	"sync"
	"time"

	"github.com/mihai-max/ao3/internal/archive"
)

// layoutCache holds the result of scanning the data folder. Scanning reads
// the data folder, which may be a network mount where every read is a slow
// remote request, so pages never scan themselves: they show the last result
// and a refresh runs in the background.
type layoutCache struct {
	dataDir string
	maxAge  time.Duration

	mu         sync.Mutex
	layout     archive.Layout
	notes      map[string]string
	err        error
	at         time.Time
	refreshing bool
	done       chan struct{} // closed when the current refresh finishes
}

type layoutSnapshot struct {
	Layout  archive.Layout
	Notes   map[string]string
	Err     error
	Ready   bool // at least one scan has finished
	Scanned time.Time
}

func newLayoutCache(dataDir string) *layoutCache {
	return &layoutCache{dataDir: dataDir, maxAge: 10 * time.Minute}
}

// get returns the cached scan immediately and starts a refresh when the
// cache is empty or stale.
func (c *layoutCache) get() layoutSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() || time.Since(c.at) > c.maxAge {
		c.startLocked()
	}
	return layoutSnapshot{Layout: c.layout, Notes: c.notes, Err: c.err, Ready: !c.at.IsZero(), Scanned: c.at}
}

// refresh starts a scan now (unless one is running) and returns a channel
// that is closed when it finishes.
func (c *layoutCache) refresh() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.startLocked()
}

func (c *layoutCache) startLocked() <-chan struct{} {
	if c.refreshing {
		return c.done
	}
	c.refreshing = true
	c.done = make(chan struct{})
	done := c.done
	go func() {
		layout, err := archive.Scan(c.dataDir)
		notes := archive.Notes(c.dataDir)
		c.mu.Lock()
		c.layout, c.notes, c.err, c.at = layout, notes, err, time.Now()
		c.refreshing = false
		c.mu.Unlock()
		close(done)
	}()
	return done
}
