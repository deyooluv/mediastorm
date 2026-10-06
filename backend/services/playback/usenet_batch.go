package playback

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"novastream/models"
)

var (
	// usenetBatchPollInterval is how often a queued (external engine) episode
	// resolve is polled until the release finishes importing.
	usenetBatchPollInterval = 2 * time.Second
	// usenetBatchQueueTimeout bounds the wait for one queued release.
	usenetBatchQueueTimeout = 15 * time.Minute
)

// resolveUsenetBatch resolves every requested episode from one usenet release.
// The release is fetched/imported once by the first resolve; later episodes
// reuse the resolved release and only re-run episode file selection.
func (s *Service) resolveUsenetBatch(ctx context.Context, candidate models.NZBResult, episodes []models.BatchEpisodeTarget) (*models.BatchResolveResponse, error) {
	start := time.Now()
	results := make([]models.BatchEpisodeResult, len(episodes))
	succeeded := 0
	var releaseErr error
	for i, ep := range episodes {
		res := models.BatchEpisodeResult{
			SeasonNumber:          ep.SeasonNumber,
			EpisodeNumber:         ep.EpisodeNumber,
			EpisodeCode:           ep.EpisodeCode,
			AbsoluteEpisodeNumber: ep.AbsoluteEpisodeNumber,
		}
		if releaseErr != nil {
			res.Error = releaseErr.Error()
			results[i] = res
			continue
		}
		resolution, err := s.resolveUsenetEpisode(ctx, withBatchEpisodeTarget(candidate, ep))
		if err != nil {
			res.Error = err.Error()
			// Until one episode succeeds, any failure other than a missing
			// episode (dead release, fetch error, timeout) applies to the whole
			// release, so don't refetch it for every remaining episode.
			if ctx.Err() != nil || (succeeded == 0 && !errors.Is(err, ErrEpisodeNotInRelease)) {
				releaseErr = err
			}
		} else {
			res.Resolution = resolution
			succeeded++
		}
		results[i] = res
	}
	log.Printf("[playback] usenet batch resolve title=%q episodes=%d succeeded=%d took=%v", strings.TrimSpace(candidate.Title), len(episodes), succeeded, time.Since(start))
	return &models.BatchResolveResponse{Results: results}, nil
}

// resolveUsenetEpisode resolves one episode, waiting for a queued external
// engine job to finish when necessary.
func (s *Service) resolveUsenetEpisode(ctx context.Context, candidate models.NZBResult) (*models.PlaybackResolution, error) {
	resolution, err := s.Resolve(ctx, candidate)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(resolution.WebDAVPath) != "" {
		return resolution, nil
	}
	if resolution.QueueID <= 0 {
		return nil, fmt.Errorf("usenet resolve returned no stream path")
	}
	if err := s.waitForQueueItem(ctx, resolution.QueueID); err != nil {
		return nil, err
	}
	// Queue completion selects a file for the request that submitted the job;
	// resolve again so this episode picks its own file from the release.
	resolution, err = s.Resolve(ctx, candidate)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(resolution.WebDAVPath) == "" {
		return nil, fmt.Errorf("usenet release still processing after queue item %d completed", resolution.QueueID)
	}
	return resolution, nil
}

func (s *Service) waitForQueueItem(ctx context.Context, queueID int64) error {
	waitCtx, cancel := context.WithTimeout(ctx, usenetBatchQueueTimeout)
	defer cancel()
	ticker := time.NewTicker(usenetBatchPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("timed out waiting for usenet queue item %d: %w", queueID, waitCtx.Err())
		case <-ticker.C:
		}
		status, err := s.QueueStatus(waitCtx, queueID)
		if err != nil {
			return err
		}
		if status != nil && strings.TrimSpace(status.WebDAVPath) != "" {
			return nil
		}
	}
}

// withBatchEpisodeTarget returns a copy of candidate targeting one episode,
// using the same attributes the per-episode playback resolve sends.
func withBatchEpisodeTarget(candidate models.NZBResult, ep models.BatchEpisodeTarget) models.NZBResult {
	attrs := make(map[string]string, len(candidate.Attributes)+6)
	for k, v := range candidate.Attributes {
		attrs[k] = v
	}
	attrs["targetSeason"] = strconv.Itoa(ep.SeasonNumber)
	attrs["targetEpisode"] = strconv.Itoa(ep.EpisodeNumber)
	if code := strings.TrimSpace(ep.EpisodeCode); code != "" {
		attrs["targetEpisodeCode"] = code
	} else if ep.SeasonNumber > 0 && ep.EpisodeNumber > 0 {
		attrs["targetEpisodeCode"] = fmt.Sprintf("S%02dE%02d", ep.SeasonNumber, ep.EpisodeNumber)
	}
	if ep.AbsoluteEpisodeNumber > 0 {
		attrs["absoluteEpisodeNumber"] = strconv.Itoa(ep.AbsoluteEpisodeNumber)
	}
	if ep.AirDate != "" {
		attrs["targetAirDate"] = ep.AirDate
	}
	if ep.IsDaily {
		attrs["isDaily"] = "true"
	}
	candidate.Attributes = attrs
	return candidate
}
