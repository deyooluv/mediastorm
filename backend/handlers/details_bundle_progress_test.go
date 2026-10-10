package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"

	"novastream/handlers"
	"novastream/models"
)

func detailsBundleSeriesFixture() *models.SeriesDetails {
	return &models.SeriesDetails{
		Title: models.Title{Name: "Show", ID: "tvdb:series:123", TVDBID: 123, TMDBID: 95350},
		Seasons: []models.SeriesSeason{
			{Number: 0, Episodes: []models.SeriesEpisode{{SeasonNumber: 0, EpisodeNumber: 1, Name: "Special"}}},
			{Number: 1, Episodes: []models.SeriesEpisode{
				{SeasonNumber: 1, EpisodeNumber: 1, Name: "Pilot"},
				{SeasonNumber: 1, EpisodeNumber: 2, Name: "Two"},
				{SeasonNumber: 1, EpisodeNumber: 3, Name: "Three"},
			}},
		},
	}
}

func getDetailsBundle(t *testing.T, h *handlers.DetailsBundleHandler, query string) handlers.DetailsBundleResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/users/user1/details-bundle?"+query, nil)
	req = mux.SetURLVars(req, map[string]string{"userID": "user1"})
	rec := httptest.NewRecorder()
	h.GetDetailsBundle(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp handlers.DetailsBundleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestDetailsBundleAnnotatesEpisodesWithProgress(t *testing.T) {
	shared := detailsBundleSeriesFixture()
	h := handlers.NewDetailsBundleHandler(
		&mockMetadataServiceDetailsBundle{seriesDetails: shared},
		&mockHistoryServiceDetailsBundle{
			watchState: &models.SeriesWatchState{
				SeriesID:    "tvdb:series:123",
				ExternalIDs: map[string]string{"tvdb": "123", "tmdb": "95350"},
				LastWatched: models.EpisodeReference{SeasonNumber: 1, EpisodeNumber: 1},
				NextEpisode: &models.EpisodeReference{SeasonNumber: 1, EpisodeNumber: 2},
			},
			playbackProgress: []models.PlaybackProgress{
				{ID: "episode:tmdb:tv:95350:s01e02", MediaType: "episode", ItemID: "tmdb:tv:95350:s01e02", SeriesID: "tmdb:tv:95350",
					SeasonNumber: 1, EpisodeNumber: 2, Position: 600, Duration: 2400, PercentWatched: 25,
					ExternalIDs: map[string]string{"tmdb": "95350"}},
			},
			watchHistory: []models.WatchHistoryItem{
				{MediaType: "episode", ItemID: "tvdb:series:123:s01e01", SeriesID: "tvdb:series:123", SeasonNumber: 1, EpisodeNumber: 1, Watched: true},
				{MediaType: "episode", ItemID: "tvdb:series:999:s01e03", SeriesID: "tvdb:series:999", SeasonNumber: 1, EpisodeNumber: 3, Watched: true},
			},
		},
		&mockContentPrefsServiceDetailsBundle{},
		&mockUserServiceDetailsBundle{exists: true},
	)

	resp := getDetailsBundle(t, h, "type=series&titleId=tvdb:series:123&tvdbId=123")

	eps := resp.SeriesDetails.Seasons[1].Episodes
	if eps[0].ItemID != "tmdb:tv:95350:s01e01" || !eps[0].Watched || eps[0].Resume != nil {
		t.Fatalf("episode 1 = %+v", eps[0])
	}
	if eps[1].ItemID != "tmdb:tv:95350:s01e02" || eps[1].Watched || eps[1].Resume == nil || !eps[1].Resume.Eligible || eps[1].Resume.Position != 600 {
		t.Fatalf("episode 2 = %+v (resume %+v)", eps[1], eps[1].Resume)
	}
	if eps[2].Watched {
		t.Fatal("episode 3 watched by another series must not match")
	}
	if shared.Seasons[1].Episodes[0].ItemID != "" {
		t.Fatal("shared metadata value was mutated")
	}
	if resp.UpNextEpisode == nil || resp.UpNextEpisode.EpisodeNumber != 2 || resp.UpNextEpisode.ItemID != "tmdb:tv:95350:s01e02" {
		t.Fatalf("upNextEpisode = %+v", resp.UpNextEpisode)
	}
	if resp.Resume == nil || !resp.Resume.Eligible || resp.Resume.Percent != 25 {
		t.Fatalf("resume = %+v", resp.Resume)
	}
	if resp.WatchState.Resume == nil || resp.WatchState.NextEpisode.ItemID != "tmdb:tv:95350:s01e02" {
		t.Fatalf("watchState = %+v", resp.WatchState)
	}
	if resp.PlaybackProgress[0].Resume == nil {
		t.Fatal("playbackProgress rows must carry resume")
	}
}

func TestDetailsBundleUpNextFallsBackToFirstRegularEpisode(t *testing.T) {
	h := handlers.NewDetailsBundleHandler(
		&mockMetadataServiceDetailsBundle{seriesDetails: detailsBundleSeriesFixture()},
		&mockHistoryServiceDetailsBundle{},
		&mockContentPrefsServiceDetailsBundle{},
		&mockUserServiceDetailsBundle{exists: true},
	)
	resp := getDetailsBundle(t, h, "type=series&titleId=tvdb:series:123")
	if resp.UpNextEpisode == nil || resp.UpNextEpisode.SeasonNumber != 1 || resp.UpNextEpisode.EpisodeNumber != 1 {
		t.Fatalf("upNextEpisode = %+v", resp.UpNextEpisode)
	}
	if resp.Resume == nil || resp.Resume.Eligible {
		t.Fatalf("resume = %+v, want present and ineligible", resp.Resume)
	}
}

func TestDetailsBundleMovieResume(t *testing.T) {
	h := handlers.NewDetailsBundleHandler(
		&mockMetadataServiceDetailsBundle{movieDetails: &models.Title{Name: "M", ID: "tmdb:movie:603", TMDBID: 603}},
		&mockHistoryServiceDetailsBundle{playbackProgress: []models.PlaybackProgress{
			{ID: "movie:tmdb:movie:603", MediaType: "movie", ItemID: "tmdb:movie:603", Position: 4800, Duration: 8000, PercentWatched: 60},
		}},
		&mockContentPrefsServiceDetailsBundle{},
		&mockUserServiceDetailsBundle{exists: true},
	)
	resp := getDetailsBundle(t, h, "type=movie&titleId=tmdb:movie:603&tmdbId=603")
	if resp.Resume == nil || !resp.Resume.Eligible || resp.Resume.Position != 4800 || resp.Resume.Duration != 8000 || resp.Resume.Percent != 60 {
		t.Fatalf("resume = %+v", resp.Resume)
	}
}
