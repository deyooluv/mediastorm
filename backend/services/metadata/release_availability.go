package metadata

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"novastream/models"
)

const (
	// releaseWarmMaxPerCall caps how many uncached movies one response may
	// queue for background release-window fetches.
	releaseWarmMaxPerCall = 20
	// releaseWarmConcurrency caps concurrent background TMDB release fetches.
	releaseWarmConcurrency = 2
	releaseWarmTimeout     = 20 * time.Second
)

// releaseWarmer deduplicates and bounds background release-window fetches so
// search and shelf responses never wait on per-title upstream calls.
type releaseWarmer struct {
	enabled  bool // set by NewService; zero-value services (tests) never warm
	mu       sync.Mutex
	inflight map[int64]struct{}
	sem      chan struct{}
}

// finalizeSearchReleaseStatuses hydrates cached movie release windows and
// normalizes release statuses for search results.
func (s *Service) finalizeSearchReleaseStatuses(results []models.SearchResult) {
	titles := make([]*models.Title, 0, len(results))
	for i := range results {
		titles = append(titles, &results[i].Title)
	}
	s.hydrateCachedMovieReleaseWindows(titles)
	ensureSearchMovieReleaseStatuses(results)
}

// finalizeTrendingReleaseStatuses is finalizeSearchReleaseStatuses for shelves.
func (s *Service) finalizeTrendingReleaseStatuses(items []models.TrendingItem) {
	titles := make([]*models.Title, 0, len(items))
	for i := range items {
		titles = append(titles, &items[i].Title)
	}
	s.hydrateCachedMovieReleaseWindows(titles)
	ensureTrendingMovieReleaseStatuses(items)
}

// hydrateCachedMovieReleaseWindows fills theatrical/home release windows for
// movies from the release cache only (no upstream calls). Movies missing from
// the cache are queued for a bounded background fetch so later responses carry
// accurate windows; until then availability falls back to the release date.
func (s *Service) hydrateCachedMovieReleaseWindows(titles []*models.Title) {
	if s == nil || s.cache == nil {
		return
	}
	var misses []int64
	for _, title := range titles {
		if title == nil || !strings.EqualFold(strings.TrimSpace(title.MediaType), "movie") || title.TMDBID <= 0 {
			continue
		}
		if len(title.Releases) > 0 || title.Theatrical != nil || title.HomeRelease != nil {
			continue
		}
		cacheID := cacheKey("tmdb", "movie", "releases", "v2", strconv.FormatInt(title.TMDBID, 10))
		var cached cachedReleasesWithCert
		ok, _ := s.cache.get(cacheID, &cached)
		if !ok {
			misses = append(misses, title.TMDBID)
			continue
		}
		if len(cached.Releases) == 0 {
			continue // negative cache: TMDB has no release data
		}
		title.Releases = append([]models.Release(nil), cached.Releases...)
		if title.Certification == "" {
			title.Certification = cached.Certification
		}
		s.ensureMovieReleasePointers(title)
	}
	s.warmMovieReleasesAsync(misses)
}

// warmMovieReleasesAsync fetches release windows for up to
// releaseWarmMaxPerCall uncached movies in the background.
func (s *Service) warmMovieReleasesAsync(tmdbIDs []int64) {
	if len(tmdbIDs) == 0 || !s.releaseWarm.enabled || s.tmdb == nil || !s.tmdb.isConfigured() {
		return
	}
	w := &s.releaseWarm
	w.mu.Lock()
	if w.inflight == nil {
		w.inflight = make(map[int64]struct{})
		w.sem = make(chan struct{}, releaseWarmConcurrency)
	}
	queued := make([]int64, 0, releaseWarmMaxPerCall)
	for _, id := range tmdbIDs {
		if len(queued) >= releaseWarmMaxPerCall {
			break
		}
		if _, busy := w.inflight[id]; busy {
			continue
		}
		w.inflight[id] = struct{}{}
		queued = append(queued, id)
	}
	sem := w.sem
	w.mu.Unlock()

	for _, id := range queued {
		go func(tmdbID int64) {
			sem <- struct{}{}
			defer func() {
				<-sem
				w.mu.Lock()
				delete(w.inflight, tmdbID)
				w.mu.Unlock()
			}()
			ctx, cancel := context.WithTimeout(context.Background(), releaseWarmTimeout)
			defer cancel()
			s.enrichMovieReleases(ctx, &models.Title{MediaType: "movie", TMDBID: tmdbID}, tmdbID)
		}(id)
	}
}
