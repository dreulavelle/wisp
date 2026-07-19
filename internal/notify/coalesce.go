package notify

import (
	"path"
	"sync"
	"time"
)

// notifyMaxWaitFactor derives the max-wait bound from the configured quiet
// period, so one knob controls both halves of the debounce. The quiet period
// absorbs the gaps *inside* a burst; the max wait caps how long a group may
// keep growing, so a continuous stream of pins can never starve it.
const notifyMaxWaitFactor = 6

// notifyMaxWaitCap is the absolute ceiling on the derived max wait, so a large
// quiet period cannot push notification latency past anything reasonable.
const notifyMaxWaitCap = 2 * time.Minute

// maxWaitFor returns the max-wait bound implied by a quiet period.
func maxWaitFor(quiet time.Duration) time.Duration {
	if d := quiet * notifyMaxWaitFactor; d < notifyMaxWaitCap {
		return d
	}
	return notifyMaxWaitCap
}

// groupKey identifies a coalescing group. Keying on the parent directory (and
// media type) is what makes one season folder collapse to one event while two
// shows resolving concurrently stay two independent events.
type groupKey struct {
	mediaType string
	dir       string
}

// group is one burst in progress.
type group struct {
	// gen distinguishes this group from a later group under the same key, so a
	// timer left over from a flushed group cannot cut a fresh group short.
	gen      uint64
	files    []string
	seen     map[string]bool
	deadline time.Time // max-wait cutoff, fixed when the group is created
	timer    *time.Timer
}

// coalescer batches Import events that share a media type and a parent
// directory into a single notification.
//
// Media servers coalesce rapid rescan requests and then scan only the path they
// kept. Because wisp's per-file events each carry one file path, every file
// coalesced away on the server side was never scanned at all — a 13-pin series
// burst landed only 3 of 7 episodes in the catalog. Batching here instead means
// the server receives one event that covers the whole burst.
//
// A group flushes once no new file has joined it for quiet, or once maxWait has
// elapsed since the group was created — whichever comes first.
//
// All methods are safe for concurrent use; pins arrive from the monitor's
// errgroup and the API path at the same time.
type coalescer struct {
	quiet   time.Duration
	maxWait time.Duration
	// emit delivers a finished batch. It is never called while mu is held.
	emit func(importBatch)

	mu     sync.Mutex
	groups map[groupKey]*group
	gen    uint64
	closed bool
}

func newCoalescer(quiet, maxWait time.Duration, emit func(importBatch)) *coalescer {
	return &coalescer{
		quiet:   quiet,
		maxWait: maxWait,
		emit:    emit,
		groups:  make(map[groupKey]*group),
	}
}

// add folds one import into its directory's group, (re)arming the flush timer.
func (c *coalescer) add(mediaType, virtualPath string) {
	key := groupKey{mediaType: mediaType, dir: path.Dir(virtualPath)}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		// Past close (shutdown in progress): never hold an event back, deliver
		// it on its own rather than parking it in a group nobody will flush.
		c.emit(importBatch{mediaType: mediaType, dir: key.dir, files: []string{virtualPath}})
		return
	}

	g, ok := c.groups[key]
	if !ok {
		c.gen++
		g = &group{gen: c.gen, seen: make(map[string]bool), deadline: time.Now().Add(c.maxWait)}
		c.groups[key] = g
	}
	if !g.seen[virtualPath] {
		g.seen[virtualPath] = true
		g.files = append(g.files, virtualPath)
	}

	// Wait for the quiet period, but never past the group's max-wait deadline.
	d := c.quiet
	if remaining := time.Until(g.deadline); remaining < d {
		d = max(remaining, 0)
	}
	if g.timer == nil {
		gen := g.gen
		g.timer = time.AfterFunc(d, func() { c.fire(key, gen) })
	} else {
		g.timer.Reset(d)
	}
	c.mu.Unlock()
}

// fire flushes the group under key, provided it is still the generation the
// timer was armed for. A stale timer (its group already flushed, and possibly
// replaced by a newer one) is a no-op.
func (c *coalescer) fire(key groupKey, gen uint64) {
	c.mu.Lock()
	g, ok := c.groups[key]
	if !ok || g.gen != gen {
		c.mu.Unlock()
		return
	}
	delete(c.groups, key)
	c.mu.Unlock()

	c.emit(importBatch{mediaType: key.mediaType, dir: key.dir, files: g.files})
}

// close flushes every pending group immediately and switches the coalescer to
// pass-through, so a pin landing during shutdown is delivered rather than lost
// with the process. It is idempotent.
func (c *coalescer) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	pending := make([]importBatch, 0, len(c.groups))
	for key, g := range c.groups {
		g.timer.Stop()
		pending = append(pending, importBatch{mediaType: key.mediaType, dir: key.dir, files: g.files})
		delete(c.groups, key)
	}
	c.mu.Unlock()

	for _, b := range pending {
		c.emit(b)
	}
}
