package plugin

import (
	"context"
	"log/slog"
	"strconv"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// ScanSource reports newly written placeholders to Silo's autoscan.
//
// This replaces the webhook Wisp used to push at media servers. The inversion
// matters: the host owns the poll timer, marker persistence, path rewriting,
// validation, dedupe, and scan enqueueing, so Wisp no longer has to know how
// the media server sees its filesystem. That path-translation guesswork was the
// single most fragile part of the old design.
type ScanSource struct {
	pluginv1.UnimplementedScanSourceServer

	library *Library
	log     *slog.Logger
}

// NewScanSource returns a scan source over a library index.
func NewScanSource(library *Library, log *slog.Logger) *ScanSource {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &ScanSource{library: library, log: log}
}

// PollChanges returns placeholders written since the host's last marker.
func (s *ScanSource) PollChanges(_ context.Context, req *pluginv1.PollChangesRequest) (*pluginv1.PollChangesResponse, error) {
	raw := req.GetMarker()

	// An empty marker means "start from now, do not replay history". Returning
	// every placeholder ever written would make configuring a source enqueue a
	// full rescan of the library, which is exactly the storm autoscan exists to
	// avoid.
	if raw == "" {
		cursor := s.library.Cursor()
		s.log.Info("scansource: first poll, starting from current position", "cursor", cursor)
		return &pluginv1.PollChangesResponse{NextMarker: strconv.FormatUint(cursor, 10)}, nil
	}

	marker, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		// The host stores our marker verbatim and cannot interpret it, so a
		// corrupt one is ours to recover from. Resyncing to the current
		// position loses at most the placeholders written during the corruption
		// — far better than replaying the entire library.
		cursor := s.library.Cursor()
		s.log.Warn("scansource: unparseable marker, resyncing to current position",
			"marker", raw, "cursor", cursor)
		return &pluginv1.PollChangesResponse{NextMarker: strconv.FormatUint(cursor, 10)}, nil
	}

	items, next := s.library.Since(marker)
	if len(items) == 0 {
		return &pluginv1.PollChangesResponse{NextMarker: raw}, nil
	}

	changes := make([]*pluginv1.ScanSourceChange, 0, len(items))
	paths := make([]string, 0, len(items))
	for _, item := range items {
		paths = append(paths, item.Path)
		changes = append(changes, &pluginv1.ScanSourceChange{
			SourcePath: item.Path,
			// A placeholder is always a single file that now exists. Saying so
			// explicitly keeps the host from walking the parent directory,
			// which on a large library is a great deal of wasted work for one
			// new episode.
			Scope: pluginv1.ScanSourceChangeScope_SCAN_SOURCE_CHANGE_SCOPE_FILE,
		})
	}

	s.log.Info("scansource: reporting new placeholders",
		"count", len(paths), "from_marker", marker, "next_marker", next)

	return &pluginv1.PollChangesResponse{
		// Both fields are populated: structured changes are preferred by the
		// host, and source_paths keeps older hosts working.
		SourcePaths: paths,
		Changes:     changes,
		NextMarker:  strconv.FormatUint(next, 10),
	}, nil
}
