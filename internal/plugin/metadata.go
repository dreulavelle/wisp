package plugin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dreulavelle/wisp/internal/metadata"
)

// IdentityResolver fills in a canonical id when the host does not supply one.
//
// Silo populates tmdb/tvdb/imdb on a request, but not always all three — TVDB
// in particular is optional on its side. Rather than failing those requests,
// Wisp derives the missing authority from the IMDb id it does have.
type IdentityResolver interface {
	ProviderIDs(ctx context.Context, mediaType, imdbID string) (tvdb, tmdb string, err error)
}

// AnimeClassifier decides which library root a title belongs in.
//
// Narrowed to one call so intake can be tested without a metadata service, and
// so the decision has exactly one place it can be made.
type AnimeClassifier interface {
	IsAnime(ctx context.Context, mediaType, imdbID string) bool
}

// MetadataAdapter bridges Wisp's metadata service to the interfaces intake
// needs.
//
// Episode numbering comes from Cinemeta, whose series data is TVDB-derived:
// the show carries a tvdb_id and every episode carries its own. That matters
// because TVDB numbering is what media servers agree on, and IMDb's episode
// ordering diverges from it often enough to file episodes under the wrong
// season. Using Cinemeta gets TVDB-aligned numbering without a TVDB key.
type MetadataAdapter struct {
	svc *metadata.Service
	now func() time.Time
}

// NewMetadataAdapter wraps a metadata service.
func NewMetadataAdapter(svc *metadata.Service) *MetadataAdapter {
	return &MetadataAdapter{svc: svc, now: time.Now}
}

// ReleasedEpisodes returns the episodes of a series that have already aired.
//
// Unaired episodes are excluded deliberately: a placeholder for one would scan
// into the library looking available and then fail the moment anyone pressed
// play. An absent item is a better lie than a broken one.
func (m *MetadataAdapter) ReleasedEpisodes(ctx context.Context, imdbID string) ([]EpisodeRef, error) {
	if m.svc == nil {
		return nil, fmt.Errorf("metadata: no service configured")
	}

	eps, err := m.svc.ReleasedEpisodes(ctx, imdbID, m.now())
	if err != nil {
		return nil, err
	}

	out := make([]EpisodeRef, 0, len(eps))
	for _, e := range eps {
		// Episode 0 is not a real episode in any numbering scheme; season 0 is
		// specials and is kept.
		if e.Number < 1 {
			continue
		}
		out = append(out, EpisodeRef{Season: e.Season, Episode: e.Number})
	}
	return out, nil
}

// IsAnime reports whether a title belongs in the anime roots.
//
// Deliberately conservative: it requires the Animation genre AND a positive
// Japanese signal, so Western animation stays out. Misfiling Pixar as anime is
// the more visible error of the two, and an unclassified anime still plays
// perfectly — it just sits in the general library.
func (m *MetadataAdapter) IsAnime(ctx context.Context, mediaType, imdbID string) bool {
	return m.svc.AnimeHeuristic(ctx, mediaType, imdbID)
}

// ProviderIDs returns the TVDB and TMDB ids for an IMDb id.
func (m *MetadataAdapter) ProviderIDs(ctx context.Context, mediaType, imdbID string) (tvdb, tmdb string, err error) {
	return metadata.ProviderIDs(ctx, mediaType, imdbID)
}

// resolveIdentity returns the canonical identity for a request, deriving it
// when the host did not supply one.
//
// Derivation is a network call, so it happens here at request time rather than
// at playback: a request can afford a second, a user staring at a spinner
// cannot.
func resolveIdentity(ctx context.Context, mediaType string, ids map[string]string, resolver IdentityResolver) (MediaID, string, error) {
	id, imdb, err := identityFrom(mediaType, ids)
	if err == nil {
		return id, imdb, nil
	}

	// Only a missing canonical id is recoverable. Without an IMDb key there is
	// nothing to derive from.
	imdb = strings.TrimSpace(ids["imdb"])
	if imdb == "" || resolver == nil {
		return MediaID{}, "", err
	}

	want, wantErr := ExpectedSource(mediaType)
	if wantErr != nil {
		return MediaID{}, "", wantErr
	}

	tvdb, tmdb, lookupErr := resolver.ProviderIDs(ctx, mediaType, imdb)
	if lookupErr != nil {
		// Say the lookup failed rather than reporting "no id exists". They call
		// for different fixes, and conflating them points the operator at the
		// wrong one.
		return MediaID{}, "", fmt.Errorf("%w (lookup for %s failed: %v)", err, imdb, lookupErr)
	}
	derived := tmdb
	if want == SourceTVDB {
		derived = tvdb
	}
	if strings.TrimSpace(derived) == "" {
		return MediaID{}, "", fmt.Errorf(
			"%w (and none could be derived from %s)", err, imdb)
	}

	id, parseErr := ParseMediaID(string(want) + ":" + strings.TrimSpace(derived))
	if parseErr != nil {
		return MediaID{}, "", parseErr
	}
	return id, imdb, nil
}
