package plugin

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// EpisodeLister enumerates the episodes of a series that have already aired.
//
// Narrowed to what intake needs so tests can substitute a stub. Unaired
// episodes are deliberately excluded: writing placeholders for them would fill
// a library with items that cannot play and would fail noisily the first time
// anyone tried.
type EpisodeLister interface {
	ReleasedEpisodes(ctx context.Context, imdbID string) ([]EpisodeRef, error)
}

// EpisodeRef is one aired episode.
type EpisodeRef struct {
	Season  int
	Episode int
}

// Intake turns Silo requests into placeholders.
type Intake struct {
	writer   *Writer
	library  *Library
	episodes EpisodeLister
	identity IdentityResolver
	anime    AnimeClassifier
	pusher   *ScanPusher
	log      *slog.Logger
}

// NewIntake wires request handling over a placeholder writer.
func NewIntake(writer *Writer, library *Library, episodes EpisodeLister, log *slog.Logger) *Intake {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Intake{
		writer: writer, library: library, episodes: episodes, log: log,
		// Non-nil by default so every call site can push unconditionally; a
		// pusher with no publisher is a no-op.
		pusher: NewScanPusher(nil, log),
	}
}

// WithScanPusher makes written placeholders reported to the host immediately,
// rather than waiting to be polled.
func (i *Intake) WithScanPusher(p *ScanPusher) *Intake {
	if p != nil {
		i.pusher = p
	}
	return i
}

// WithAnimeClassifier enables routing anime into its own library roots. When
// unset, everything is written to the general roots.
func (i *Intake) WithAnimeClassifier(c AnimeClassifier) *Intake {
	i.anime = c
	return i
}

// WithIdentityResolver enables deriving a missing canonical id from the IMDb id
// the host did supply, so a request without a TVDB id is not simply refused.
func (i *Intake) WithIdentityResolver(r IdentityResolver) *Intake {
	i.identity = r
	return i
}

// identityFrom extracts the canonical identity and lookup key from the ids Silo
// supplies.
//
// Silo populates "tmdb", "tvdb" and "imdb" keys, but not always all three: TVDB
// in particular is optional on its side. Requiring the right one up front makes
// a missing id a clear request failure rather than a placeholder that scans in
// and then cannot play.
func identityFrom(mediaType string, ids map[string]string) (id MediaID, imdb string, err error) {
	want, err := ExpectedSource(mediaType)
	if err != nil {
		return MediaID{}, "", err
	}

	raw := strings.TrimSpace(ids[string(want)])
	if raw == "" {
		return MediaID{}, "", fmt.Errorf(
			"intake: %s request has no %s id; Wisp identifies %ss by %s",
			mediaType, want, mediaType, want)
	}
	id, err = ParseMediaID(string(want) + ":" + raw)
	if err != nil {
		return MediaID{}, "", err
	}

	imdb = strings.TrimSpace(ids["imdb"])
	if imdb == "" {
		return MediaID{}, "", fmt.Errorf(
			"intake: %s request has no imdb id; it is the only key the stream provider accepts", mediaType)
	}
	return id, imdb, nil
}

// Fulfill writes placeholders for a request, one per requested quality.
//
// Placeholders are reported as completed rather than queued. For on-demand
// playback that is the honest signal: once the placeholder exists the title is
// playable, and there is no download to wait on. Reporting queued would leave
// requests pending forever, since nothing downstream will ever move them along.
func (i *Intake) Fulfill(ctx context.Context, req *pluginv1.FulfillRequest) (*pluginv1.FulfillResponse, error) {
	desc := req.GetRequest()
	mediaType := strings.ToLower(strings.TrimSpace(desc.GetMediaType()))

	id, imdb, err := resolveIdentity(ctx, mediaType, desc.GetExternalIds(), i.identity)
	if err != nil {
		i.log.Warn("intake: rejecting request", "title", desc.GetTitle(), "error", err)
		return &pluginv1.FulfillResponse{Message: err.Error()}, nil
	}

	qualities := req.GetQualities()
	if len(qualities) == 0 {
		// The host governs quality. No tier means "whatever is available",
		// expressed as a single unconstrained placeholder.
		qualities = []*pluginv1.RequestedQuality{{}}
	}

	connectionID := ""
	if conns := req.GetConnections(); len(conns) > 0 {
		connectionID = conns[0].GetId()
	}

	var targets []*pluginv1.FulfillmentTarget
	for _, q := range qualities {
		written, err := i.writeFor(ctx, mediaType, desc, id, imdb, q.GetId())

		target := &pluginv1.FulfillmentTarget{
			Quality:      q.GetId(),
			ConnectionId: connectionID,
			ExternalId:   id.String(),
		}
		if err != nil {
			i.log.Error("intake: placeholder write failed",
				"title", desc.GetTitle(), "id", id.String(), "quality", q.GetId(), "error", err)
			target.Status = "failed"
			target.Message = err.Error()
		} else {
			target.Status = "completed"
			target.Message = fmt.Sprintf("%d placeholder(s) written; playable on demand", written)
		}
		targets = append(targets, target)
	}

	return &pluginv1.FulfillResponse{Targets: targets}, nil
}

