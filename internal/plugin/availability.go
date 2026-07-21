package plugin

import (
	"context"
	"strings"

	"github.com/dreulavelle/wisp/internal/aiostreams"
)

// hasPlayableStream returns true if the stream source has at least one
// playable URL for the given media item. Used by the availability gate to
// avoid writing placeholders for content that has no streams yet.
func hasPlayableStream(ctx context.Context, client *aiostreams.Client, mediaType, imdbID string, season, episode int, quality string) bool {
	if client == nil {
		return false
	}
	streams, err := client.Search(ctx, mediaType, imdbID, season, episode)
	if err != nil || len(streams) == 0 {
		return false
	}
	if quality == "" {
		return true
	}
	for _, s := range streams {
		if strings.EqualFold(s.Resolution, quality) && s.URL != "" {
			return true
		}
	}
	// Fallback: any stream with a URL counts.
	for _, s := range streams {
		if s.URL != "" {
			return true
		}
	}
	return false
}
