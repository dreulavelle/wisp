package store

import (
	"context"
	"encoding/json"
	"time"

	"go.etcd.io/bbolt"
)

var monitorsBucket = []byte("monitors")

// Monitor is a persistent media request. Movie monitors complete after every
// requested quality is pinned; series monitors stay active for future episodes.
type Monitor struct {
	ID          string    `json:"id"`
	MediaType   string    `json:"media_type"`
	IMDbID      string    `json:"imdb_id"`
	TMDbID      string    `json:"tmdb_id,omitempty"`
	TVDbID      string    `json:"tvdb_id,omitempty"`
	Title       string    `json:"title"`
	Year        int       `json:"year"`
	Qualities   []string  `json:"qualities"`
	Enabled     bool      `json:"enabled"`
	Completed   bool      `json:"completed,omitempty"`
	LastChecked time.Time `json:"last_checked,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (s *Store) UpsertMonitor(_ context.Context, m Monitor) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(monitorsBucket)
		if data := b.Get([]byte(m.ID)); data != nil {
			var old Monitor
			if json.Unmarshal(data, &old) == nil && m.CreatedAt.IsZero() {
				m.CreatedAt = old.CreatedAt
			}
		}
		now := time.Now()
		if m.CreatedAt.IsZero() {
			m.CreatedAt = now
		}
		m.UpdatedAt = now
		data, err := json.Marshal(m)
		if err != nil {
			return err
		}
		return b.Put([]byte(m.ID), data)
	})
}

func (s *Store) ListMonitors(_ context.Context) ([]Monitor, error) {
	var monitors []Monitor
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(monitorsBucket).ForEach(func(_, value []byte) error {
			var m Monitor
			if err := json.Unmarshal(value, &m); err != nil {
				return err
			}
			monitors = append(monitors, m)
			return nil
		})
	})
	return monitors, err
}

func (s *Store) DeleteMonitor(_ context.Context, id string) (bool, error) {
	deleted := false
	err := s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(monitorsBucket)
		deleted = b.Get([]byte(id)) != nil
		return b.Delete([]byte(id))
	})
	return deleted, err
}
