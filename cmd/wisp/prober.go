package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dreulavelle/wisp/internal/aiostreams"
	"golang.org/x/sync/semaphore"
)

// probeConfig holds the clamped probe knobs (see config.Config for bounds).
type probeConfig struct {
	Concurrency int           // global cap on in-flight probe HTTP requests
	Window      int           // per-unit concurrent candidate probes
	Timeout     time.Duration // per-request network timeout, started after permit
}

// A prober verifies transport availability of ranked candidate streams. It owns
// the one global concurrency semaphore, the dedicated HTTP client, and the
// ordered-window probe logic. It never reranks, filters, or parses results — it
// only picks the highest-ranked candidate whose resolver can serve media.
type prober struct {
	cfg     probeConfig
	sem     *semaphore.Weighted
	client  *http.Client
	metrics *probeMetrics
	log     *slog.Logger
}

// newProber builds a prober with a shared, connection-pooled HTTP client. The
// client has no overall timeout (that is applied per-request after a permit is
// acquired) but bounds connection setup so a dead host fails fast.
func newProber(cfg probeConfig, log *slog.Logger) *prober {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   cfg.Concurrency,
		MaxConnsPerHost:       cfg.Concurrency,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &prober{
		cfg:     cfg,
		sem:     semaphore.NewWeighted(int64(cfg.Concurrency)),
		client:  &http.Client{Transport: transport},
		metrics: &probeMetrics{},
		log:     log,
	}
}

// probe failure reasons (also used as Prometheus label values).
const (
	reasonTimeout     = "timeout"
	reasonRateLimited = "rate_limited"
	reasonDead        = "dead"
	reasonNonMedia    = "non_media"
	reasonCanceled    = "canceled"
)

// probeError is a classified probe failure. reason drives metrics; retryAfter is
// set only for a rate-limited (429) response.
type probeError struct {
	reason     string
	retryAfter time.Duration
	err        error
}

func (e *probeError) Error() string {
	if e.err != nil {
		return fmt.Sprintf("probe %s: %v", e.reason, e.err)
	}
	return "probe " + e.reason
}

func (e *probeError) Unwrap() error { return e.err }

// rateLimitedError surfaces a provider 429 out of selectPlayableStream so callers
// can back off (and honor Retry-After) instead of treating it as "no stream".
type rateLimitedError struct{ RetryAfter time.Duration }

func (e *rateLimitedError) Error() string { return "probe rate limited by provider" }

// selectPlayableStream returns the highest-ranked candidate whose resolver can
// serve media, verifying availability in AIOStreams rank order. It probes up to
// cfg.Window candidates concurrently (a rank-preserving sliding window) and
// commits a candidate only once every higher-ranked candidate has finished
// (failed) — so a lower-ranked probe that returns first can never beat a
// still-pending higher rank. A 429 cancels the unit's remaining probes.
func (p *prober) selectPlayableStream(ctx context.Context, streams []aiostreams.Stream) (aiostreams.Stream, int64, error) {
	n := len(streams)
	if n == 0 {
		return aiostreams.Stream{}, 0, errors.New("no candidates to probe")
	}
	p.metrics.selections.Add(1)

	// Cancel outstanding probes as soon as a winner is decided or a 429 stops the
	// wave; this aborts in-flight lower-ranked requests and unblocks any queued
	// on the semaphore.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		size int64
		err  error
	}
	results := make([]result, n) // results[i] written by candidate i's goroutine,
	// read by the coordinator only after receiving i on done (a happens-before).
	done := make(chan int, n)
	var wg sync.WaitGroup

	inflight := 0
	next := 0     // next unlaunched candidate index
	bestPass := n // smallest index observed to pass (n = none yet)
	launch := func() {
		idx := next
		next++
		inflight++
		wg.Add(1)
		go func() {
			defer wg.Done()
			size, err := p.probe(ctx, streams[idx].URL)
			results[idx] = result{size: size, err: err}
			done <- idx
		}()
	}

	// Prime the window.
	for inflight < p.cfg.Window && next < n {
		launch()
	}

	// Coordinator: owns completed/cursor/winner state (single goroutine), so only
	// atomics/channels cross goroutine boundaries.
	completed := make([]bool, n)
	cursor := 0 // lowest index not yet known-failed
	winner := -1
	var winSize int64
	rateLimited := false
	var retryAfter time.Duration
	var lastErr error

	for inflight > 0 {
		idx := <-done
		inflight--
		completed[idx] = true
		r := results[idx]

		if r.err == nil {
			if idx < bestPass {
				bestPass = idx
			}
		} else {
			lastErr = r.err
			var pe *probeError
			if errors.As(r.err, &pe) && pe.reason == reasonRateLimited {
				// Provider pressure: stop the whole wave now (cancelling any
				// in-flight probe, higher- or lower-ranked) and surface Retry-After.
				// A lower-ranked candidate that already passed must NOT win.
				rateLimited = true
				retryAfter = pe.retryAfter
				cancel()
				break
			}
			if next < n && next < bestPass {
				// Slide the window: only a failure of a still-relevant candidate
				// (better-ranked than any pass so far) launches the next one.
				launch()
			}
		}

		// Commit the best-ranked pass, but only once every higher-ranked candidate
		// has finished (failed) — head-of-line by design; rank is authoritative.
		for cursor < n && completed[cursor] {
			if results[cursor].err == nil {
				winner = cursor
				winSize = results[cursor].size
				cancel()
				break
			}
			cursor++
		}
		if winner != -1 {
			break
		}
	}

	cancel()
	wg.Wait()

	switch {
	case winner != -1:
		return streams[winner], winSize, nil
	case rateLimited:
		return aiostreams.Stream{}, 0, &rateLimitedError{RetryAfter: retryAfter}
	default:
		return aiostreams.Stream{}, 0, fmt.Errorf("all %d candidates failed probing: %w", n, lastErr)
	}
}

