package plugin

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

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

// marker encodes a position as "<epoch>:<seq>".
//
// The epoch is what makes a reset detectable. Sequence numbers restart at zero
// with the process and Rebuild re-derives them from the files still on disk, so
// a bare number is ambiguous: the host cannot tell "position 40 of the run that
// issued it" from "position 40 of a later, shorter run". Getting that wrong is
// silent — the stored marker outruns the cursor and autoscan simply stops
// hearing about new placeholders.
func (s *ScanSource) marker(seq uint64) string {
	return strconv.FormatUint(s.library.Epoch(), 10) + ":" + strconv.FormatUint(seq, 10)
}

// resync returns a response that abandons the host's position for our current
// one. Losing the placeholders written during the confusion beats either
// replaying the whole library or going permanently deaf.
func (s *ScanSource) resync(reason, given string) *pluginv1.PollChangesResponse {
	cursor := s.library.Cursor()
	s.log.Warn("scansource: resyncing to current position",
		"reason", reason, "marker", given, "cursor", cursor)
	return &pluginv1.PollChangesResponse{NextMarker: s.marker(cursor)}
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
		return &pluginv1.PollChangesResponse{NextMarker: s.marker(cursor)}, nil
	}

	epochPart, seqPart, ok := strings.Cut(raw, ":")
	if !ok {
		// A bare number is a marker from before markers carried an epoch. It
		// cannot be validated against this incarnation, so treat it as foreign.
		return s.resync("marker predates epoch tagging", raw), nil
	}

	epoch, epochErr := strconv.ParseUint(epochPart, 10, 64)
	marker, seqErr := strconv.ParseUint(seqPart, 10, 64)
	if epochErr != nil || seqErr != nil {
		// The host stores our marker verbatim and cannot interpret it, so a
		// corrupt one is ours to recover from.
		return s.resync("unparseable marker", raw), nil
	}
	if epoch != s.library.Epoch() {
		// The index was rebuilt since this marker was issued, so its sequence
		// numbers mean nothing here. This is the restart case the epoch exists
		// to catch.
		return s.resync("index was rebuilt since this marker was issued", raw), nil
	}

	items, next := s.library.Since(marker)
	if len(items) == 0 {
		// Since clamps a marker that runs past the cursor; propagate the
		// clamped value rather than echoing a position we cannot honour.
		return &pluginv1.PollChangesResponse{NextMarker: s.marker(next)}, nil
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
		NextMarker:  s.marker(next),
	}, nil
}
