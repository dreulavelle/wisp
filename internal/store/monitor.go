package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/dreulavelle/wisp/internal/library"
	"go.etcd.io/bbolt"
)

var monitorsBucket = []byte("monitors")

// DueReason values record why a Monitored item's DueAt was set, so consumers
// (the schedule API) can tell a real content date from a plain retry ceiling.
const (
	// DueReasonRetry is the zero value: DueAt is a fallback re-check ceiling, not
	// a real date (no stream yet, a metadata error, or no known upcoming episode).
	DueReasonRetry = ""
	// DueReasonRelease means DueAt is a movie's home-media release date.
	DueReasonRelease = "release"
	// DueReasonAirstamp means DueAt is a series' next episode air time.
	DueReasonAirstamp = "airstamp"
	// DueReasonTierBackoff means DueAt is driven by a per-quality-tier backoff: the
	// only remaining unpinned targets are tiers that consistently return "results
	// exist but not at this resolution" (e.g. a 2160p request for a show with no 4K
	// rips), so the tight retry cadence is replaced by the tier's NextTry. Like the
	// other retry reasons it is not a real content date — the schedule API reports
	// it as "retrying", never "waiting".
	DueReasonTierBackoff = "tier_backoff"
)

// TierBackoffState is the per-quality-tier retry backoff for a monitored title. A
// tier is backed off when every aired unit of the title (each aired episode of a
// series, or the movie) reports NoQualityMatch for it in a pass — i.e. the
// resolution is genuinely absent across the whole title, not merely unreleased.
// Misses is the consecutive-miss streak (drives an exponential schedule); NextTry
// is the earliest time to re-attempt the tier. wisp never permanently gives up:
// NextTry is capped (WISP_TIER_BACKOFF_MAX), so a late release is still retried.
type TierBackoffState struct {
	Misses  int       `json:"misses"`
	NextTry time.Time `json:"next_try"`
}

// Monitored is a title wisp is tracking until it can be pinned: a movie awaiting
// its home-media release/availability, or an ongoing series whose new episodes
// should be pinned as they air. It persists in the same bbolt DB as pins so the
// watchlist survives restarts.
type Monitored struct {
	Key       string    // stable id, e.g. "movie:tt123" or "series:tt456"
	MediaType string    // "movie" | "series"
	IMDbID    string    // "tt…" (may be empty for a tmdb-only movie)
	TMDbID    string    // stremio/tmdb id used against AIOStreams and TMDB
	TVDbID    string    // for folder tagging
	Title     string    // for folder/file naming
	Year      int       // for folder/file naming
	Qualities []string  // requested tiers; empty = default (best stream)
	Seasons   []int     // series: requested seasons; empty = all
	DueAt     time.Time // earliest time worth re-checking (release or next air)
	DueReason string    // why DueAt was set — one of the DueReason* constants

	// Category is the library root this title resolved to (library.Root*). It is
	// decided ONCE at first intake (explicit is_anime flag, else a metadata
	// heuristic), stored here, inherited by every pin, and NEVER re-derived — the
	// root is part of each pin's VirtualPath, so re-deriving would orphan files.
	Category string
	// RequestRef is an opaque caller key (e.g. a Silo request id) echoed back on
	// the status API; wisp never interprets it.
	RequestRef string
	// PendingAired is the scheduler's last count of aired-but-unpinned episodes
	// for a series (0 = caught up). It lets the status API report series
	// completion without a network call. Meaningless for movies.
	PendingAired int
	// PendingByTier breaks PendingAired down by the requested quality tier that is
	// still unpinned, keyed by canonical quality (library.NormalizeQuality). It
	// exists so the status API can discount work belonging to a tier the scheduler
	// has given up on — PendingAired alone is a scalar that mixes tiers, so a
	// nonexistent 2160p release would otherwise block completion forever.
	//
	// It is a subset, not a partition: the default ("best available") tier is never
	// tracked here, so sum(PendingByTier) <= PendingAired. Nil/absent for old
	// records and titles with no requested tiers — zero-value tolerant.
	PendingByTier map[string]int `json:"PendingByTier,omitempty"`
	// Failed marks a permanent give-up (unresolvable identity). wisp otherwise
	// retries indefinitely, so this is rare by design; it is never set for an
	// unreleased/unaired title.
	Failed bool

	// TierBackoff is the per-quality-tier backoff state, keyed by canonical quality
	// (library.NormalizeQuality, e.g. "2160p"). A tier appears here once it has been
	// detected absent across the whole title; a successful pin of that tier removes
	// it. Nil/absent for old records and titles with no requested tiers — zero-value
	// tolerant. Kept readable (never hidden) so the schedule API can surface it.
	TierBackoff map[string]TierBackoffState `json:"TierBackoff,omitempty"`

	// Observability / control (kept-and-marked so the monitor list doubles as a
	// request history — idea from drondeseries's PR #5).
	Enabled     bool      // false = paused; kept but not refreshed
	Completed   bool      // movie: every requested quality is pinned
	LastChecked time.Time // when the scheduler last processed it
	LastError   string    // last non-fatal error, for surfacing in the CRUD API

	AddedAt   time.Time
	UpdatedAt time.Time
}