// bounded drains: after reading enough to allow connection reuse for a tiny
// (partial) body, close. For a 200 that ignored Range the body may be the whole
// media file — read only a small bounded amount, never the full body.
const (
	partialDrainLimit = 1 << 10 // ample for a one-byte 206 body
	okDrainLimit      = 2 << 10 // bounded peek for a Range-ignored 200
)

// probe issues a one-byte ranged GET (AIOStreams resolver permalinks do not
// support HEAD) and reports the full media size. It acquires the global
// concurrency permit around the HTTP operation — honoring ctx while queued — and
// starts the per-request network timeout only AFTER the permit is held, so queue
// wait never consumes the network budget. Every probe path (normal pin and
// self-heal re-resolve) funnels through here, sharing one limit.
func (p *prober) probe(ctx context.Context, rawURL string) (int64, error) {
	p.metrics.candidates.Add(1)

	queueStart := time.Now()
	if err := p.sem.Acquire(ctx, 1); err != nil {
		// Cancelled/queued-out before doing any network work.
		return 0, &probeError{reason: reasonCanceled, err: err}
	}
	defer p.sem.Release(1)
	p.metrics.addQueueWait(time.Since(queueStart))
	p.metrics.requests.Add(1)

	reqCtx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()

	start := time.Now()
	size, err := p.do(reqCtx, rawURL)
	p.metrics.addProbeDuration(time.Since(start))
	if err != nil {
		p.metrics.recordFailure(classifyProbe(reqCtx, ctx, err))
	}
	return size, err
}

// do performs the ranged GET and body handling. It is separated from probe so
// the semaphore/timeout accounting stays readable.
func (p *prober) do(ctx context.Context, rawURL string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "wisp")
	req.Header.Set("Range", "bytes=0-0")
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		// Provider pressure: surface it distinctly so the wave can be stopped and
		// Retry-After honored. Drain a little so the connection can be reused.
		_, _ = io.CopyN(io.Discard, resp.Body, okDrainLimit)
		return 0, &probeError{
			reason:     reasonRateLimited,
			retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			err:        fmt.Errorf("upstream returned HTTP %d", resp.StatusCode),
		}
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		_, _ = io.CopyN(io.Discard, resp.Body, okDrainLimit)
		return 0, &probeError{reason: reasonDead, err: fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)}
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.HasPrefix(contentType, "text/") || strings.Contains(contentType, "json") {
		_, _ = io.CopyN(io.Discard, resp.Body, okDrainLimit)
		return 0, &probeError{reason: reasonNonMedia, err: fmt.Errorf("upstream returned non-media content type %q", contentType)}
	}

	if resp.StatusCode == http.StatusPartialContent {
		// Small partial body: drain the one byte fully so the socket is reusable.
		_, _ = io.CopyN(io.Discard, resp.Body, partialDrainLimit)
		size, err := contentRangeSize(resp.Header.Get("Content-Range"))
		if err != nil {
			return 0, &probeError{reason: reasonDead, err: err}
		}
		return size, nil
	}

	// 200: the server ignored Range and the body may be the entire media file.
	// Read only a bounded peek then close — never download the full body. Losing
	// socket reuse is preferable to fetching gigabytes of media.
	_, _ = io.CopyN(io.Discard, resp.Body, okDrainLimit)
	if resp.ContentLength <= 0 {
		return 0, &probeError{reason: reasonDead, err: fmt.Errorf("upstream did not report a size (HTTP %d)", resp.StatusCode)}
	}
	return resp.ContentLength, nil
}

