package history

import (
	"fmt"
	"strings"

	"novastream/internal/mediaidentity"
	"novastream/models"
)

// Resume thresholds. An item is offered for resume only when its watched
// percentage is strictly between these bounds. They mirror the rule clients
// applied locally before resume became server-computed (percent > 5 and
// percent < 95), so moving the decision server-side does not change behaviour.
const (
	ResumeMinPercent = 5.0
	ResumeMaxPercent = 95.0
)

// ComputeResume builds the server resume decision for a playback position.
// Position and duration are seconds. When duration is unknown (<= 0) the
// provided percent (0–100) is used as-is, e.g. for Trakt-imported progress.
func ComputeResume(position, duration, percent float64) models.ResumeState {
	if position < 0 {
		position = 0
	}
	if duration > 0 {
		percent = (position / duration) * 100
	}
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	return models.ResumeState{
		Position: position,
		Duration: max(duration, 0),
		Percent:  percent,
		Eligible: percent > ResumeMinPercent && percent < ResumeMaxPercent,
	}
}

// ProgressResume returns the resume state for a stored progress entry. The
// stored PercentWatched is authoritative (it already accounts for a missing
// duration), so it is used directly rather than recomputed.
func ProgressResume(p models.PlaybackProgress) *models.ResumeState {
	state := ComputeResume(p.Position, 0, p.PercentWatched)
	state.Duration = max(p.Duration, 0)
	return &state
}

// WithProgressResume returns a copy of items with Resume populated on each.
func WithProgressResume(items []models.PlaybackProgress) []models.PlaybackProgress {
	if items == nil {
		return nil
	}
	out := make([]models.PlaybackProgress, len(items))
	for i, item := range items {
		item.Resume = ProgressResume(item)
		out[i] = item
	}
	return out
}

// EpisodeItemID returns the canonical progress/watch-history item ID for an
// episode of the given series (e.g. "tmdb:tv:95350:s01e01"). It runs the same
// identity resolution as progress writes, so the returned ID is the key the
// server stores progress under (TMDB-preferred when the series' external IDs
// are known). Returns "" when the episode cannot be addressed.
func EpisodeItemID(seriesID string, seriesExternalIDs map[string]string, seasonNumber, episodeNumber int) string {
	if strings.TrimSpace(seriesID) == "" && len(seriesExternalIDs) == 0 {
		return ""
	}
	if seasonNumber < 0 || episodeNumber <= 0 {
		return ""
	}
	identity := mediaidentity.Resolve(mediaidentity.Input{
		MediaType:     "episode",
		SeriesID:      seriesID,
		SeasonNumber:  seasonNumber,
		EpisodeNumber: episodeNumber,
		ExternalIDs:   seriesOnlyExternalIDs(seriesExternalIDs),
	})
	if _, _, _, ok := mediaidentity.ParseEpisodeID(identity.ID); !ok {
		return ""
	}
	return identity.ID
}

// seriesOnlyExternalIDs keeps the series-level provider IDs so episode-scoped
// keys never leak into series identity resolution.
func seriesOnlyExternalIDs(ids map[string]string) map[string]string {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string]string, 3)
	for _, key := range []string{"tmdb", "tvdb", "imdb"} {
		if v := strings.TrimSpace(ids[key]); v != "" {
			out[key] = v
		}
	}
	return out
}

