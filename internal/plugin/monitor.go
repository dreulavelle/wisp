package plugin

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// TaskFillEpisodes is the scheduled task id declared in the manifest.
const TaskFillEpisodes = "fill-episodes"

// Monitor keeps monitored series complete as new episodes air.
//
// On-demand playback makes this far simpler than it used to be. The old monitor
// had to resolve a stream and pin it, which meant provider calls, quality
// tiers, and backoff. Now a new episode only needs a placeholder: resolution
// happens later, if and when someone presses play. The task is therefore pure
// bookkeeping and never touches the stream provider.
type Monitor struct {
	library  *Library
	writer   *Writer
	episodes EpisodeLister
	log      *slog.Logger
}

// NewMonitor builds the scheduled-task handler.
func NewMonitor(library *Library, writer *Writer, episodes EpisodeLister, log *slog.Logger) *Monitor {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Monitor{library: library, writer: writer, episodes: episodes, log: log}
}

// series is one show Wisp already tracks, derived from its placeholders.
type series struct {
	id      MediaID
	imdb    string
	title   string
	year    int
	quality string
	have    map[[2]int]bool // season/episode pairs already written
}

// Run fills in episodes that have aired since the last pass.
//
// Implements pluginv1.ScheduledTaskServer.
func (m *Monitor) Run(ctx context.Context, req *pluginv1.RunScheduledTaskRequest) (*pluginv1.RunScheduledTaskResponse, error) {
	if key := req.GetTaskKey(); key != "" && key != TaskFillEpisodes {
		return nil, fmt.Errorf("monitor: unknown task %q", key)
	}
	if m.writer == nil || m.episodes == nil {
		m.log.Warn("monitor: not configured; skipping")
		return &pluginv1.RunScheduledTaskResponse{}, nil
	}

	shows := m.trackedSeries()
	if len(shows) == 0 {
		return &pluginv1.RunScheduledTaskResponse{}, nil
	}

	written, failed := 0, 0
	for _, s := range shows {
		// Honour cancellation between shows: the host bounds a task run, and a
		// library with many series should stop cleanly rather than be killed
		// mid-write.
		if err := ctx.Err(); err != nil {
			m.log.Warn("monitor: cancelled", "written", written)
			return &pluginv1.RunScheduledTaskResponse{}, nil
		}

		aired, err := m.episodes.ReleasedEpisodes(ctx, s.imdb)
		if err != nil {
			// One unreachable show must not stop the pass.
			m.log.Warn("monitor: enumerate episodes failed",
				"title", s.title, "id", s.id.String(), "error", err)
			failed++
			continue
		}

		for _, ep := range aired {
			if s.have[[2]int{ep.Season, ep.Episode}] {
				continue
			}
			path, err := m.writer.Write(Item{
				MediaType: "series",
				Title:     s.title,
				Year:      s.year,
				ID:        s.id,
				IMDbID:    s.imdb,
				Season:    ep.Season,
				Episode:   ep.Episode,
				Quality:   s.quality,
			})
			if err != nil {
				m.log.Warn("monitor: write failed",
					"title", s.title, "season", ep.Season, "episode", ep.Episode, "error", err)
				failed++
				continue
			}
			m.library.Add(Placeholder{
				Path: path, MediaType: "series", ID: s.id, IMDbID: s.imdb,
				Season: ep.Season, Episode: ep.Episode, Quality: s.quality,
			})
			written++
			m.log.Info("monitor: new episode",
				"title", s.title, "season", ep.Season, "episode", ep.Episode)
		}
	}

	if written > 0 || failed > 0 {
		m.log.Info("monitor: pass complete",
			"series", len(shows), "written", written, "failed", failed)
	}
	return &pluginv1.RunScheduledTaskResponse{}, nil
}

// trackedSeries derives what to monitor from the placeholders on disk.
//
// There is deliberately no separate list of monitored shows. A show is
// monitored because Wisp has written placeholders for it, so the library and
// the monitor cannot disagree — and deleting a show from the library stops it
// being monitored, which is what an operator deleting it would expect.
func (m *Monitor) trackedSeries() []series {
	byID := map[string]*series{}

	for _, p := range m.library.List() {
		if p.MediaType != "series" || p.IMDbID == "" || !p.ID.Valid() {
			continue
		}
		key := p.ID.String()
		s, ok := byID[key]
		if !ok {
			title, year := titleFromPath(p.Path)
			s = &series{
				id: p.ID, imdb: p.IMDbID, title: title, year: year,
				quality: p.Quality, have: map[[2]int]bool{},
			}
			byID[key] = s
		}
		s.have[[2]int{p.Season, p.Episode}] = true

		// Prefer the highest tier already present, so a show upgraded to 2160p
		// does not start collecting new episodes at its old tier.
		if tierRank(p.Quality) > tierRank(s.quality) {
			s.quality = p.Quality
		}
	}

	out := make([]series, 0, len(byID))
	for _, s := range byID {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id.Value < out[j].id.Value })
	return out
}

// titleFromPath recovers a show's title and year from its folder name.
//
// The placeholder URL carries identity but not the display title, and the
// folder is the only place it survives. Reconstructing it here avoids a
// metadata call purely to name a file we are about to write next to files that
// already carry the name.
func titleFromPath(path string) (title string, year int) {
	// .../Shows/<Title> (<Year>) [tvdb-N]/Season NN/<file>.strm
	parts := strings.Split(filepath.ToSlash(path), "/")
	if len(parts) < 3 {
		return "", 0
	}
	folder := parts[len(parts)-3]

	// Strip the id tag.
	if i := strings.LastIndex(folder, " ["); i > 0 {
		folder = folder[:i]
	}
	// Strip and capture the year.
	if i := strings.LastIndex(folder, " ("); i > 0 && strings.HasSuffix(folder, ")") {
		yearPart := folder[i+2 : len(folder)-1]
		folder = folder[:i]
		if n, err := parseYear(yearPart); err == nil {
			year = n
		}
	}
	return strings.TrimSpace(folder), year
}

func parseYear(s string) (int, error) {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, err
	}
	if n < 1800 || n > 2200 {
		return 0, fmt.Errorf("implausible year %d", n)
	}
	return n, nil
}

// tierRank orders quality labels so the best already in use can be picked.
func tierRank(q string) int {
	switch strings.ToLower(strings.TrimSpace(q)) {
	case "2160p", "4k", "uhd":
		return 4
	case "1080p":
		return 3
	case "720p":
		return 2
	case "":
		return 0
	default:
		return 1
	}
}

// MonitorHolder lets the host bind the scheduled task before configuration has
// arrived, since a plugin is registered with all its capability servers at
// startup but only learns its settings later.
type MonitorHolder struct {
	pluginv1.UnimplementedScheduledTaskServer
	mu      sync.RWMutex
	monitor *Monitor
}

// NewMonitorHolder returns an unconfigured holder.
func NewMonitorHolder() *MonitorHolder { return &MonitorHolder{} }

// Set swaps the active monitor. Safe to call whenever settings change.
func (h *MonitorHolder) Set(m *Monitor) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.monitor = m
}

// Run forwards to the configured monitor, or does nothing if there is none.
func (h *MonitorHolder) Run(ctx context.Context, req *pluginv1.RunScheduledTaskRequest) (*pluginv1.RunScheduledTaskResponse, error) {
	h.mu.RLock()
	m := h.monitor
	h.mu.RUnlock()

	if m == nil {
		return &pluginv1.RunScheduledTaskResponse{}, nil
	}
	return m.Run(ctx, req)
}
