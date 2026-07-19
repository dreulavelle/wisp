package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dreulavelle/wisp/internal/aiostreams"
)

// testProber builds a prober with production-shaped defaults for unit tests.
func testProber() *prober {
	return newProber(probeConfig{Concurrency: 8, Window: 3, Timeout: 10 * time.Second}, slog.New(slog.DiscardHandler))
}

// mediaProbe writes a 206 partial response advertising the given full size.
func mediaProbe(size string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/x-matroska")
		w.Header().Set("Content-Range", "bytes 0-0/"+size)
		w.WriteHeader(http.StatusPartialContent)
		fmt.Fprint(w, "x")
	}
}

func streamsFor(urls ...string) []aiostreams.Stream {
	out := make([]aiostreams.Stream, len(urls))
	for i, u := range urls {
		out[i] = aiostreams.Stream{URL: u, Filename: fmt.Sprintf("rank%d.mkv", i)}
	}
	return out
}

// A lower-ranked candidate that succeeds FIRST must not beat a higher-ranked one
// that succeeds later: AIOStreams' rank is authoritative (deliberate
// head-of-line). The slow top rank still wins.
func TestSelectPrefersHigherRankEvenWhenSlower(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/slow-best", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(120 * time.Millisecond)
		mediaProbe("100")(w, r)
	})
	mux.HandleFunc("/fast-worse", mediaProbe("50"))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := testProber()
	got, size, err := p.selectPlayableStream(context.Background(),
		streamsFor(srv.URL+"/slow-best", srv.URL+"/fast-worse"))
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != srv.URL+"/slow-best" || size != 100 {
		t.Fatalf("winner = %q size %d, want the slow top-ranked stream", got.URL, size)
	}
}

// The highest-ranked AVAILABLE candidate wins: a dead top rank is skipped, and
// the best surviving rank is committed even if a still-lower rank also passed.
func TestSelectHighestAvailableWins(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/dead", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "expired", http.StatusForbidden)
	})
	mux.HandleFunc("/second", mediaProbe("222"))
	mux.HandleFunc("/third", mediaProbe("333"))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := testProber()
	got, size, err := p.selectPlayableStream(context.Background(),
		streamsFor(srv.URL+"/dead", srv.URL+"/second", srv.URL+"/third"))
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != srv.URL+"/second" || size != 222 {
		t.Fatalf("winner = %q size %d, want /second", got.URL, size)
	}
}

// Per-unit in-flight probes never exceed WISP_PROBE_WINDOW.
func TestPerUnitWindowBound(t *testing.T) {
	var cur, max int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&cur, 1)
		for {
			old := atomic.LoadInt32(&max)
			if n <= old || atomic.CompareAndSwapInt32(&max, old, n) {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
		atomic.AddInt32(&cur, -1)
		http.Error(w, "no", http.StatusForbidden) // all fail → probe every candidate
	}))
	defer srv.Close()

	p := newProber(probeConfig{Concurrency: 32, Window: 3, Timeout: 5 * time.Second}, slog.New(slog.DiscardHandler))
	urls := make([]string, 12)
	for i := range urls {
		urls[i] = srv.URL
	}
	_, _, err := p.selectPlayableStream(context.Background(), streamsFor(urls...))
	if err == nil {
		t.Fatal("expected all-fail error")
	}
	if got := atomic.LoadInt32(&max); got > 3 {
		t.Fatalf("max per-unit in-flight = %d, want <= 3 (window)", got)
	}
	if got := atomic.LoadInt32(&max); got != 3 {
		t.Fatalf("max per-unit in-flight = %d, want exactly 3 (window fully used)", got)
	}
}

// Global in-flight probes across concurrent units never exceed
// WISP_PROBE_CONCURRENCY, even when demand (units x window) is higher.
func TestGlobalConcurrencyBound(t *testing.T) {
	const (
		concurrency = 8
		window      = 3
		units       = 4 // 4 x 3 = 12 demanded, capped at 8
	)
	var cur, max int32
	release := make(chan struct{})
	arrived := make(chan struct{}, units*window)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&cur, 1)
		for {
			old := atomic.LoadInt32(&max)
			if n <= old || atomic.CompareAndSwapInt32(&max, old, n) {
				break
			}
		}
		arrived <- struct{}{}
		<-release // hold the permit until the test releases
		atomic.AddInt32(&cur, -1)
		w.Header().Set("Content-Type", "video/x-matroska")
		w.Header().Set("Content-Range", "bytes 0-0/1")
		w.WriteHeader(http.StatusPartialContent)
	}))
	defer srv.Close()

	p := newProber(probeConfig{Concurrency: concurrency, Window: window, Timeout: 5 * time.Second}, slog.New(slog.DiscardHandler))
	urls := make([]string, window)
	for i := range urls {
		urls[i] = srv.URL
	}

	var wg sync.WaitGroup
	for u := 0; u < units; u++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = p.selectPlayableStream(context.Background(), streamsFor(urls...))
		}()
	}

	// Wait until the semaphore is saturated (8 probes admitted).
	for i := 0; i < concurrency; i++ {
		select {
		case <-arrived:
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d/%d probes admitted", i, concurrency)
		}
	}
	// No 9th probe may be admitted while the 8 are held.
	select {
	case <-arrived:
		t.Fatalf("a 9th probe was admitted; global cap of %d breached", concurrency)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&max); got > concurrency {
		t.Fatalf("max global in-flight = %d, want <= %d", got, concurrency)
	}
}

