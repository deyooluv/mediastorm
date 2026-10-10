package handlers_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"

	"novastream/handlers"
	"novastream/models"
)

func TestUpdatePlaybackProgressBuildsEpisodeItemIDFromParts(t *testing.T) {
	svc := &fakeHistoryService{}
	handler := handlers.NewHistoryHandler(svc, fakeUserService{}, false)
	body := bytes.NewBufferString(`{"mediaType":"episode","seriesId":"tmdb:tv:95350","seasonNumber":1,"episodeNumber":2,"position":600,"duration":1200}`)
	req := httptest.NewRequest(http.MethodPost, "/api/users/u1/history/progress", body)
	req = mux.SetURLVars(req, map[string]string{"userID": "u1"})
	rec := httptest.NewRecorder()

	handler.UpdatePlaybackProgress(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got, want := svc.lastProgress.ItemID, "tmdb:tv:95350:s01e02"; got != want {
		t.Fatalf("itemId = %q, want %q", got, want)
	}
	var resp models.PlaybackProgress
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Resume == nil || !resp.Resume.Eligible || resp.Resume.Percent != 50 || resp.Resume.Position != 600 || resp.Resume.Duration != 1200 {
		t.Fatalf("unexpected resume %+v", resp.Resume)
	}
}

func TestUpdatePlaybackProgressLegacyItemIDStillAccepted(t *testing.T) {
	svc := &fakeHistoryService{}
	handler := handlers.NewHistoryHandler(svc, fakeUserService{}, false)
	body := bytes.NewBufferString(`{"mediaType":"episode","itemId":"tvdb:series:1:S01E03","seriesId":"tvdb:series:1","seasonNumber":1,"episodeNumber":3,"position":10,"duration":100}`)
	req := httptest.NewRequest(http.MethodPost, "/api/users/u1/history/progress", body)
	req = mux.SetURLVars(req, map[string]string{"userID": "u1"})
	rec := httptest.NewRecorder()

	handler.UpdatePlaybackProgress(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if svc.lastProgress.ItemID != "tvdb:series:1:S01E03" {
		t.Fatalf("explicit itemId was rewritten: %q", svc.lastProgress.ItemID)
	}
}

func TestUpdatePlaybackProgressRejectsUnaddressableEpisode(t *testing.T) {
	svc := &fakeHistoryService{}
	handler := handlers.NewHistoryHandler(svc, fakeUserService{}, false)
	body := bytes.NewBufferString(`{"mediaType":"episode","seriesId":"tmdb:tv:1","position":10,"duration":100}`)
	req := httptest.NewRequest(http.MethodPost, "/api/users/u1/history/progress", body)
	req = mux.SetURLVars(req, map[string]string{"userID": "u1"})
	rec := httptest.NewRecorder()

	handler.UpdatePlaybackProgress(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestUpdateWatchHistoryBuildsEpisodeItemIDFromParts(t *testing.T) {
	svc := &fakeHistoryService{}
	handler := handlers.NewHistoryHandler(svc, fakeUserService{}, false)
	body := bytes.NewBufferString(`{"mediaType":"episode","seriesId":"tvdb:series:5","externalIds":{"tmdb":"77"},"seasonNumber":2,"episodeNumber":4,"watched":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/users/u1/history/watched", body)
	req = mux.SetURLVars(req, map[string]string{"userID": "u1"})
	rec := httptest.NewRecorder()

	handler.UpdateWatchHistory(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got, want := svc.lastWatch.ItemID, "tmdb:tv:77:s02e04"; got != want {
		t.Fatalf("itemId = %q, want %q", got, want)
	}
}

func TestListPlaybackProgressTitleFilterAndResume(t *testing.T) {
	svc := &fakeHistoryService{progressItems: []models.PlaybackProgress{
		{ID: "episode:tmdb:tv:1:s01e01", MediaType: "episode", ItemID: "tmdb:tv:1:s01e01", SeriesID: "tmdb:tv:1", SeasonNumber: 1, EpisodeNumber: 1, Position: 300, Duration: 1000, PercentWatched: 30},
		{ID: "episode:tmdb:tv:2:s01e01", MediaType: "episode", ItemID: "tmdb:tv:2:s01e01", SeriesID: "tmdb:tv:2", SeasonNumber: 1, EpisodeNumber: 1, Position: 990, Duration: 1000, PercentWatched: 99},
	}}
	handler := handlers.NewHistoryHandler(svc, fakeUserService{}, false)

	req := httptest.NewRequest(http.MethodGet, "/api/users/u1/history/progress?titleId=tmdb:tv:1&type=series", nil)
	req = mux.SetURLVars(req, map[string]string{"userID": "u1"})
	rec := httptest.NewRecorder()
	handler.ListPlaybackProgress(rec, req)

	var scoped []models.PlaybackProgress
	if err := json.Unmarshal(rec.Body.Bytes(), &scoped); err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 1 || scoped[0].SeriesID != "tmdb:tv:1" {
		t.Fatalf("unexpected scoped list %+v", scoped)
	}
	if scoped[0].Resume == nil || !scoped[0].Resume.Eligible || scoped[0].Resume.Percent != 30 {
		t.Fatalf("unexpected resume %+v", scoped[0].Resume)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/users/u1/history/progress", nil)
	req = mux.SetURLVars(req, map[string]string{"userID": "u1"})
	rec = httptest.NewRecorder()
	handler.ListPlaybackProgress(rec, req)
	var all []models.PlaybackProgress
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("unscoped list len = %d, want 2", len(all))
	}
	if all[1].Resume == nil || all[1].Resume.Eligible {
		t.Fatalf("99%% progress must not be resume-eligible: %+v", all[1].Resume)
	}
}

func TestListContinueWatchingIncludesServerResume(t *testing.T) {
	svc := &fakeHistoryService{
		items: []models.SeriesWatchState{{
			SeriesID:    "tmdb:tv:9",
			ExternalIDs: map[string]string{"tmdb": "9"},
			LastWatched: models.EpisodeReference{SeasonNumber: 1, EpisodeNumber: 2},
			NextEpisode: &models.EpisodeReference{SeasonNumber: 1, EpisodeNumber: 2},
		}},
		progressItems: []models.PlaybackProgress{{
			MediaType: "episode", ItemID: "tmdb:tv:9:s01e02", SeriesID: "tmdb:tv:9",
			SeasonNumber: 1, EpisodeNumber: 2, Position: 120, Duration: 1200, PercentWatched: 10,
		}},
	}
	handler := handlers.NewHistoryHandler(svc, fakeUserService{}, false)
	req := httptest.NewRequest(http.MethodGet, "/users/u1/history/continue", nil)
	req = mux.SetURLVars(req, map[string]string{"userID": "u1"})
	rec := httptest.NewRecorder()
	handler.ListContinueWatching(rec, req)

	var items []models.SeriesWatchState
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("len = %d", len(items))
	}
	r := items[0].Resume
	if r == nil || !r.Eligible || r.Position != 120 || r.Duration != 1200 || r.Percent != 10 {
		t.Fatalf("unexpected resume %+v", r)
	}
	if items[0].NextEpisode.ItemID != "tmdb:tv:9:s01e02" {
		t.Fatalf("nextEpisode.itemId = %q", items[0].NextEpisode.ItemID)
	}
}