// MergeProgressIntoContinueWatching computes PercentWatched, ResumePercent and
// the server Resume state for each continue-watching item using the user's
// playback progress, and stamps canonical ItemIDs on episode references. This
// keeps the map-building and threshold work off client JS threads.
//
// Semantics: for series, Resume describes NextEpisode (falling back to the
// last-watched episode's progress when they are the same episode); for movies
// it describes the movie. PercentWatched remains the max of resume and
// last-watched progress for display compatibility.
func MergeProgressIntoContinueWatching(items []models.SeriesWatchState, progress []models.PlaybackProgress) []models.SeriesWatchState {
	byItemID := make(map[string]*models.PlaybackProgress, len(progress)*2)
	byEpisode := make(map[string]*models.PlaybackProgress)
	// byExternalEpisode keys episode progress by each series-level external ID
	// (tvdb/tmdb/imdb) so progress recorded under one provider ID still matches a
	// continue-watching item that was canonicalised under a different one (e.g.
	// E02 under tmdb:tv:220102 while E01/E03 use tvdb:series:450033).
	byExternalEpisode := make(map[string]*models.PlaybackProgress)
	byEpisodeTvdb := make(map[string]*models.PlaybackProgress)

	for i := range progress {
		p := &progress[i]
		if p.ItemID != "" {
			byItemID[p.ItemID] = p
		}
		if p.ID != "" {
			byItemID[p.ID] = p
		}
		if p.MediaType == "episode" {
			if p.SeriesID != "" {
				byEpisode[fmt.Sprintf("%s:S%dE%d", p.SeriesID, p.SeasonNumber, p.EpisodeNumber)] = p
			}
			for _, key := range seriesExternalEpisodeKeys(p.ExternalIDs, p.SeasonNumber, p.EpisodeNumber) {
				byExternalEpisode[key] = p
			}
			if epTvdb := strings.TrimSpace(p.ExternalIDs["episodeTvdb"]); epTvdb != "" {
				byEpisodeTvdb[epTvdb] = p
			}
		}
	}

	episodeProgress := func(ep *models.EpisodeReference, item models.SeriesWatchState) *models.PlaybackProgress {
		if ep == nil {
			return nil
		}
		if ep.EpisodeID != "" {
			if p, ok := byItemID[ep.EpisodeID]; ok {
				return p
			}
		}
		if p, ok := byEpisode[fmt.Sprintf("%s:S%dE%d", item.SeriesID, ep.SeasonNumber, ep.EpisodeNumber)]; ok {
			return p
		}
		for _, k := range seriesExternalEpisodeKeys(item.ExternalIDs, ep.SeasonNumber, ep.EpisodeNumber) {
			if p, ok := byExternalEpisode[k]; ok {
				return p
			}
		}
		if ep.TvdbID != "" {
			if p, ok := byEpisodeTvdb[strings.TrimSpace(ep.TvdbID)]; ok {
				return p
			}
		}
		return nil
	}
	percentOf := func(p *models.PlaybackProgress) float64 {
		if p == nil {
			return 0
		}
		return p.PercentWatched
	}

	merged := make([]models.SeriesWatchState, len(items))
	for i, item := range items {
		merged[i] = item

		if item.NextEpisode == nil {
			// Movies may already carry active/enriched progress from the
			// continue endpoint. Do not let a stale raw zero progress row erase
			// that value and make the home shelf filter the card out.
			moviePct := max(item.ResumePercent, item.PercentWatched)
			raw := byItemID[item.SeriesID]
			if raw != nil && raw.PercentWatched > moviePct {
				moviePct = raw.PercentWatched
			}
			merged[i].PercentWatched = moviePct
			merged[i].ResumePercent = moviePct
			merged[i].Resume = resumeWithPercent(raw, moviePct)
			continue
		}

		nextProgress := episodeProgress(item.NextEpisode, item)
		lastProgress := episodeProgress(&item.LastWatched, item)
		isSame := item.LastWatched.SeasonNumber == item.NextEpisode.SeasonNumber &&
			item.LastWatched.EpisodeNumber == item.NextEpisode.EpisodeNumber

		resumeProgress := nextProgress
		if percentOf(resumeProgress) == 0 && isSame {
			resumeProgress = lastProgress
		}
		resumePct := percentOf(resumeProgress)
		merged[i].PercentWatched = max(resumePct, percentOf(lastProgress))
		merged[i].ResumePercent = resumePct
		merged[i].Resume = resumeWithPercent(resumeProgress, resumePct)

		next := *item.NextEpisode
		next.ItemID = EpisodeItemID(item.SeriesID, item.ExternalIDs, next.SeasonNumber, next.EpisodeNumber)
		merged[i].NextEpisode = &next
		if item.LastWatched.EpisodeNumber > 0 {
			merged[i].LastWatched.ItemID = EpisodeItemID(item.SeriesID, item.ExternalIDs, item.LastWatched.SeasonNumber, item.LastWatched.EpisodeNumber)
		}
	}

	return merged
}

// resumeWithPercent builds a resume state for percent, carrying the matched
// progress entry's position/duration when one exists.
func resumeWithPercent(p *models.PlaybackProgress, percent float64) *models.ResumeState {
	var position, duration float64
	if p != nil {
		position, duration = p.Position, p.Duration
	}
	state := ComputeResume(position, 0, percent)
	state.Duration = max(duration, 0)
	return &state
}

// seriesExternalEpisodeKeys builds provider-agnostic episode lookup keys from a
// series' external IDs (tvdb/tmdb/imdb). Keying episode progress by every known
// provider ID lets progress and continue-watching items that reference the same
// show under different series IDs still resolve to the same key.
func seriesExternalEpisodeKeys(externalIDs map[string]string, season, episode int) []string {
	if len(externalIDs) == 0 {
		return nil
	}
	var keys []string
	for _, idType := range []string{"tvdb", "tmdb", "imdb"} {
		val := strings.TrimSpace(externalIDs[idType])
		if val == "" {
			continue
		}
		if idType == "imdb" {
			val = strings.ToLower(val)
		}
		keys = append(keys, fmt.Sprintf("%s:%s:S%dE%d", idType, val, season, episode))
	}
	return keys
}
