package models

import (
	"encoding/json"
	"testing"
	"time"
)

func TestReleaseAvailability(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	day := func(offset int) string { return now.AddDate(0, 0, offset).Format("2006-01-02") }

	cases := []struct {
		name       string
		mediaType  string
		status     string
		date       string
		theatrical *Release
		home       *Release
		want       string
	}{
		{"movie released status", "movie", MovieReleaseStatusReleased, "", nil, nil, AvailabilityReleased},
		{"movie theatrical status", "movie", MovieReleaseStatusTheatrical, "", nil, nil, AvailabilityTheatrical},
		{"movie upcoming soon", "movie", MovieReleaseStatusUpcoming, day(10), nil, nil, AvailabilityComingSoon},
		{"movie upcoming far", "movie", MovieReleaseStatusUpcoming, day(90), nil, nil, AvailabilityUnreleased},
		{"movie upcoming no date", "movie", MovieReleaseStatusUpcoming, "", nil, nil, AvailabilityUnreleased},
		{"movie unknown no date", "movie", MovieReleaseStatusUnknown, "", nil, nil, ""},
		{"movie unknown future date", "movie", MovieReleaseStatusUnknown, day(5), nil, nil, AvailabilityComingSoon},
		{"movie stale upcoming date passed", "movie", MovieReleaseStatusUpcoming, day(-3), nil, nil, AvailabilityTheatrical},
		{"movie home window wins", "movie", MovieReleaseStatusUpcoming, "", &Release{Type: "theatrical", Date: day(-60)}, &Release{Type: "digital", Date: day(-1)}, AvailabilityReleased},
		{"movie in theaters window", "movie", "", "", &Release{Type: "theatrical", Date: day(-20)}, &Release{Type: "digital", Date: day(40)}, AvailabilityTheatrical},
		{"movie future window soon", "movie", "", day(20), &Release{Type: "theatrical", Date: day(20)}, nil, AvailabilityComingSoon},
		{"series released", "series", SeriesReleaseStatusReleased, "", nil, nil, AvailabilityReleased},
		{"series unreleased soon", "series", SeriesReleaseStatusUnreleased, day(29), nil, nil, AvailabilityComingSoon},
		{"series unreleased far", "series", SeriesReleaseStatusUnreleased, day(31), nil, nil, AvailabilityUnreleased},
		{"series unreleased no date", "series", SeriesReleaseStatusUnreleased, "", nil, nil, AvailabilityUnreleased},
		{"series stale unreleased premiered", "series", SeriesReleaseStatusUnreleased, day(-1), nil, nil, AvailabilityReleased},
		{"series unknown", "series", "", "", nil, nil, ""},
		{"person", "person", "", day(1), nil, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReleaseAvailability(tc.mediaType, tc.status, tc.date, tc.theatrical, tc.home, nil, 0, now); got != tc.want {
				t.Fatalf("ReleaseAvailability() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTitleMarshalJSONDerivesReleaseDateAndAvailability(t *testing.T) {
	future := time.Now().AddDate(0, 0, 7).Format("2006-01-02")
	title := Title{
		ID:         "tmdb:movie:1",
		MediaType:  "movie",
		Status:     MovieReleaseStatusUpcoming,
		Theatrical: &Release{Type: "theatrical", Date: future + "T00:00:00Z"},
		HomeRelease: &Release{
			Type: "digital",
			Date: time.Now().AddDate(0, 3, 0).Format("2006-01-02"),
		},
	}
	raw, err := json.Marshal(TrendingItem{Rank: 1, Title: title})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		Title struct {
			ReleaseDate  string `json:"releaseDate"`
			Availability string `json:"availability"`
			Status       string `json:"status"`
		} `json:"title"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Title.ReleaseDate != future {
		t.Fatalf("releaseDate = %q, want %q", decoded.Title.ReleaseDate, future)
	}
	if decoded.Title.Availability != AvailabilityComingSoon {
		t.Fatalf("availability = %q, want %q", decoded.Title.Availability, AvailabilityComingSoon)
	}
	if decoded.Title.Status != MovieReleaseStatusUpcoming {
		t.Fatalf("status changed to %q", decoded.Title.Status)
	}

	// A stale cached availability is recomputed on encode.
	var roundTrip Title
	if err := json.Unmarshal([]byte(`{"mediaType":"series","status":"released","availability":"unreleased"}`), &roundTrip); err != nil {
		t.Fatalf("unmarshal title: %v", err)
	}
	raw, _ = json.Marshal(roundTrip)
	var check map[string]any
	_ = json.Unmarshal(raw, &check)
	if check["availability"] != AvailabilityReleased {
		t.Fatalf("availability = %v, want released", check["availability"])
	}
}

func TestWatchlistItemMarshalJSONAvailability(t *testing.T) {
	item := WatchlistItem{
		ID:          "m1",
		MediaType:   "movie",
		Status:      MovieReleaseStatusReleased,
		HomeRelease: &Release{Type: "digital", Date: "2020-05-01"},
	}
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var check map[string]any
	_ = json.Unmarshal(raw, &check)
	if check["availability"] != AvailabilityReleased || check["releaseDate"] != "2020-05-01" {
		t.Fatalf("got availability=%v releaseDate=%v", check["availability"], check["releaseDate"])
	}
}

func TestMovieWindowReleaseDateIgnoresPremieres(t *testing.T) {
	got := MovieWindowReleaseDate(nil, nil, []Release{
		{Type: "premiere", Date: "2026-01-10"},
		{Type: "theatrical", Date: "2026-03-01T00:00:00.000Z"},
		{Type: "digital", Date: "2026-05-01"},
	})
	if got != "2026-03-01" {
		t.Fatalf("MovieWindowReleaseDate() = %q, want 2026-03-01", got)
	}
}

func TestSeriesPremiereDateSkipsSpecials(t *testing.T) {
	seasons := []SeriesSeason{
		{Number: 0, Episodes: []SeriesEpisode{{AiredDate: "2020-01-01"}}},
		{Number: 1, Episodes: []SeriesEpisode{{AiredDate: "2027-02-03"}, {AiredDate: "2027-01-20"}, {}}},
	}
	if got := SeriesPremiereDate(seasons); got != "2027-01-20" {
		t.Fatalf("SeriesPremiereDate() = %q, want 2027-01-20", got)
	}
}

func TestMovieReleaseStatusUsesReleaseDateBeforeYearFallback(t *testing.T) {
	future := time.Now().AddDate(1, 0, 0)
	title := Title{MediaType: "movie", Year: time.Now().Year() - 1, ReleaseDate: future.Format("2006-01-02")}
	if got := MovieReleaseStatus(title); got != MovieReleaseStatusUpcoming {
		t.Fatalf("MovieReleaseStatus() = %q, want upcoming", got)
	}
}
