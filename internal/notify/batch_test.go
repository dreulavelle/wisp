package notify

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
)

// collectJSON spins up a server that decodes every POSTed body.
func collectJSON(t *testing.T, out *[]map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		*out = append(*out, payload)
		w.WriteHeader(http.StatusAccepted)
	}))
}

// A coalesced series burst is ONE webhook carrying every exact file path in the
// plural episodeFiles array.
//
// The directory must never appear as a file path: a live Silo instance rejects
// a directory there ("webhook paths matched no library folder") and queues no
// scan, while still returning 202 — so folder-scoping would silently deliver
// nothing. This test is the guard against that regression.
func TestArrImportBatchSendsEveryExactPathInOneRequest(t *testing.T) {
	var payloads []map[string]any
	server := collectJSON(t, &payloads)
	defer server.Close()

	const dir = "shows/Foo/Season 01"
	files := []string{dir + "/a.mkv", dir + "/b.mkv", dir + "/c.mkv"}

	tgt := newArrTarget(server.URL, "/mnt/wisp", slog.New(slog.DiscardHandler))
	tgt.ImportBatch(context.Background(), importBatch{
		mediaType: "series", dir: dir, files: files,
	})

	if len(payloads) != 1 {
		t.Fatalf("burst sent %d webhooks, want exactly 1", len(payloads))
	}
	if payloads[0]["eventType"] != "Download" {
		t.Fatalf("eventType = %v", payloads[0]["eventType"])
	}

	entries, ok := payloads[0]["episodeFiles"].([]any)
	if !ok {
		t.Fatalf("payload has no episodeFiles array: %#v", payloads[0])
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.(map[string]any)["path"].(string))
	}
	slices.Sort(got)
	want := []string{
		"/mnt/wisp/shows/Foo/Season 01/a.mkv",
		"/mnt/wisp/shows/Foo/Season 01/b.mkv",
		"/mnt/wisp/shows/Foo/Season 01/c.mkv",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("episodeFiles paths = %v, want every exact file %v", got, want)
	}

	// No path in the payload may be a bare directory.
	for _, p := range got {
		if p == "/mnt/wisp/"+dir {
			t.Errorf("emitted the season directory %q as a file path", p)
		}
	}
	// The singular field must not carry the directory either.
	if ef, ok := payloads[0]["episodeFile"]; ok {
		t.Errorf("payload still sets singular episodeFile = %v", ef)
	}

	series := payloads[0]["series"].(map[string]any)
	if series["path"] != "/mnt/wisp/shows/Foo" {
		t.Errorf("series.path = %v, want the show folder", series["path"])
	}
}

// A coalesced movie burst — one file per quality tier in a single folder — is
// likewise ONE webhook carrying every exact path, in the plural movieFiles
// array. Symmetric with the series case; the directory must never be a path.
func TestArrImportBatchMovieBurstSendsEveryExactPathInOneRequest(t *testing.T) {
	var payloads []map[string]any
	server := collectJSON(t, &payloads)
	defer server.Close()

	const dir = "movies/Foo (2020)"
	tgt := newArrTarget(server.URL, "/mnt/wisp", slog.New(slog.DiscardHandler))
	tgt.ImportBatch(context.Background(), importBatch{
		mediaType: "movie", dir: dir,
		files: []string{dir + "/a - [1080p].mkv", dir + "/b - [2160p].mkv"},
	})

	if len(payloads) != 1 {
		t.Fatalf("burst sent %d webhooks, want exactly 1", len(payloads))
	}
	if payloads[0]["eventType"] != "Download" {
		t.Fatalf("eventType = %v", payloads[0]["eventType"])
	}

	entries, ok := payloads[0]["movieFiles"].([]any)
	if !ok {
		t.Fatalf("payload has no movieFiles array: %#v", payloads[0])
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.(map[string]any)["path"].(string))
	}
	slices.Sort(got)
	want := []string{
		"/mnt/wisp/movies/Foo (2020)/a - [1080p].mkv",
		"/mnt/wisp/movies/Foo (2020)/b - [2160p].mkv",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("movieFiles paths = %v, want every exact file %v", got, want)
	}

	// No path may be the bare containing folder.
	for _, p := range got {
		if p == "/mnt/wisp/"+dir {
			t.Errorf("emitted the movie directory %q as a file path", p)
		}
	}
	if mf, ok := payloads[0]["movieFile"]; ok {
		t.Errorf("payload still sets singular movieFile = %v", mf)
	}

	// Context field, exactly as the probe sent it.
	movie := payloads[0]["movie"].(map[string]any)
	if movie["folderPath"] != "/mnt/wisp/movies/Foo (2020)" {
		t.Errorf("movie.folderPath = %v, want the movie folder", movie["folderPath"])
	}
}

