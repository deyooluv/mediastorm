package history

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"novastream/models"
)

const cacheTestUser = "cache-user"
const cacheTestSeriesID = "tmdb:tv:5550"

// countingMetadataService serves fixed series metadata and counts upstream
// calls. Safe for concurrent use (series builds fan out goroutines).
type countingMetadataService struct {
	*mockMetadataService
	failSeries map[string]bool
	liteCalls  atomic.Int64
	fullCalls  atomic.Int64
}

func (m *countingMetadataService) fails(query models.SeriesDetailsQuery) bool {
	return m.failSeries[query.TitleID]
}

func (m *countingMetadataService) SeriesDetailsLite(ctx context.Context, query models.SeriesDetailsQuery) (*models.SeriesDetails, error) {
	m.liteCalls.Add(1)
	if m.fails(query) {
		return nil, fmt.Errorf("lite lookup failed for %s", query.TitleID)
	}
	return m.mockMetadataService.SeriesDetails(ctx, query)
}

func (m *countingMetadataService) SeriesDetails(ctx context.Context, query models.SeriesDetailsQuery) (*models.SeriesDetails, error) {
	m.fullCalls.Add(1)
	if m.fails(query) {
		return nil, fmt.Errorf("full lookup failed for %s", query.TitleID)
	}
	return m.mockMetadataService.SeriesDetails(ctx, query)
}

func cacheTestSeriesDetails() *models.SeriesDetails {
	episodes := make([]models.SeriesEpisode, 0, 4)
	for ep := 1; ep <= 4; ep++ {
		episodes = append(episodes, models.SeriesEpisode{
			ID:            fmt.Sprintf("tmdb:episode:%d", 9000+ep),
			Name:          fmt.Sprintf("Episode %d", ep),
			SeasonNumber:  1,
			EpisodeNumber: ep,
			AiredDate:     "2020-01-0" + fmt.Sprint(ep),
		})
	}
	return &models.SeriesDetails{
		Title:   models.Title{ID: cacheTestSeriesID, Name: "Cache Show", Year: 2020, TMDBID: 5550},
		Seasons: []models.SeriesSeason{{Number: 1, Episodes: episodes}},
	}
}

func newSeriesStatesCacheService(t *testing.T) (*Service, *countingMetadataService) {
	t.Helper()
	svc, err := NewService(t.TempDir())
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	meta := &countingMetadataService{mockMetadataService: &mockMetadataService{seriesDetails: cacheTestSeriesDetails()}}
	svc.SetMetadataService(meta)
	return svc, meta
}

func cacheTestEpisodeUpdate(episode int, watched bool) models.WatchHistoryUpdate {
	return models.WatchHistoryUpdate{
		MediaType:     "episode",
		ItemID:        fmt.Sprintf("%s:s01e%02d", cacheTestSeriesID, episode),
		Name:          fmt.Sprintf("Episode %d", episode),
		Watched:       &watched,
		WatchedAt:     time.Now().UTC().Add(-time.Duration(10-episode) * time.Hour),
		SeasonNumber:  1,
		EpisodeNumber: episode,
		SeriesID:      cacheTestSeriesID,
		SeriesName:    "Cache Show",
		ExternalIDs:   map[string]string{"tmdb": "5550"},
	}
}

func cacheTestProgressUpdate(episode int, position float64) models.PlaybackProgressUpdate {
	return models.PlaybackProgressUpdate{
		MediaType:     "episode",
		ItemID:        fmt.Sprintf("%s:s01e%02d", cacheTestSeriesID, episode),
		Position:      position,
		Duration:      1000,
		SeriesID:      cacheTestSeriesID,
		SeriesName:    "Cache Show",
		SeasonNumber:  1,
		EpisodeNumber: episode,
		ExternalIDs:   map[string]string{"tmdb": "5550"},
	}
}

func mustWatch(t *testing.T, svc *Service, episode int, watched bool) {
	t.Helper()
	if _, err := svc.UpdateWatchHistory(cacheTestUser, cacheTestEpisodeUpdate(episode, watched)); err != nil {
		t.Fatalf("UpdateWatchHistory(e%d) error = %v", episode, err)
	}
}

