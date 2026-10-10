package history

import (
	"fmt"
	"strings"

	"novastream/internal/mediaidentity"
	"novastream/models"
)

// SeriesProgressIndex resolves per-episode watch/resume state for one series
// from that series' (already title-filtered) playback progress and the user's
// watch history. It lets the details bundle hand clients a fully annotated
// episode list so they never need to pull every progress or history row.
type SeriesProgressIndex struct {
	seriesID    string
	externalIDs map[string]string
	progress    map[string]models.PlaybackProgress // keyed by sNNeNN
	watched     map[string]bool                    // keyed by sNNeNN
}

// NewSeriesProgressIndex builds an index for the series identified by
// seriesID/externalIDs (series-level tmdb/tvdb/imdb). progress should already
// be scoped to the series; watchHistory may be the user's full history and is
// filtered here by series identity.
func NewSeriesProgressIndex(seriesID string, externalIDs map[string]string, progress []models.PlaybackProgress, watchHistory []models.WatchHistoryItem) *SeriesProgressIndex {
	idx := &SeriesProgressIndex{
		seriesID:    strings.TrimSpace(seriesID),
		externalIDs: seriesOnlyExternalIDs(externalIDs),
		progress:    make(map[string]models.PlaybackProgress),
		watched:     make(map[string]bool),
	}
	for _, p := range progress {
		if p.MediaType != "episode" {
			continue
		}
		season, episode, ok := progressEpisodeNumbers(p)
		if !ok {
			continue
		}
		key := episodeKey(season, episode)
		if existing, found := idx.progress[key]; found && !p.UpdatedAt.After(existing.UpdatedAt) {
			continue
		}
		idx.progress[key] = p
	}

	target := mediaidentity.Resolve(mediaidentity.Input{MediaType: "series", ID: idx.seriesID, ExternalIDs: idx.externalIDs})
	for _, item := range watchHistory {
		if !item.Watched || mediaidentity.NormalizeMediaType(item.MediaType) != "episode" || item.EpisodeNumber <= 0 {
			continue
		}
		if !idx.watchHistoryMatchesSeries(item, target) {
			continue
		}
		idx.watched[episodeKey(item.SeasonNumber, item.EpisodeNumber)] = true
	}
	return idx
}

func progressEpisodeNumbers(p models.PlaybackProgress) (int, int, bool) {
	if p.EpisodeNumber > 0 {
		return p.SeasonNumber, p.EpisodeNumber, true
	}
	for _, id := range []string{p.ItemID, p.ID} {
		if _, season, episode, ok := mediaidentity.ParseEpisodeID(id); ok && episode > 0 {
			return season, episode, true
		}
	}
	return 0, 0, false
}

func (idx *SeriesProgressIndex) watchHistoryMatchesSeries(item models.WatchHistoryItem, target mediaidentity.Identity) bool {
	seriesID := strings.TrimSpace(item.SeriesID)
	if seriesID == "" {
		seriesID = mediaidentity.InferSeriesIDFromEpisodeItemID(item.ItemID)
	}
	if seriesID != "" && strings.EqualFold(seriesID, idx.seriesID) {
		return true
	}
	if seriesID != "" {
		current := mediaidentity.Resolve(mediaidentity.Input{MediaType: "series", ID: seriesID})
		if identitiesReferToSameTitle(current, target) {
			return true
		}
	}
	// Series-level external IDs bridge rows recorded under another provider's
	// series ID (split series IDs). Episode-scoped keys are ignored.
	itemIDs := mediaidentity.NormalizeExternalIDs(item.ExternalIDs)
	for _, key := range []string{"tmdb", "tvdb"} {
		if v := idx.externalIDs[key]; v != "" && strings.TrimSpace(itemIDs[key]) == v {
			return true
		}
	}
	if v := idx.externalIDs["imdb"]; v != "" && strings.EqualFold(strings.TrimSpace(itemIDs["imdb"]), v) {
		return true
	}
	return false
}

// ItemID returns the canonical progress item ID for an episode of the series.
func (idx *SeriesProgressIndex) ItemID(seasonNumber, episodeNumber int) string {
	if idx == nil {
		return ""
	}
	return EpisodeItemID(idx.seriesID, idx.externalIDs, seasonNumber, episodeNumber)
}