// A lone movie pin keeps the singular movieFile payload rather than a one-entry
// plural array — the movie counterpart of the series equivalence guarantee, and
// what makes WISP_NOTIFY_DEBOUNCE=0 byte-for-byte equivalent to the old
// behavior for movies.
func TestArrImportBatchOfOneMovieKeepsSingularPayload(t *testing.T) {
	var payloads []map[string]any
	server := collectJSON(t, &payloads)
	defer server.Close()

	tgt := newArrTarget(server.URL, "/mnt/wisp", slog.New(slog.DiscardHandler))
	tgt.ImportBatch(context.Background(), importBatch{
		mediaType: "movie",
		dir:       "movies/Foo (2020)",
		files:     []string{"movies/Foo (2020)/a.mkv"},
	})

	if len(payloads) != 1 {
		t.Fatalf("sent %d webhooks, want 1", len(payloads))
	}
	movieFile, ok := payloads[0]["movieFile"].(map[string]any)
	if !ok {
		t.Fatalf("single pin did not use the singular movieFile: %#v", payloads[0])
	}
	if movieFile["path"] != "/mnt/wisp/movies/Foo (2020)/a.mkv" {
		t.Errorf("movieFile.path = %v, want the exact file", movieFile["path"])
	}
	// Fields the plain Import payload never sets must not appear.
	for _, k := range []string{"movieFiles", "movie"} {
		if _, ok := payloads[0][k]; ok {
			t.Errorf("single-file payload gained a %q field it did not have before", k)
		}
	}
}

// A lone series pin keeps the singular episodeFile payload — the shape a
// disabled debounce window produces — rather than a one-entry plural array.
// This is what makes WISP_NOTIFY_DEBOUNCE=0 byte-for-byte equivalent to the
// pre-coalescing behavior.
func TestArrImportBatchOfOneSeriesKeepsSingularPayload(t *testing.T) {
	var payloads []map[string]any
	server := collectJSON(t, &payloads)
	defer server.Close()

	tgt := newArrTarget(server.URL, "/mnt/wisp", slog.New(slog.DiscardHandler))
	tgt.ImportBatch(context.Background(), importBatch{
		mediaType: "series",
		dir:       "shows/Foo/Season 01",
		files:     []string{"shows/Foo/Season 01/a.mkv"},
	})

	if len(payloads) != 1 {
		t.Fatalf("sent %d webhooks, want 1", len(payloads))
	}
	episodeFile, ok := payloads[0]["episodeFile"].(map[string]any)
	if !ok {
		t.Fatalf("single pin did not use the singular episodeFile: %#v", payloads[0])
	}
	if episodeFile["path"] != "/mnt/wisp/shows/Foo/Season 01/a.mkv" {
		t.Errorf("episodeFile.path = %v, want the exact file", episodeFile["path"])
	}
	// Fields the plain Import payload never set must not appear.
	for _, k := range []string{"episodeFiles", "series"} {
		if _, ok := payloads[0][k]; ok {
			t.Errorf("single-file payload gained a %q field it did not have before", k)
		}
	}
}

// Jellyfin/Emby take a list natively, so a burst becomes one request carrying
// every exact path — no folder-scoping and no loss of per-file detail.
func TestMediaBrowserImportBatchSendsEveryPathInOneRequest(t *testing.T) {
	var payloads []map[string]any
	server := collectJSON(t, &payloads)
	defer server.Close()

	tgt := newMediaBrowserTarget(mediaBrowserConfig{
		flavor: "jellyfin", baseURL: server.URL, createType: "Modified", mountPath: "/mnt/wisp",
	}, slog.New(slog.DiscardHandler))
	tgt.ImportBatch(context.Background(), importBatch{
		mediaType: "series",
		dir:       "shows/Foo/Season 01",
		files:     []string{"shows/Foo/Season 01/a.mkv", "shows/Foo/Season 01/b.mkv"},
	})

	if len(payloads) != 1 {
		t.Fatalf("burst sent %d requests, want 1", len(payloads))
	}
	updates := payloads[0]["Updates"].([]any)
	if len(updates) != 2 {
		t.Fatalf("got %d updates, want 2", len(updates))
	}
	var paths []string
	for _, u := range updates {
		m := u.(map[string]any)
		paths = append(paths, m["path"].(string))
		if m["updateType"] != "Modified" {
			t.Errorf("updateType = %v, want Modified", m["updateType"])
		}
	}
	slices.Sort(paths)
	want := []string{"/mnt/wisp/shows/Foo/Season 01/a.mkv", "/mnt/wisp/shows/Foo/Season 01/b.mkv"}
	if !slices.Equal(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
}

// Plex already scans folders, so a burst collapses to the single refresh it
// would otherwise have sent N identical copies of.
func TestPlexImportBatchRefreshesFolderOnce(t *testing.T) {
	var refreshed []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections" {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer><Directory key="1"><Location path="/mnt/wisp/shows"/></Directory></MediaContainer>`))
			return
		}
		q, _ := url.ParseQuery(r.URL.RawQuery)
		refreshed = append(refreshed, q.Get("path"))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	tgt := newPlexTarget(server.URL, "tok", "/mnt/wisp", slog.New(slog.DiscardHandler))
	tgt.ImportBatch(context.Background(), importBatch{
		mediaType: "series",
		dir:       "shows/Foo/Season 01",
		files: []string{
			"shows/Foo/Season 01/a.mkv",
			"shows/Foo/Season 01/b.mkv",
			"shows/Foo/Season 01/c.mkv",
		},
	})

	if !slices.Equal(refreshed, []string{"/mnt/wisp/shows/Foo/Season 01"}) {
		t.Fatalf("refreshed = %v, want exactly one refresh of the season folder", refreshed)
	}
}
