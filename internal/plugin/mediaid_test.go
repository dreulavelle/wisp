package plugin

import "testing"

func TestParseMediaID(t *testing.T) {
	cases := map[string]MediaID{
		"tmdb:603":    {SourceTMDB, "603"},
		"TMDB:603":    {SourceTMDB, "603"},
		"tvdb:121361": {SourceTVDB, "121361"},
		"tt0133093":   {SourceIMDb, "tt0133093"},
		" tmdb:603 ":  {SourceTMDB, "603"},
	}
	for raw, want := range cases {
		got, err := ParseMediaID(raw)
		if err != nil {
			t.Errorf("ParseMediaID(%q) error = %v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("ParseMediaID(%q) = %+v, want %+v", raw, got, want)
		}
	}
}

func TestParseMediaIDRejects(t *testing.T) {
	bad := []string{
		"", "   ", "603", "tmdb:", "tvdb:",
		// TMDB and TVDB ids are integers; anything else would become a
		// directory name.
		"tmdb:abc", "tvdb:12x", "tmdb:-1", "tmdb:1.5",
		"imdb:tt0133093", // IMDb is never prefixed
		"anilist:123",    // unsupported authority
		"tt",             // too short to be an IMDb id
	}
	for _, raw := range bad {
		if got, err := ParseMediaID(raw); err == nil {
			t.Errorf("ParseMediaID(%q) = %+v, want error", raw, got)
		}
	}
}

func TestMediaIDString(t *testing.T) {
	cases := map[MediaID]string{
		{SourceTMDB, "603"}:       "tmdb:603",
		{SourceTVDB, "121361"}:    "tvdb:121361",
		{SourceIMDb, "tt0133093"}: "tt0133093",
	}
	for id, want := range cases {
		if got := id.String(); got != want {
			t.Errorf("%+v.String() = %q, want %q", id, got, want)
		}
	}
}

// The bracketed tag is how a media server matches the exact title from a
// directory name. Getting the format wrong means every item is matched by
// fuzzy title search instead, which is where wrong-season episodes come from.
func TestMediaIDFolderTag(t *testing.T) {
	cases := map[MediaID]string{
		{SourceTMDB, "603"}:       "[tmdb-603]",
		{SourceTVDB, "121361"}:    "[tvdb-121361]",
		{SourceIMDb, "tt0133093"}: "[imdb-tt0133093]",
	}
	for id, want := range cases {
		if got := id.FolderTag(); got != want {
			t.Errorf("%+v.FolderTag() = %q, want %q", id, got, want)
		}
	}
}

func TestParseMediaIDRoundTripsThroughString(t *testing.T) {
	for _, id := range []MediaID{
		{SourceTMDB, "603"}, {SourceTVDB, "121361"}, {SourceIMDb, "tt0133093"},
	} {
		got, err := ParseMediaID(id.String())
		if err != nil {
			t.Errorf("round trip of %+v failed: %v", id, err)
			continue
		}
		if got != id {
			t.Errorf("round trip of %+v produced %+v", id, got)
		}
	}
}

// Movies are TMDB and series are TVDB. TVDB's season and episode numbering is
// what media servers agree on; IMDb's episode ordering diverges from it often
// enough that using IMDb as the series identity files episodes under the wrong
// season.
func TestExpectedSource(t *testing.T) {
	if got, err := ExpectedSource("movie"); err != nil || got != SourceTMDB {
		t.Errorf("ExpectedSource(movie) = %q, %v; want tmdb", got, err)
	}
	if got, err := ExpectedSource("series"); err != nil || got != SourceTVDB {
		t.Errorf("ExpectedSource(series) = %q, %v; want tvdb", got, err)
	}
	if _, err := ExpectedSource("audiobook"); err == nil {
		t.Error("ExpectedSource accepted an unknown media type")
	}
}

func TestValidateIdentity(t *testing.T) {
	ok := []struct {
		mediaType string
		id        MediaID
	}{
		{"movie", MediaID{SourceTMDB, "603"}},
		{"series", MediaID{SourceTVDB, "121361"}},
	}
	for _, c := range ok {
		if err := ValidateIdentity(c.mediaType, c.id); err != nil {
			t.Errorf("ValidateIdentity(%s, %+v) = %v, want nil", c.mediaType, c.id, err)
		}
	}

	// A swapped authority is the exact failure mode this type exists to
	// prevent, so it must be rejected rather than quietly accepted.
	bad := []struct {
		mediaType string
		id        MediaID
	}{
		{"movie", MediaID{SourceTVDB, "121361"}},
		{"series", MediaID{SourceTMDB, "603"}},
		{"movie", MediaID{SourceIMDb, "tt0133093"}},
		{"series", MediaID{SourceIMDb, "tt0944947"}},
	}
	for _, c := range bad {
		if err := ValidateIdentity(c.mediaType, c.id); err == nil {
			t.Errorf("ValidateIdentity(%s, %+v) accepted the wrong authority", c.mediaType, c.id)
		}
	}
}