// Watched reports whether the episode is in the user's watch history.
func (idx *SeriesProgressIndex) Watched(seasonNumber, episodeNumber int) bool {
	if idx == nil {
		return false
	}
	return idx.watched[episodeKey(seasonNumber, episodeNumber)]
}

// Resume returns the episode's resume state, or nil when it has no progress.
func (idx *SeriesProgressIndex) Resume(seasonNumber, episodeNumber int) *models.ResumeState {
	if idx == nil {
		return nil
	}
	p, ok := idx.progress[episodeKey(seasonNumber, episodeNumber)]
	if !ok {
		return nil
	}
	return ProgressResume(p)
}

// AnnotateSeriesDetails returns a copy of details whose episodes carry
// itemId/watched/resume. The input (often a shared metadata-cache value) is
// never mutated.
func (idx *SeriesProgressIndex) AnnotateSeriesDetails(details *models.SeriesDetails) *models.SeriesDetails {
	if idx == nil || details == nil {
		return details
	}
	out := *details
	out.Seasons = make([]models.SeriesSeason, len(details.Seasons))
	for i, season := range details.Seasons {
		season.Episodes = make([]models.SeriesEpisode, len(details.Seasons[i].Episodes))
		for j, ep := range details.Seasons[i].Episodes {
			ep.ItemID = idx.ItemID(ep.SeasonNumber, ep.EpisodeNumber)
			ep.Watched = idx.Watched(ep.SeasonNumber, ep.EpisodeNumber)
			ep.Resume = idx.Resume(ep.SeasonNumber, ep.EpisodeNumber)
			season.Episodes[j] = ep
		}
		out.Seasons[i] = season
	}
	return &out
}

// UpNextEpisode picks the episode a series page should offer to play: the
// watch state's next episode when present, otherwise the first episode of the
// first regular season (falling back to any season with episodes), matching
// the client's previous fallback. The result carries itemId; nil when the
// series has no episodes.
func (idx *SeriesProgressIndex) UpNextEpisode(watchState *models.SeriesWatchState, details *models.SeriesDetails) *models.EpisodeReference {
	var ref *models.EpisodeReference
	if watchState != nil && watchState.NextEpisode != nil {
		next := *watchState.NextEpisode
		ref = &next
	} else if ep := firstPlayableEpisode(details); ep != nil {
		ref = &models.EpisodeReference{
			Numbering:             ep.Numbering,
			SeasonNumber:          ep.SeasonNumber,
			EpisodeNumber:         ep.EpisodeNumber,
			AbsoluteEpisodeNumber: ep.AbsoluteEpisodeNumber,
			Title:                 ep.Name,
			Overview:              ep.Overview,
			RuntimeMinutes:        ep.Runtime,
			AirDate:               ep.AiredDate,
			AirDateTimeUTC:        ep.AiredDateTimeUTC,
			Image:                 ep.Image,
		}
		if ep.TVDBID > 0 {
			ref.TvdbID = fmt.Sprintf("%d", ep.TVDBID)
		}
	}
	if ref == nil {
		return nil
	}
	ref.ItemID = idx.ItemID(ref.SeasonNumber, ref.EpisodeNumber)
	return ref
}

func firstPlayableEpisode(details *models.SeriesDetails) *models.SeriesEpisode {
	if details == nil {
		return nil
	}
	pick := func(regularOnly bool) *models.SeriesEpisode {
		var best *models.SeriesSeason
		for i := range details.Seasons {
			s := &details.Seasons[i]
			if len(s.Episodes) == 0 || (regularOnly && s.Number <= 0) {
				continue
			}
			if best == nil || s.Number < best.Number {
				best = s
			}
		}
		if best == nil {
			return nil
		}
		first := &best.Episodes[0]
		for i := range best.Episodes {
			if best.Episodes[i].EpisodeNumber < first.EpisodeNumber {
				first = &best.Episodes[i]
			}
		}
		return first
	}
	if ep := pick(true); ep != nil {
		return ep
	}
	return pick(false)
}