// The per-request network timeout starts AFTER the permit is acquired: a probe
// that waits a long time in the queue still gets its full network budget.
func TestTimeoutClockStartsAfterPermit(t *testing.T) {
	mux := http.NewServeMux()
	// Rank 0 holds the single permit for ~150ms then fails, so rank 1 waits that
	// long in the queue.
	mux.HandleFunc("/slow-fail", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond)
		http.Error(w, "no", http.StatusForbidden)
	})
	// Rank 1 needs ~120ms of network time. With a 250ms timeout counted only from
	// permit acquisition it succeeds; if the ~150ms queue wait were charged
	// against it, it would time out.
	mux.HandleFunc("/needs-time", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(120 * time.Millisecond)
		mediaProbe("999")(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newProber(probeConfig{Concurrency: 1, Window: 2, Timeout: 250 * time.Millisecond}, slog.New(slog.DiscardHandler))
	got, size, err := p.selectPlayableStream(context.Background(),
		streamsFor(srv.URL+"/slow-fail", srv.URL+"/needs-time"))
	if err != nil {
		t.Fatalf("selection failed (queue wait likely ate the timeout): %v", err)
	}
	if got.URL != srv.URL+"/needs-time" || size != 999 {
		t.Fatalf("winner = %q size %d, want /needs-time", got.URL, size)
	}
}

// A 429 stops the unit's remaining probes and surfaces Retry-After: a
// lower-ranked candidate that would otherwise pass must NOT win.
func TestRateLimitCancelsWaveAndHonorsRetryAfter(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/limited", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	// Would pass if the wave were not stopped.
	mux.HandleFunc("/would-pass", mediaProbe("500"))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newProber(probeConfig{Concurrency: 8, Window: 3, Timeout: 5 * time.Second}, slog.New(slog.DiscardHandler))
	_, _, err := p.selectPlayableStream(context.Background(),
		streamsFor(srv.URL+"/limited", srv.URL+"/would-pass"))
	var rl *rateLimitedError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want *rateLimitedError", err)
	}
	if rl.RetryAfter != 7*time.Second {
		t.Fatalf("RetryAfter = %v, want 7s", rl.RetryAfter)
	}
}

// A 206 partial body is fully drained (one byte), leaving the connection
// reusable; a 200 whose Range was ignored (a multi-GB media body) is NOT fully
// drained — only a small bounded peek is read.
func TestBodyHandlingBoundedDrainOnLargeOK(t *testing.T) {
	const declared = 200 << 20 // 200 MiB "media" body
	var written int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/x-matroska")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", declared))
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		buf := make([]byte, 32<<10)
		for {
			n, err := w.Write(buf)
			atomic.AddInt64(&written, int64(n))
			if err != nil {
				return // client closed after its bounded peek
			}
			if flusher != nil {
				flusher.Flush()
			}
			if atomic.LoadInt64(&written) >= declared {
				return
			}
		}
	}))
	defer srv.Close()

	p := testProber()
	size, err := p.probe(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if size != declared {
		t.Fatalf("size = %d, want %d (from Content-Length)", size, declared)
	}
	// Give the server goroutine a moment to observe the closed connection.
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt64(&written); got >= declared {
		t.Fatalf("server wrote %d bytes (>= full %d): media body was fully drained", got, declared)
	}
}

// TestProbeDrains206Body confirms the one-byte partial body is consumed (so the
// socket is reusable) and the size comes from Content-Range.
func TestProbeDrains206Body(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=0-0" {
			t.Errorf("Range = %q, want bytes=0-0", r.Header.Get("Range"))
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", "bytes 0-0/424242")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "y")
	}))
	defer srv.Close()

	size, err := testProber().probe(context.Background(), srv.URL)
	if err != nil || size != 424242 {
		t.Fatalf("size = %d err = %v, want 424242", size, err)
	}
}