// writeFor writes every placeholder a request implies and returns the count.
func (i *Intake) writeFor(ctx context.Context, mediaType string, desc *pluginv1.RequestDescriptor, id MediaID, imdb, quality string) (int, error) {
	base := Item{
		MediaType: mediaType,
		Title:     desc.GetTitle(),
		Year:      int(desc.GetYear()),
		ID:        id,
		IMDbID:    imdb,
		Quality:   quality,
	}

	// Classified once here, then carried onto every placeholder this request
	// produces — including each episode of a series, which must not end up
	// split across two roots because a lookup flapped mid-fan-out.
	if i.anime != nil {
		base.Anime = i.anime.IsAnime(ctx, mediaType, imdb)
	}

	if mediaType == "movie" {
		path, err := i.writeOne(base)
		if err != nil {
			return 0, err
		}
		i.log.Info("intake: placeholder written", "path", path)
		i.pusher.Push(ctx, []string{path})
		return 1, nil
	}

	// A series request covers a show, not an episode, so it fans out to every
	// aired episode. Unaired ones are skipped: a placeholder that cannot play
	// is worse than an absent one, because it looks available.
	if i.episodes == nil {
		return 0, fmt.Errorf("intake: no episode source configured; cannot expand a series request")
	}
	eps, err := i.episodes.ReleasedEpisodes(ctx, imdb)
	if err != nil {
		return 0, fmt.Errorf("intake: enumerate episodes: %w", err)
	}
	if len(eps) == 0 {
		return 0, fmt.Errorf("intake: %s has no aired episodes yet", desc.GetTitle())
	}

	var (
		paths    []string
		firstErr error
	)
	for _, ep := range eps {
		item := base
		item.Season, item.Episode = ep.Season, ep.Episode

		path, err := i.writeOne(item)
		if err != nil {
			// One bad episode should not sink a whole season. Record the first
			// failure and keep going, so a request for a 60-episode show still
			// delivers 59 playable items.
			if firstErr == nil {
				firstErr = err
			}
			i.log.Warn("intake: episode placeholder failed",
				"title", desc.GetTitle(), "season", ep.Season, "episode", ep.Episode, "error", err)
			continue
		}
		paths = append(paths, path)
	}

	if len(paths) == 0 && firstErr != nil {
		return 0, firstErr
	}
	i.log.Info("intake: series placeholders written",
		"title", desc.GetTitle(), "id", id.String(), "written", len(paths), "aired", len(eps))
	// One push for the whole request rather than one per episode: a season
	// arriving as 24 separate notifications would have the host resolve and
	// enqueue 24 times over for work it can do in a single pass.
	i.pusher.Push(ctx, paths)
	return len(paths), nil
}

// writeOne writes a placeholder and records it for the dashboard and autoscan.
func (i *Intake) writeOne(item Item) (string, error) {
	path, err := i.writer.Write(item)
	if err != nil {
		return "", err
	}
	// Registering after the write, not before, so autoscan never reports a path
	// that does not yet exist on disk.
	i.library.Add(Placeholder{
		Path:      path,
		MediaType: item.MediaType,
		ID:        item.ID,
		IMDbID:    item.IMDbID,
		Season:    item.Season,
		Episode:   item.Episode,
		Quality:   item.Quality,
	})
	return path, nil
}

// CheckStatus reports on previously fulfilled targets.
//
// A placeholder has no download to track: it is either present and playable or
// it is gone. Reporting completed keeps requests from sitting pending forever
// waiting for progress that will never arrive.
func (i *Intake) CheckStatus(_ context.Context, req *pluginv1.CheckStatusRequest) (*pluginv1.CheckStatusResponse, error) {
	statuses := make([]*pluginv1.TargetStatus, 0, len(req.GetTargets()))
	for _, t := range req.GetTargets() {
		statuses = append(statuses, &pluginv1.TargetStatus{
			Quality:      t.GetQuality(),
			ConnectionId: t.GetConnectionId(),
			Status:       "completed",
			Message:      "playable on demand",
		})
	}
	return &pluginv1.CheckStatusResponse{Statuses: statuses}, nil
}