// monitoredSearchID is the id an item's pins are stored under — imdb if known,
// else "tmdb:<id>" — so category backfill and dedupe lookups match how pins are
// keyed. It mirrors the same helper in the monitor/main packages.
func monitoredSearchID(m Monitored) string {
	if m.IMDbID != "" {
		return m.IMDbID
	}
	if m.TMDbID != "" {
		return "tmdb:" + m.TMDbID
	}
	return ""
}

// ApplyQualityPolicy is an idempotent startup migration that rewrites every
// monitor's requested tiers through the configured policy, so a tier the operator
// disallowed stops being scraped on the very next scheduler pass. Filtering at
// intake alone is not enough: Monitored.Qualities is what targetsForQualities
// reads on every pass, and Intake unions a new request onto the stored list, so a
// 2160p left in the store would be re-requested for ever.
//
// A monitor whose ONLY tier is disallowed is marked Failed with an explanatory
// LastError rather than silently downgraded to "best available" — the request was
// for a resolution wisp will not fetch, so the status API should report it failed
// and let the caller close it, exactly as a fresh intake would be rejected. An
// already-Completed or already-Failed monitor is left untouched.
//
// Removing a tier also drops its backoff and pending-work bookkeeping, so a
// series is not left reporting pending episodes for a tier nobody is chasing.
// Monitors that request nothing ("best available") are unconstrained and skipped.
//
// It is one-way: re-enabling a tier later does not restore it to existing
// monitors, which must be re-requested. Returns the number of monitors rewritten
// and the keys of those marked failed.
func (s *Store) ApplyQualityPolicy(_ context.Context, p library.QualityPolicy) (int, []string, error) {
	type kv struct{ k, v []byte }
	var writes []kv
	var failedKeys []string
	err := s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(monitorsBucket)
		// Collect writes during iteration and apply them after — mutating a bbolt
		// bucket inside its own ForEach can make the cursor skip or repeat keys.
		if err := b.ForEach(func(k, v []byte) error {
			var m Monitored
			if err := json.Unmarshal(v, &m); err != nil {
				return err
			}
			if len(m.Qualities) == 0 || m.Completed || m.Failed {
				return nil
			}
			next, applyErr := p.Apply(m.Qualities)
			switch {
			case errors.Is(applyErr, library.ErrNoAllowedQuality):
				m.Failed = true
				m.LastError = "requested quality " + strings.Join(m.Qualities, ",") +
					" is disabled by the configured quality policy; re-request at an allowed tier"
				failedKeys = append(failedKeys, m.Key)
			case applyErr != nil:
				return applyErr
			case sameStrings(m.Qualities, next):
				return nil // already compliant — leave the record byte-identical
			default:
				for _, q := range m.Qualities {
					if containsString(next, q) {
						continue
					}
					m.PendingAired -= m.PendingByTier[q]
					delete(m.PendingByTier, q)
					delete(m.TierBackoff, q)
				}
				if m.PendingAired < 0 {
					m.PendingAired = 0
				}
				m.Qualities = next
			}
			m.UpdatedAt = time.Now()
			val, err := json.Marshal(m)
			if err != nil {
				return err
			}
			writes = append(writes, kv{append([]byte(nil), k...), val})
			return nil
		}); err != nil {
			return err
		}
		for _, w := range writes {
			if err := b.Put(w.k, w.v); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return len(writes), failedKeys, nil
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// PutMonitored inserts or replaces a monitored item by its key.
func (s *Store) PutMonitored(_ context.Context, m Monitored) error {
	if m.AddedAt.IsZero() {
		m.AddedAt = time.Now()
	}
	m.UpdatedAt = time.Now()
	return s.db.Update(func(tx *bbolt.Tx) error {
		val, err := json.Marshal(m)
		if err != nil {
			return err
		}
		return tx.Bucket(monitorsBucket).Put([]byte(m.Key), val)
	})
}

// GetMonitored returns the monitored item for a key, or (nil, nil) if absent.
func (s *Store) GetMonitored(_ context.Context, key string) (*Monitored, error) {
	var item *Monitored
	err := s.db.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(monitorsBucket).Get([]byte(key))
		if v == nil {
			return nil
		}
		var m Monitored
		if err := json.Unmarshal(v, &m); err != nil {
			return err
		}
		item = &m
		return nil
	})
	return item, err
}

// ListMonitored returns every monitored item.
func (s *Store) ListMonitored(_ context.Context) ([]Monitored, error) {
	var items []Monitored
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(monitorsBucket).ForEach(func(_, v []byte) error {
			var m Monitored
			if err := json.Unmarshal(v, &m); err != nil {
				return err
			}
			items = append(items, m)
			return nil
		})
	})
	return items, err
}

// DeleteMonitored removes a monitored item (e.g. a movie that finished pinning).
func (s *Store) DeleteMonitored(_ context.Context, key string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(monitorsBucket).Delete([]byte(key))
	})
}

// CountMonitored returns the number of monitored items (for observability).
func (s *Store) CountMonitored(_ context.Context) (int, error) {
	n := 0
	err := s.db.View(func(tx *bbolt.Tx) error {
		n = tx.Bucket(monitorsBucket).Stats().KeyN
		return nil
	})
	return n, err
}