// classifyProbe extracts the failure reason from a probe error. A per-request
// deadline maps to a timeout; a parent-cancellation (winner decided or 429 wave
// stop) maps to canceled; an already-classified probeError keeps its reason.
func classifyProbe(reqCtx, parentCtx context.Context, err error) string {
	var pe *probeError
	if errors.As(err, &pe) {
		return pe.reason
	}
	if parentCtx.Err() != nil {
		return reasonCanceled
	}
	if reqCtx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		return reasonTimeout
	}
	return reasonDead
}

// parseRetryAfter reads a Retry-After header in delta-seconds or HTTP-date form,
// returning 0 when absent or unparseable.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// probeMetrics are the observable static-limit counters for probing. Durations
// are exposed summary-style (sum + count) so an operator can chart averages
// without a histogram dependency.
type probeMetrics struct {
	selections     atomic.Int64 // selectPlayableStream invocations
	candidates     atomic.Int64 // candidate probes started (launched)
	requests       atomic.Int64 // probe HTTP requests issued (permit held)
	failTimeout    atomic.Int64
	failRateLimit  atomic.Int64
	failDead       atomic.Int64
	failNonMedia   atomic.Int64
	failCanceled   atomic.Int64
	queueWaitNanos atomic.Int64
	queueWaitCount atomic.Int64
	probeNanos     atomic.Int64
	probeCount     atomic.Int64
}

func (m *probeMetrics) addQueueWait(d time.Duration) {
	m.queueWaitNanos.Add(int64(d))
	m.queueWaitCount.Add(1)
}

func (m *probeMetrics) addProbeDuration(d time.Duration) {
	m.probeNanos.Add(int64(d))
	m.probeCount.Add(1)
}

func (m *probeMetrics) recordFailure(reason string) {
	switch reason {
	case reasonTimeout:
		m.failTimeout.Add(1)
	case reasonRateLimited:
		m.failRateLimit.Add(1)
	case reasonNonMedia:
		m.failNonMedia.Add(1)
	case reasonCanceled:
		m.failCanceled.Add(1)
	default:
		m.failDead.Add(1)
	}
}

// probeSnapshot is a point-in-time read of the probe counters for /metrics.
type probeSnapshot struct {
	Selections     int64
	Candidates     int64
	Requests       int64
	FailTimeout    int64
	FailRateLimit  int64
	FailDead       int64
	FailNonMedia   int64
	FailCanceled   int64
	QueueWaitNanos int64
	QueueWaitCount int64
	ProbeNanos     int64
	ProbeCount     int64
}

func (m *probeMetrics) snapshot() probeSnapshot {
	return probeSnapshot{
		Selections:     m.selections.Load(),
		Candidates:     m.candidates.Load(),
		Requests:       m.requests.Load(),
		FailTimeout:    m.failTimeout.Load(),
		FailRateLimit:  m.failRateLimit.Load(),
		FailDead:       m.failDead.Load(),
		FailNonMedia:   m.failNonMedia.Load(),
		FailCanceled:   m.failCanceled.Load(),
		QueueWaitNanos: m.queueWaitNanos.Load(),
		QueueWaitCount: m.queueWaitCount.Load(),
		ProbeNanos:     m.probeNanos.Load(),
		ProbeCount:     m.probeCount.Load(),
	}
}
