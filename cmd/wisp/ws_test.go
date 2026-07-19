package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dreulavelle/wisp/internal/aiostreams"
	"github.com/dreulavelle/wisp/internal/store"
	"golang.org/x/net/websocket"
)

// wsTestServer starts a.handleWS on a test server and returns its ws:// URL.
func wsTestServer(t *testing.T, a *app) string {
	t.Helper()
	srv := httptest.NewServer(websocket.Handler(a.handleWS))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// wsDial connects one client and waits until wisp has registered it, so a
// broadcast issued right after can't race ahead of the registration.
func wsDial(t *testing.T, a *app, wsURL string, want int) *websocket.Conn {
	t.Helper()
	conn, err := websocket.Dial(wsURL, "", "http://localhost/")
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	deadline := time.Now().Add(3 * time.Second)
	for {
		a.wsClientsMu.Lock()
		n := len(a.wsClients)
		a.wsClientsMu.Unlock()
		if n >= want {
			return conn
		}
		if time.Now().After(deadline) {
			t.Fatalf("ws clients registered = %d, want %d", n, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// wsRecv reads one frame and decodes it as a pin_completed event.
func wsRecv(t *testing.T, conn *websocket.Conn) wsPinMessage {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	var raw string
	if err := websocket.Message.Receive(conn, &raw); err != nil {
		t.Fatalf("ws receive: %v", err)
	}
	var msg wsPinMessage
	// A torn or interleaved write shows up here: the frame won't parse.
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("ws frame is not valid JSON (%v): %q", err, raw)
	}
	if msg.Event != "pin_completed" {
		t.Fatalf("event = %q, want pin_completed (frame %q)", msg.Event, raw)
	}
	return msg
}

// Under a concurrent broadcast every connected client must receive every event,
// exactly once, intact, and in the *same* order as every other client.
//
// The shared order is the load-bearing assertion. Each client is fed by one
// dedicated writer goroutine draining that client's own channel, and
// broadcastPinCompleted enqueues to all channels under wsClientsMu — so
// broadcasts totally order, and no two clients can disagree about the sequence.
// Delivering each broadcast from a goroutine spawned per client per event (as
// wisp did before) drops that guarantee: clients observe different interleavings
// and a burst spawns an unbounded number of goroutines.
func TestWSBroadcastConcurrentClientsAgreeOnEveryFrame(t *testing.T) {
	a := testApp(t)
	wsURL := wsTestServer(t, a)
	clientA := wsDial(t, a, wsURL, 1)
	clientB := wsDial(t, a, wsURL, 2)

	const broadcasters, perBroadcaster = 4, 5
	const total = broadcasters * perBroadcaster

	var wg sync.WaitGroup
	for b := range broadcasters {
		wg.Add(1)
		go func(b int) {
			defer wg.Done()
			for i := range perBroadcaster {
				a.broadcastPinCompleted("series", "tt1", "555", "777",
					fmt.Sprintf("shows/Demo/Season 01/Demo - S%02dE%02d - [1080p].mkv", b+1, i+1))
			}
		}(b)
	}
	wg.Wait()

	read := func(name string, conn *websocket.Conn) []string {
		t.Helper()
		got := make([]string, 0, total)
		seen := make(map[string]bool, total)
		for range total {
			msg := wsRecv(t, conn)
			if msg.IMDbID != "tt1" || msg.TMDbID != "555" || msg.TVDbID != "777" {
				t.Fatalf("client %s: ids = %q/%q/%q, want tt1/555/777", name, msg.IMDbID, msg.TMDbID, msg.TVDbID)
			}
			if seen[msg.VirtualPath] {
				t.Fatalf("client %s: duplicate frame for %q", name, msg.VirtualPath)
			}
			seen[msg.VirtualPath] = true
			got = append(got, msg.VirtualPath)
		}
		return got
	}

	gotA, gotB := read("A", clientA), read("B", clientB)
	if len(gotA) != total || len(gotB) != total {
		t.Fatalf("frames received = %d/%d, want %d each", len(gotA), len(gotB), total)
	}
	for i := range gotA {
		if gotA[i] != gotB[i] {
			t.Fatalf("clients disagree at frame %d: A=%q B=%q", i, gotA[i], gotB[i])
		}
	}
}

// The whole point of the placeholder is that the media-server plugin gets told
// when it becomes real. Resolving one on first playback goes through reResolve,
// which must broadcast pin_completed just like the eager pin does — otherwise
// the catalog keeps the 1-byte size until an unrelated full poll.
func TestReResolvePlaceholderBroadcastsPinCompleted(t *testing.T) {
	backend := wispTestBackend(t)
	defer backend.Close()

	a := testApp(t)
	a.aio = aiostreams.New(backend.URL+"/stremio/uuid/blob/manifest.json", "pw")
	a.prober = testProber()
	wsURL := wsTestServer(t, a)
	conn := wsDial(t, a, wsURL, 1)

	vpath := "movies/Inception (2010) [tmdb-27205]/Inception (2010) - [1080p].mkv"
	placeholder := store.Pin{
		MediaType: "movie", IMDbID: "tt1375666", TMDbID: "27205", TVDbID: "441314",
		Title: "Inception", Year: 2010, Quality: "1080p", VirtualPath: vpath,
		Size: 1, ResolvedAt: time.Now(),
	}
	if err := a.store.Upsert(context.Background(), placeholder); err != nil {
		t.Fatal(err)
	}

	p := placeholder
	if err := a.reResolve(context.Background(), &p); err != nil {
		t.Fatalf("reResolve: %v", err)
	}
	if p.SourceURL == "" || p.Size <= 1 {
		t.Fatalf("resolved pin = %#v, want a real SourceURL and size", p)
	}

	msg := wsRecv(t, conn)
	if msg.VirtualPath != vpath {
		t.Fatalf("broadcast path = %q, want %q", msg.VirtualPath, vpath)
	}
	if msg.MediaType != "movie" || msg.IMDbID != "tt1375666" || msg.TMDbID != "27205" || msg.TVDbID != "441314" {
		t.Fatalf("broadcast carried the wrong identity: %#v", msg)
	}
}

// A self-heal is not a placeholder transition: the path and the entry the media
// server already holds are unchanged, so it must stay silent rather than
// triggering a rescan on every flaky-upstream recovery mid-playback.
func TestReResolveSelfHealDoesNotBroadcast(t *testing.T) {
	backend := wispTestBackend(t)
	defer backend.Close()

	a := testApp(t)
	a.aio = aiostreams.New(backend.URL+"/stremio/uuid/blob/manifest.json", "pw")
	a.prober = testProber()
	wsURL := wsTestServer(t, a)
	conn := wsDial(t, a, wsURL, 1)

	vpath := "movies/Inception (2010) [tmdb-27205]/Inception (2010) - [1080p].mkv"
	pin := store.Pin{
		MediaType: "movie", IMDbID: "tt1375666", TMDbID: "27205",
		Title: "Inception", Year: 2010, Quality: "1080p", VirtualPath: vpath,
		SourceURL: "http://dead.invalid/stream", Size: 123, ResolvedAt: time.Now(),
	}
	if err := a.store.Upsert(context.Background(), pin); err != nil {
		t.Fatal(err)
	}
	if err := a.reResolve(context.Background(), &pin); err != nil {
		t.Fatalf("reResolve: %v", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := websocket.Message.Receive(conn, &raw); err == nil {
		t.Fatalf("self-heal broadcast an event: %q", raw)
	}
}

// waitForWSClients blocks until wisp holds exactly want registered clients.
func waitForWSClients(t *testing.T, a *app, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		a.wsClientsMu.Lock()
		n := len(a.wsClients)
		a.wsClientsMu.Unlock()
		if n == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ws clients = %d, want %d", n, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A client that stops talking must be evicted, not held forever. This is the
// half-open case: the connection is still open from wisp's side (no FIN ever
// arrives), so without a read deadline Receive blocks for the process lifetime
// and the conn, its writer goroutine, and its wsClients entry all leak.
func TestWSEvictsSilentClient(t *testing.T) {
	a := testApp(t)
	a.wsReadTimeoutOverride = 100 * time.Millisecond
	wsURL := wsTestServer(t, a)

	conn := wsDial(t, a, wsURL, 1)
	defer conn.Close()

	// The client never sends anything and never closes; the deadline evicts it.
	waitForWSClients(t, a, 0)

	// And a broadcast to zero clients must not panic or block on the dead entry.
	a.broadcastPinCompleted("movie", "tt1", "", "", "movies/Gone/Gone.mkv")
}

// An active client must never be evicted: every successful receive re-arms the
// deadline, so a client that keeps sending survives well past the timeout.
func TestWSKeepsActiveClientAlive(t *testing.T) {
	a := testApp(t)
	a.wsReadTimeoutOverride = 150 * time.Millisecond
	wsURL := wsTestServer(t, a)

	conn := wsDial(t, a, wsURL, 1)
	defer conn.Close()

	// Ping across ~4 timeout windows.
	for range 12 {
		if err := websocket.Message.Send(conn, "ping"); err != nil {
			t.Fatalf("keepalive send: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	a.wsClientsMu.Lock()
	n := len(a.wsClients)
	a.wsClientsMu.Unlock()
	if n != 1 {
		t.Fatalf("ws clients = %d after keepalives, want 1 (active client evicted)", n)
	}

	// Still wired up: it receives broadcasts.
	a.broadcastPinCompleted("movie", "tt1", "", "", "movies/Alive/Alive.mkv")
	if msg := wsRecv(t, conn); msg.VirtualPath != "movies/Alive/Alive.mkv" {
		t.Fatalf("path = %q", msg.VirtualPath)
	}
}
