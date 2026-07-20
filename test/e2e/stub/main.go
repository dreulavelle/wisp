// Command stub stands in for the Wisp plugin's resolver during end-to-end
// tests.
//
// It runs inside the Silo container on loopback, which is exactly where a real
// plugin lives: the host launches plugins as subprocesses and reaches them over
// 127.0.0.1. Running it anywhere else would not exercise the server-side hop
// that turns a host-local placeholder target into a client-reachable URL.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9099", "listen address")
	target := flag.String("target", "https://cdn.e2e.invalid/movie.mkv?token=e2e", "URL to redirect to")
	flag.Parse()

	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// Mirrors the plugin contract: answer a resolver path with 302 + Location.
	mux.HandleFunc("/api/v1/plugins/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("resolve %s", r.URL.Path)
		w.Header().Set("Location", *target)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusFound)
	})

	// Lets a test assert that a title with nothing available is reported as
	// retryable rather than broken.
	mux.HandleFunc("/unavailable/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no stream yet", http.StatusServiceUnavailable)
	})

	log.Printf("stub listening on %s -> %s", *addr, *target)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Printf("stub: %v", err)
		os.Exit(1)
	}
}
