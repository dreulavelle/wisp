package plugin

import (
	"fmt"
	"strconv"
	"strings"
)

// IDSource names the authority an id belongs to.
type IDSource string

const (
	// SourceTMDB identifies movies. TMDB is the canonical movie authority.
	SourceTMDB IDSource = "tmdb"
	// SourceTVDB identifies series. TVDB's season and episode numbering is the
	// authority media servers agree on; IMDb's episode ordering diverges from
	// it often enough that using IMDb as the series identity produces episodes
	// filed under the wrong season.
	SourceTVDB IDSource = "tvdb"
	// SourceIMDb is the lookup key, not an identity. AIOStreams and the Stremio
	// addon ecosystem accept IMDb ids and nothing else — verified against a live
	// instance, where tmdb: and tvdb: ids return zero candidates.
	SourceIMDb IDSource = "imdb"
)

// MediaID is a canonical library identity.
//
// Wisp deliberately keeps identity and lookup separate. Identity is what the
// library is organized by and what a media server matches on: TMDB for movies,
// TVDB for series. Lookup is what the stream provider understands, which is
// only ever IMDb. Conflating the two is what produces libraries where the
// "correct" IMDb id maps to the wrong TVDB entry.
type MediaID struct {
	Source IDSource
	Value  string
}

// ParseMediaID reads "tmdb:603", "tvdb:121361", or a bare IMDb id.
func ParseMediaID(raw string) (MediaID, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return MediaID{}, fmt.Errorf("mediaid: empty id")
	}

	if prefix, value, ok := strings.Cut(s, ":"); ok {
		switch IDSource(strings.ToLower(prefix)) {
		case SourceTMDB:
			return newNumeric(SourceTMDB, value)
		case SourceTVDB:
			return newNumeric(SourceTVDB, value)
		default:
			return MediaID{}, fmt.Errorf("mediaid: unknown id source %q", prefix)
		}
	}

	if strings.HasPrefix(s, "tt") && len(s) > 2 {
		return MediaID{Source: SourceIMDb, Value: s}, nil
	}
	return MediaID{}, fmt.Errorf("mediaid: %q is not a recognized id", raw)
}

func newNumeric(src IDSource, value string) (MediaID, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return MediaID{}, fmt.Errorf("mediaid: %s id is empty", src)
	}
	// TMDB and TVDB ids are integers. Rejecting anything else keeps a malformed
	// id from becoming a directory name.
	if _, err := strconv.ParseUint(v, 10, 64); err != nil {
		return MediaID{}, fmt.Errorf("mediaid: %s id %q is not numeric", src, v)
	}
	return MediaID{Source: src, Value: v}, nil
}

// String renders the id in the form used in resolver paths.
func (m MediaID) String() string {
	if m.Source == SourceIMDb {
		return m.Value
	}
	return string(m.Source) + ":" + m.Value
}

// FolderTag renders the id in the bracketed form media servers parse from
// directory names, e.g. "[tmdb-603]" or "[tvdb-121361]".
//
// Silo, Plex, Jellyfin and Emby all read this convention, so encoding identity
// in the folder name means a scanner matches the right title without Wisp
// having to push metadata anywhere.
func (m MediaID) FolderTag() string {
	if m.Source == SourceIMDb {
		return "[imdb-" + m.Value + "]"
	}
	return "[" + string(m.Source) + "-" + m.Value + "]"
}

// Valid reports whether the id is usable.
func (m MediaID) Valid() bool {
	return m.Source != "" && m.Value != ""
}

// ExpectedSource returns the identity authority for a media type.
func ExpectedSource(mediaType string) (IDSource, error) {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "movie":
		return SourceTMDB, nil
	case "series":
		return SourceTVDB, nil
	default:
		return "", fmt.Errorf("mediaid: unknown media type %q", mediaType)
	}
}

// ValidateIdentity reports whether an id is the right authority for a media
// type. Movies must be TMDB and series must be TVDB; anything else is a
// mis-filed item waiting to happen.
func ValidateIdentity(mediaType string, id MediaID) error {
	want, err := ExpectedSource(mediaType)
	if err != nil {
		return err
	}
	if id.Source != want {
		return fmt.Errorf("mediaid: %s identity must be %s, got %s", mediaType, want, id.Source)
	}
	return nil
}
