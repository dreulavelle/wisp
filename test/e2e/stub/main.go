// Command stub stands in for the services Wisp talks to during end-to-end
// tests.
//
// It serves two things:
//
//   - An AIOStreams-shaped /api/v1/search endpoint, so the test exercises the
//     real plugin binary without depending on a live provider. Provider health
//     is exactly the kind of external state that turns a regression suite into
//     a coin flip.
//   - A bare resolver, used by the variant of the test that runs without the
//     plugin installed.
//
// It listens on loopback inside the Silo container, which is where a real
// plugin lives: the host launches plugins as subprocesses and reaches them
// over 127.0.0.1.
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// sampleMedia is a one-second h264/aac clip, under 5KB.
//
// The stub used to hand out a URL pointing at nothing, which let the test prove
// a redirect happened and nothing more. The paths that actually OPEN the stream
// — remux, transcode, seek anchors — broke twice without the suite noticing,
// because a redirect to a dead URL looks identical to one that works. Serving
// real media lets the test follow the redirect and check that watchable bytes
// come back.
//
//go:embed sample.mp4
var sampleMedia []byte

// serveMedia serves the sample with range support, which is what a player and
// ffmpeg both do first.
func serveMedia(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "video/mp4")
	http.ServeContent(w, r, "sample.mp4", time.Time{}, bytes.NewReader(sampleMedia))
}

// searchResult mirrors the shape AIOStreams returns from /api/v1/search.
type searchResult struct {
	URL        string `json:"url"`
	Filename   string `json:"filename"`
	ParsedFile struct {
		Resolution string `json:"resolution"`
		Title      string `json:"title"`
	} `json:"parsedFile"`
}

func result(url, filename, resolution string) searchResult {
	r := searchResult{URL: url, Filename: filename}
	r.ParsedFile.Resolution = resolution
	r.ParsedFile.Title = filename
	return r
}

func main() {
	addr := flag.String("addr", "127.0.0.1:9099", "listen address")
	target := flag.String("target", "https://cdn.e2e.invalid/movie.mkv?token=e2e", "URL to hand back as a stream")
	flag.Parse()

	mux := http.NewServeMux()

	// The media the resolver points at. Everything downstream opens this.
	mux.HandleFunc("/media.mp4", serveMedia)

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// AIOStreams' native search API. Two candidates at different tiers so the
	// test can assert quality selection actually chooses rather than taking
	// whatever came first.
	mux.HandleFunc("/api/v1/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		log.Printf("search type=%s id=%s", q.Get("type"), q.Get("id"))

		payload := map[string]any{
			"success": true,
			"data": map[string]any{
				"filtered": 0,
				"results": []searchResult{
					result(*target+"&q=2160p", "E2E.Title.2160p.mkv", "2160p"),
					result(*target+"&q=1080p", "E2E.Title.1080p.mkv", "1080p"),
				},
				"errors": []any{},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	})

	// Serves the catalog feed and the binaries it points at, so the test can
	// prove a catalog install works rather than trusting a reading of Silo's
	// resolution code. Silo picks binaries[<os>/<arch>], verifies the download
	// against that checksum, then reads the real manifest from the binary.
	mux.HandleFunc("/repository.json", func(w http.ResponseWriter, _ *http.Request) {
		body, err := os.ReadFile(os.Getenv("WISP_E2E_REPOSITORY"))
		if err != nil {
			http.Error(w, "no catalog feed", http.StatusNotFound)
			return
		}
		log.Printf("served catalog feed")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})

	mux.HandleFunc("/binaries/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/binaries/")
		body, err := os.ReadFile(filepath.Join(os.Getenv("WISP_E2E_DIST"), name))
		if err != nil {
			http.Error(w, "no such binary", http.StatusNotFound)
			return
		}
		log.Printf("served binary %s (%d bytes)", name, len(body))
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
	})

	// Bare resolver for the no-plugin variant of the test.
	mux.HandleFunc("/api/v1/plugins/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("resolve %s", r.URL.Path)
		w.Header().Set("Location", *target)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusFound)
	})

	log.Printf("stub listening on %s -> %s", *addr, *target)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Printf("stub: %v", err)
		os.Exit(1)
	}
}