func mustSeriesState(t *testing.T, svc *Service) models.SeriesWatchState {
	t.Helper()
	states, err := svc.ListSeriesStates(cacheTestUser)
	if err != nil {
		t.Fatalf("ListSeriesStates() error = %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("expected 1 series state, got %d: %+v", len(states), states)
	}
	return states[0]
}

func cachedSeriesStatesEntry(svc *Service, userID string) *cachedSeriesStates {
	svc.mu.RLock()
	defer svc.mu.RUnlock()
	return svc.seriesStatesCache[userID]
}

func currentRevision(svc *Service, userID string) watchStateRevision {
	svc.mu.RLock()
	defer svc.mu.RUnlock()
	return svc.watchStateRevisionLocked(userID)
}

func nextEpisodeNumber(state models.SeriesWatchState) int {
	if state.NextEpisode == nil {
		return 0
	}
	return state.NextEpisode.EpisodeNumber
}

func TestListSeriesStatesCacheHitReturnsIndependentCopies(t *testing.T) {
	svc, _ := newSeriesStatesCacheService(t)
	mustWatch(t, svc, 1, true)

	first := mustSeriesState(t, svc)
	entry := cachedSeriesStatesEntry(svc, cacheTestUser)
	if entry == nil {
		t.Fatal("expected series states to be cached after first build")
	}
	if nextEpisodeNumber(first) != 2 {
		t.Fatalf("expected next episode 2, got %+v", first.NextEpisode)
	}

	// Mutate the returned value; the cache must not observe it.
	first.SeriesTitle = "mutated"
	if first.ExternalIDs == nil {
		first.ExternalIDs = map[string]string{}
	}
	first.ExternalIDs["tmdb"] = "mutated"
	if first.NextEpisode != nil {
		first.NextEpisode.EpisodeNumber = 99
	}
	for k := range first.WatchedEpisodes {
		delete(first.WatchedEpisodes, k)
	}

	second := mustSeriesState(t, svc)
	if got := cachedSeriesStatesEntry(svc, cacheTestUser); got != entry {
		t.Fatal("expected second call to be served from the same cache entry")
	}
	if second.SeriesTitle == "mutated" || second.ExternalIDs["tmdb"] == "mutated" || nextEpisodeNumber(second) != 2 || len(second.WatchedEpisodes) == 0 {
		t.Fatalf("cached result was mutated through a returned copy: %+v", second)
	}
}

func TestListSeriesStatesCacheInvalidatedByEveryMutation(t *testing.T) {
	type mutationCase struct {
		name   string
		setup  func(t *testing.T, svc *Service)
		mutate func(t *testing.T, svc *Service)
		// verify checks the rebuilt state reflects the mutation (nil = only the
		// invalidation itself is asserted).
		verify func(t *testing.T, svc *Service)
	}
	cases := []mutationCase{
		{
			name:   "UpdateWatchHistory",
			mutate: func(t *testing.T, svc *Service) { mustWatch(t, svc, 2, true) },
			verify: func(t *testing.T, svc *Service) {
				if got := nextEpisodeNumber(mustSeriesState(t, svc)); got != 3 {
					t.Fatalf("expected next episode 3 after watching e2, got %d", got)
				}
			},
		},
		{
			name:  "ToggleWatched",
			setup: func(t *testing.T, svc *Service) { mustWatch(t, svc, 2, true) },
			mutate: func(t *testing.T, svc *Service) {
				if _, err := svc.ToggleWatched(cacheTestUser, cacheTestEpisodeUpdate(2, true)); err != nil {
					t.Fatalf("ToggleWatched() error = %v", err)
				}
			},
			verify: func(t *testing.T, svc *Service) {
				if got := nextEpisodeNumber(mustSeriesState(t, svc)); got != 2 {
					t.Fatalf("expected next episode 2 after unwatching e2, got %d", got)
				}
			},
		},
		{
			name:  "DeleteWatchHistoryItem",
			setup: func(t *testing.T, svc *Service) { mustWatch(t, svc, 2, true) },
			mutate: func(t *testing.T, svc *Service) {
				if err := svc.DeleteWatchHistoryItem(cacheTestUser, "episode", cacheTestSeriesID+":s01e02"); err != nil {
					t.Fatalf("DeleteWatchHistoryItem() error = %v", err)
				}
			},
			verify: func(t *testing.T, svc *Service) {
				if got := nextEpisodeNumber(mustSeriesState(t, svc)); got != 2 {
					t.Fatalf("expected next episode 2 after deleting e2, got %d", got)
				}
			},
		},
		{
			name: "BulkUpdateWatchHistory",
			mutate: func(t *testing.T, svc *Service) {
				if _, err := svc.BulkUpdateWatchHistory(cacheTestUser, []models.WatchHistoryUpdate{
					cacheTestEpisodeUpdate(2, true), cacheTestEpisodeUpdate(3, true),
				}); err != nil {
					t.Fatalf("BulkUpdateWatchHistory() error = %v", err)
				}
			},
			verify: func(t *testing.T, svc *Service) {
				if got := nextEpisodeNumber(mustSeriesState(t, svc)); got != 4 {
					t.Fatalf("expected next episode 4 after bulk watch, got %d", got)
				}
			},
		},
		{
			name: "ImportWatchHistory",
			mutate: func(t *testing.T, svc *Service) {
				if _, err := svc.ImportWatchHistory(cacheTestUser, []models.WatchHistoryUpdate{
					cacheTestEpisodeUpdate(2, true), cacheTestEpisodeUpdate(3, true),
				}); err != nil {
					t.Fatalf("ImportWatchHistory() error = %v", err)
				}
			},
			verify: func(t *testing.T, svc *Service) {
				if got := nextEpisodeNumber(mustSeriesState(t, svc)); got != 4 {
					t.Fatalf("expected next episode 4 after import, got %d", got)
				}
			},
		},
		{
			name: "UpdatePlaybackProgress",
			mutate: func(t *testing.T, svc *Service) {
				if _, err := svc.UpdatePlaybackProgress(cacheTestUser, cacheTestProgressUpdate(2, 400)); err != nil {
					t.Fatalf("UpdatePlaybackProgress() error = %v", err)
				}
			},
			verify: func(t *testing.T, svc *Service) {
				state := mustSeriesState(t, svc)
				if state.ResumePercent <= 0 && state.PercentWatched <= 0 {
					t.Fatalf("expected in-progress resume state after progress update, got %+v", state)
				}
			},
		},
		{
			name: "UpdatePlaybackProgressAutoWatched",
			mutate: func(t *testing.T, svc *Service) {
				if _, err := svc.UpdatePlaybackProgress(cacheTestUser, cacheTestProgressUpdate(2, 950)); err != nil {
					t.Fatalf("UpdatePlaybackProgress() error = %v", err)
				}
			},
			verify: func(t *testing.T, svc *Service) {
				if got := nextEpisodeNumber(mustSeriesState(t, svc)); got != 3 {
					t.Fatalf("expected next episode 3 after auto-watched progress, got %d", got)
				}
			},
		},
		{
			name: "DeletePlaybackProgress",
			setup: func(t *testing.T, svc *Service) {
				if _, err := svc.UpdatePlaybackProgress(cacheTestUser, cacheTestProgressUpdate(2, 400)); err != nil {
					t.Fatalf("UpdatePlaybackProgress() error = %v", err)
				}
			},
			mutate: func(t *testing.T, svc *Service) {
				if err := svc.DeletePlaybackProgress(cacheTestUser, "episode", cacheTestSeriesID+":s01e02"); err != nil {
					t.Fatalf("DeletePlaybackProgress() error = %v", err)
				}
			},
			verify: func(t *testing.T, svc *Service) {
				state := mustSeriesState(t, svc)
				if state.ResumePercent != 0 {
					t.Fatalf("expected no resume state after deleting progress, got %+v", state)
				}
			},
		},
		{
			name: "HideFromContinueWatching",
			mutate: func(t *testing.T, svc *Service) {
				if err := svc.HideFromContinueWatching(cacheTestUser, cacheTestSeriesID); err != nil {
					t.Fatalf("HideFromContinueWatching() error = %v", err)
				}
			},
		},
		{
			name: "ClearWatchHistory",
			mutate: func(t *testing.T, svc *Service) {
				if _, err := svc.ClearWatchHistory(); err != nil {
					t.Fatalf("ClearWatchHistory() error = %v", err)
				}
			},
			verify: func(t *testing.T, svc *Service) {
				states, err := svc.ListSeriesStates(cacheTestUser)
				if err != nil {
					t.Fatalf("ListSeriesStates() error = %v", err)
				}
				if len(states) != 0 {
					t.Fatalf("expected no series after clearing history, got %+v", states)
				}
			},
		},
		{
			name: "ClearPlaybackProgress",
			setup: func(t *testing.T, svc *Service) {
				if _, err := svc.UpdatePlaybackProgress(cacheTestUser, cacheTestProgressUpdate(2, 400)); err != nil {
					t.Fatalf("UpdatePlaybackProgress() error = %v", err)
				}
			},
			mutate: func(t *testing.T, svc *Service) {
				if _, err := svc.ClearPlaybackProgress(); err != nil {
					t.Fatalf("ClearPlaybackProgress() error = %v", err)
				}
			},
		},
		{
			name: "RecordEpisode",
			mutate: func(t *testing.T, svc *Service) {
				_, _ = svc.RecordEpisode(cacheTestUser, models.EpisodeWatchPayload{
					SeriesID: cacheTestSeriesID, SeriesTitle: "Cache Show",
					Episode: models.EpisodeReference{SeasonNumber: 1, EpisodeNumber: 2},
				})
			},
		},
		{
			name: "SetMetadataService",
			mutate: func(t *testing.T, svc *Service) {
				svc.SetMetadataService(&countingMetadataService{mockMetadataService: &mockMetadataService{seriesDetails: cacheTestSeriesDetails()}})
			},
		},
		{
			name: "AirtimeGenerationChange",
			mutate: func(t *testing.T, svc *Service) {
				svc.mu.Lock()
				svc.airtimeState.generation = time.Now().UnixNano()
				svc.mu.Unlock()
			},
		},
		{
			name: "ImportWatchHistoryUnwatch",
			mutate: func(t *testing.T, svc *Service) {
				unwatched := false
				update := cacheTestEpisodeUpdate(1, true)
				update.Watched = &unwatched
				if _, err := svc.UpdateWatchHistory(cacheTestUser, update); err != nil {
					t.Fatalf("UpdateWatchHistory(unwatch) error = %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newSeriesStatesCacheService(t)
			mustWatch(t, svc, 1, true)
			if tc.setup != nil {
				tc.setup(t, svc)
			}
			_, _ = svc.ListSeriesStates(cacheTestUser)
			before := currentRevision(svc, cacheTestUser)
			entry := cachedSeriesStatesEntry(svc, cacheTestUser)
			if entry == nil {
				t.Fatal("expected cache entry before mutation")
			}

			tc.mutate(t, svc)

			after := currentRevision(svc, cacheTestUser)
			if after == before {
				t.Fatalf("%s did not change the watch state revision", tc.name)
			}
			if _, err := svc.ListSeriesStates(cacheTestUser); err != nil {
				t.Fatalf("ListSeriesStates() error = %v", err)
			}
			if got := cachedSeriesStatesEntry(svc, cacheTestUser); got == entry {
				t.Fatalf("%s: stale cache entry was served after mutation", tc.name)
			}
			if tc.verify != nil {
				tc.verify(t, svc)
			}
		})
	}
}

func TestListSeriesStatesRevisionIsPerUser(t *testing.T) {
	svc, _ := newSeriesStatesCacheService(t)
	mustWatch(t, svc, 1, true)
	_ = mustSeriesState(t, svc)
	entry := cachedSeriesStatesEntry(svc, cacheTestUser)

	other := cacheTestEpisodeUpdate(1, true)
	if _, err := svc.UpdateWatchHistory("someone-else", other); err != nil {
		t.Fatalf("UpdateWatchHistory(other) error = %v", err)
	}
	_ = mustSeriesState(t, svc)
	if got := cachedSeriesStatesEntry(svc, cacheTestUser); got != entry {
		t.Fatal("another user's mutation should not invalidate this user's cache")
	}
}

func TestListSeriesStatesTTLSafetyNet(t *testing.T) {
	svc, _ := newSeriesStatesCacheService(t)
	mustWatch(t, svc, 1, true)
	_ = mustSeriesState(t, svc)
	entry := cachedSeriesStatesEntry(svc, cacheTestUser)

	svc.mu.Lock()
	entry.expiresAt = time.Now().Add(-time.Second)
	svc.mu.Unlock()

	_ = mustSeriesState(t, svc)
	if got := cachedSeriesStatesEntry(svc, cacheTestUser); got == entry {
		t.Fatal("expired cache entry should be rebuilt")
	}
}

func TestListSeriesStatesExpiryCappedByActiveHeartbeat(t *testing.T) {
	svc, _ := newSeriesStatesCacheService(t)
	mustWatch(t, svc, 1, true)
	if _, err := svc.UpdatePlaybackProgress(cacheTestUser, cacheTestProgressUpdate(2, 400)); err != nil {
		t.Fatalf("UpdatePlaybackProgress() error = %v", err)
	}
	_ = mustSeriesState(t, svc)
	entry := cachedSeriesStatesEntry(svc, cacheTestUser)
	if entry == nil {
		t.Fatal("expected cache entry")
	}
	if limit := time.Now().Add(activeProgressTTL + time.Second); entry.expiresAt.After(limit) {
		t.Fatalf("cache expiry %v should be capped by live heartbeat TTL (%v)", entry.expiresAt, limit)
	}
}

// A mutation that lands while a build is in flight must prevent that build's
// (possibly pre-mutation) result from being cached.
func TestListSeriesStatesSkipsCachingWhenRevisionChangesDuringBuild(t *testing.T) {
	svc, _ := newSeriesStatesCacheService(t)
	mustWatch(t, svc, 1, true)

	blocking := &blockingMetadataService{
		countingMetadataService: &countingMetadataService{mockMetadataService: &mockMetadataService{seriesDetails: cacheTestSeriesDetails()}},
		entered:                 make(chan struct{}),
		release:                 make(chan struct{}),
	}
	svc.SetMetadataService(blocking)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = svc.ListSeriesStates(cacheTestUser)
	}()
	select {
	case <-blocking.entered:
	case <-time.After(10 * time.Second):
		close(blocking.release)
		t.Fatal("build never reached the metadata lookup")
	}
	mustWatch(t, svc, 2, true)
	close(blocking.release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("build did not finish")
	}

	if entry := cachedSeriesStatesEntry(svc, cacheTestUser); entry != nil {
		t.Fatal("build that raced a mutation must not be cached")
	}
	if got := nextEpisodeNumber(mustSeriesState(t, svc)); got != 3 {
		t.Fatalf("expected next episode 3 after concurrent watch, got %d", got)
	}
}

func TestListSeriesStatesConcurrentMissesShareOneBuild(t *testing.T) {
	svc, _ := newSeriesStatesCacheService(t)
	mustWatch(t, svc, 1, true)

	// Only the first lookup blocks; any separate build would pass straight
	// through (unlike sync.Once, which would make later callers wait too).
	blocking := &firstLookupBlockingMetadataService{
		countingMetadataService: &countingMetadataService{mockMetadataService: &mockMetadataService{seriesDetails: cacheTestSeriesDetails()}},
		entered:                 make(chan struct{}),
		release:                 make(chan struct{}),
	}
	svc.SetMetadataService(blocking)

	const callers = 4
	results := make(chan []models.SeriesWatchState, callers)
	go func() {
		states, _ := svc.ListSeriesStates(cacheTestUser)
		results <- states
	}()
	select {
	case <-blocking.entered:
	case <-time.After(10 * time.Second):
		close(blocking.release)
		t.Fatal("build never reached the metadata lookup")
	}
	for i := 1; i < callers; i++ {
		go func() {
			states, _ := svc.ListSeriesStates(cacheTestUser)
			results <- states
		}()
	}

	// Joiners must wait on the in-flight build rather than run their own.
	select {
	case <-results:
		close(blocking.release)
		t.Fatal("a concurrent caller finished while the shared build was still blocked")
	case <-time.After(150 * time.Millisecond):
	}
	close(blocking.release)

	var all [][]models.SeriesWatchState
	for i := 0; i < callers; i++ {
		select {
		case states := <-results:
			if len(states) != 1 {
				t.Fatalf("caller got %d series states, want 1", len(states))
			}
			all = append(all, states)
		case <-time.After(10 * time.Second):
			t.Fatal("callers did not finish after the build was released")
		}
	}
	// Each caller owns its result.
	all[0][0].SeriesTitle = "mutated"
	for i := 1; i < len(all); i++ {
		if all[i][0].SeriesTitle == "mutated" {
			t.Fatalf("caller %d shares backing storage with caller 0", i)
		}
	}
}

type firstLookupBlockingMetadataService struct {
	*countingMetadataService
	lookups atomic.Int64
	entered chan struct{}
	release chan struct{}
}

func (m *firstLookupBlockingMetadataService) SeriesDetailsLite(ctx context.Context, query models.SeriesDetailsQuery) (*models.SeriesDetails, error) {
	if m.lookups.Add(1) == 1 {
		close(m.entered)
		<-m.release
	}
	return m.countingMetadataService.SeriesDetailsLite(ctx, query)
}

type blockingMetadataService struct {
	*countingMetadataService
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (m *blockingMetadataService) SeriesDetailsLite(ctx context.Context, query models.SeriesDetailsQuery) (*models.SeriesDetails, error) {
	m.once.Do(func() {
		close(m.entered)
		<-m.release
	})
	return m.countingMetadataService.SeriesDetailsLite(ctx, query)
}

func TestSeriesMetadataNegativeCache(t *testing.T) {
	svc, meta := newSeriesStatesCacheService(t)
	meta.failSeries = map[string]bool{cacheTestSeriesID: true}
	ctx := context.Background()

	if _, err := svc.getSeriesMetadataWithCache(ctx, cacheTestSeriesID, "Cache Show", nil); err == nil {
		t.Fatal("expected lookup failure")
	}
	if meta.liteCalls.Load() != 1 || meta.fullCalls.Load() != 1 {
		t.Fatalf("expected one lite + one full call, got lite=%d full=%d", meta.liteCalls.Load(), meta.fullCalls.Load())
	}

	// Within the negative TTL the failure is replayed without upstream calls.
	if _, err := svc.getSeriesMetadataWithCache(ctx, cacheTestSeriesID, "Cache Show", nil); err == nil {
		t.Fatal("expected cached lookup failure")
	}
	if meta.liteCalls.Load() != 1 || meta.fullCalls.Load() != 1 {
		t.Fatalf("negative cache should suppress retries, got lite=%d full=%d", meta.liteCalls.Load(), meta.fullCalls.Load())
	}

	svc.mu.Lock()
	entry := svc.metadataCache[cacheTestSeriesID]
	if entry == nil || entry.err == nil {
		svc.mu.Unlock()
		t.Fatal("expected negative cache entry")
	}
	if ttl := entry.expiresAt.Sub(entry.cachedAt); ttl < 15*time.Minute || ttl > 60*time.Minute {
		svc.mu.Unlock()
		t.Fatalf("negative cache TTL %v outside 15-60 minute range", ttl)
	}
	// Expire it; the provider has recovered.
	entry.expiresAt = time.Now().Add(-time.Second)
	svc.mu.Unlock()
	meta.failSeries = nil

	details, err := svc.getSeriesMetadataWithCache(ctx, cacheTestSeriesID, "Cache Show", nil)
	if err != nil || details == nil {
		t.Fatalf("expected recovery after negative TTL, got details=%v err=%v", details, err)
	}
	if meta.liteCalls.Load() != 2 {
		t.Fatalf("expected a fresh upstream call after negative TTL, got lite=%d", meta.liteCalls.Load())
	}
}

func TestSeriesMetadataNegativeCacheIgnoresCallerCancellation(t *testing.T) {
	svc, meta := newSeriesStatesCacheService(t)
	meta.failSeries = map[string]bool{cacheTestSeriesID: true}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := svc.getSeriesMetadataWithCache(ctx, cacheTestSeriesID, "Cache Show", nil); err == nil {
		t.Fatal("expected lookup failure")
	}
	svc.mu.RLock()
	entry := svc.metadataCache[cacheTestSeriesID]
	svc.mu.RUnlock()
	if entry != nil {
		t.Fatal("cancelled lookups must not be negative-cached")
	}
}

func TestListPlaybackProgressPrunesExpiredActiveEntriesWithoutChangingOutput(t *testing.T) {
	svc, _ := newSeriesStatesCacheService(t)
	if _, err := svc.UpdatePlaybackProgress(cacheTestUser, cacheTestProgressUpdate(2, 400)); err != nil {
		t.Fatalf("UpdatePlaybackProgress() error = %v", err)
	}

	// Age the live heartbeat past its TTL and make it look "fresher" than the
	// stored row so it would win if it were (incorrectly) overlaid.
	svc.mu.Lock()
	for key, progress := range svc.activePlaybackProgress[cacheTestUser] {
		progress.UpdatedAt = time.Now().Add(-activeProgressTTL - time.Minute)
		progress.Position = 999
		svc.activePlaybackProgress[cacheTestUser][key] = progress
	}
	svc.mu.Unlock()
	revBefore := currentRevision(svc, cacheTestUser)

	items, err := svc.ListPlaybackProgress(cacheTestUser)
	if err != nil {
		t.Fatalf("ListPlaybackProgress() error = %v", err)
	}
	if len(items) != 1 || items[0].Position != 400 {
		t.Fatalf("expected stored progress (position 400), got %+v", items)
	}
	svc.mu.RLock()
	_, stillActive := svc.activePlaybackProgress[cacheTestUser]
	svc.mu.RUnlock()
	if stillActive {
		t.Fatal("expected expired active heartbeat to be pruned")
	}
	if currentRevision(svc, cacheTestUser) != revBefore {
		t.Fatal("pruning expired heartbeats must not bump the watch state revision")
	}
}

// Exercises ListPlaybackProgress (shared lock + conditional prune) concurrently
// with progress writes and series state reads. Run with -race.
func TestListPlaybackProgressConcurrentWithWrites(t *testing.T) {
	svc, _ := newSeriesStatesCacheService(t)
	mustWatch(t, svc, 1, true)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := svc.ListPlaybackProgress(cacheTestUser); err != nil {
					t.Errorf("ListPlaybackProgress() error = %v", err)
					return
				}
				if _, err := svc.ListSeriesStates(cacheTestUser); err != nil {
					t.Errorf("ListSeriesStates() error = %v", err)
					return
				}
			}
		}()
	}
	for i := 0; i < 30; i++ {
		update := cacheTestProgressUpdate(2, float64(100+i))
		if _, err := svc.UpdatePlaybackProgress(cacheTestUser, update); err != nil {
			t.Fatalf("UpdatePlaybackProgress() error = %v", err)
		}
		// Periodically age heartbeats so readers race on the prune path.
		if i%5 == 0 {
			svc.mu.Lock()
			for key, progress := range svc.activePlaybackProgress[cacheTestUser] {
				progress.UpdatedAt = time.Now().Add(-activeProgressTTL - time.Minute)
				svc.activePlaybackProgress[cacheTestUser][key] = progress
			}
			svc.mu.Unlock()
		}
	}
	close(stop)
	wg.Wait()

	items, err := svc.ListPlaybackProgress(cacheTestUser)
	if err != nil || len(items) != 1 || items[0].Position != 129 {
		t.Fatalf("expected final stored progress position 129, got %+v err=%v", items, err)
	}
}
