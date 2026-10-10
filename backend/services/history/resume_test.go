package history

import (
	"testing"
	"time"

	"novastream/models"
)

func TestComputeResumeThresholds(t *testing.T) {
	cases := []struct {
		name                    string
		position, duration, pct float64
		wantPercent             float64
		wantEligible            bool
	}{
		{"exactly 5% is not eligible", 50, 1000, 0, 5, false},
		{"just over 5% is eligible", 51, 1000, 0, 5.1, true},
		{"mid playback", 500, 1000, 0, 50, true},
		{"exactly 95% is not eligible", 950, 1000, 0, 95, false},
		{"percent-only import", 0, 0, 40, 40, true},
		{"overrun clamps to 100", 1200, 1000, 0, 100, false},
		{"no progress", 0, 0, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeResume(tc.position, tc.duration, tc.pct)
			if got.Percent != tc.wantPercent || got.Eligible != tc.wantEligible {
				t.Fatalf("ComputeResume = %+v, want percent=%v eligible=%v", got, tc.wantPercent, tc.wantEligible)
			}
			if got.Position != tc.position || got.Duration != tc.duration {
				t.Fatalf("position/duration not preserved: %+v", got)
			}
		})
	}
}

func TestProgressResumeUsesStoredPercent(t *testing.T) {
	got := ProgressResume(models.PlaybackProgress{Position: 0, Duration: 0, PercentWatched: 33})
	if !got.Eligible || got.Percent != 33 || got.Position != 0 || got.Duration != 0 {
		t.Fatalf("ProgressResume = %+v", got)
	}
}

func TestEpisodeItemIDPrefersTMDB(t *testing.T) {
	cases := []struct {
		seriesID string
		ext      map[string]string
		want     string
	}{
		{"tmdb:tv:95350", nil, "tmdb:tv:95350:s01e01"},
		{"tvdb:series:123", map[string]string{"tvdb": "123", "tmdb": "95350"}, "tmdb:tv:95350:s01e01"},
		{"tvdb:series:123", nil, "tvdb:series:123:s01e01"},
		{"tvdb:series:123", map[string]string{"episodeTmdb": "1"}, "tvdb:series:123:s01e01"},
	}
	for _, tc := range cases {
		if got := EpisodeItemID(tc.seriesID, tc.ext, 1, 1); got != tc.want {
			t.Fatalf("EpisodeItemID(%q, %v) = %q, want %q", tc.seriesID, tc.ext, got, tc.want)
		}
	}
	if got := EpisodeItemID("tmdb:tv:1", nil, 1, 0); got != "" {
		t.Fatalf("episode 0 must not be addressable, got %q", got)
	}
	if got := EpisodeItemID("", nil, 1, 1); got != "" {
		t.Fatalf("missing series must not be addressable, got %q", got)
	}
	if got := EpisodeItemID("tmdb:tv:1", nil, 0, 3); got != "tmdb:tv:1:s00e03" {
		t.Fatalf("specials should be addressable, got %q", got)
	}
}

func TestMergeProgressIntoContinueWatchingSetsResume(t *testing.T) {
	items := []models.SeriesWatchState{
		{
			SeriesID:    "tvdb:series:450033",
			ExternalIDs: map[string]string{"tvdb": "450033", "tmdb": "220102"},
			LastWatched: models.EpisodeReference{SeasonNumber: 1, EpisodeNumber: 1},
			NextEpisode: &models.EpisodeReference{SeasonNumber: 1, EpisodeNumber: 2},
		},
		{SeriesID: "tmdb:movie:603", PercentWatched: 97},
	}
	progress := []models.PlaybackProgress{
		{MediaType: "episode", ItemID: "tmdb:tv:220102:s01e02", SeriesID: "tmdb:tv:220102", SeasonNumber: 1, EpisodeNumber: 2,
			Position: 300, Duration: 1500, PercentWatched: 20, ExternalIDs: map[string]string{"tmdb": "220102"}},
		{MediaType: "movie", ItemID: "tmdb:movie:603", Position: 7000, Duration: 8000, PercentWatched: 87.5},
	}
	merged := MergeProgressIntoContinueWatching(items, progress)

	series := merged[0]
	if series.Resume == nil || !series.Resume.Eligible || series.Resume.Position != 300 || series.Resume.Duration != 1500 || series.Resume.Percent != 20 {
		t.Fatalf("series resume = %+v", series.Resume)
	}
	if series.ResumePercent != 20 {
		t.Fatalf("ResumePercent = %v", series.ResumePercent)
	}
	if series.NextEpisode.ItemID != "tmdb:tv:220102:s01e02" || series.LastWatched.ItemID != "tmdb:tv:220102:s01e01" {
		t.Fatalf("item ids next=%q last=%q", series.NextEpisode.ItemID, series.LastWatched.ItemID)
	}
	if items[0].NextEpisode.ItemID != "" {
		t.Fatal("input NextEpisode must not be mutated")
	}

	movie := merged[1]
	// The enriched 97% beats the stored 87.5% row, and is past the resume cap.
	if movie.Resume == nil || movie.Resume.Eligible || movie.Resume.Percent != 97 || movie.Resume.Position != 7000 {
		t.Fatalf("movie resume = %+v", movie.Resume)
	}
}

func TestSeriesProgressIndexPrefersLatestProgress(t *testing.T) {
	now := time.Now()
	idx := NewSeriesProgressIndex("tmdb:tv:1", nil, []models.PlaybackProgress{
		{MediaType: "episode", ItemID: "tmdb:tv:1:s01e01", SeasonNumber: 1, EpisodeNumber: 1, PercentWatched: 10, UpdatedAt: now.Add(-time.Hour)},
		{MediaType: "episode", ItemID: "tvdb:series:9:S01E01", PercentWatched: 60, UpdatedAt: now},
	}, []models.WatchHistoryItem{
		{MediaType: "episode", ItemID: "tmdb:tv:1:s01e02", SeasonNumber: 1, EpisodeNumber: 2, Watched: true},
		{MediaType: "episode", ItemID: "tmdb:tv:1:s01e03", SeasonNumber: 1, EpisodeNumber: 3, Watched: false},
	})
	if r := idx.Resume(1, 1); r == nil || r.Percent != 60 {
		t.Fatalf("resume = %+v, want latest (60%%)", r)
	}
	if !idx.Watched(1, 2) || idx.Watched(1, 3) {
		t.Fatal("watched flags wrong")
	}
	if idx.ItemID(1, 2) != "tmdb:tv:1:s01e02" {
		t.Fatalf("item id = %q", idx.ItemID(1, 2))
	}
}
