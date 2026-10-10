package handlers

import (
	"net/http/httptest"
	"testing"
	"time"

	"novastream/models"
)

func TestDisplayListQueryUnwatchedCountsActivatesEnrichment(t *testing.T) {
	req := httptest.NewRequest("GET", "/?includeUnwatchedCounts=true", nil)
	query := parseDisplayListQuery(req)
	if !query.IncludeUnwatchedCounts || !query.Active() {
		t.Fatalf("unwatched-count query did not activate enrichment: %#v", query)
	}
}

func TestDisplayListQueryAppliesBeforePagination(t *testing.T) {
	items := []models.TrendingItem{
		{Rank: 1, Title: models.Title{Name: "The Zebra", MediaType: "series", Year: 2020, Genres: []string{"History"}}},
		{Rank: 2, Title: models.Title{Name: "Alpha", MediaType: "movie", Year: 2024, Genres: []string{"Drama"}}},
		{Rank: 3, Title: models.Title{Name: "A Bravo", MediaType: "series", Year: 2022, Genres: []string{"History"}}},
	}
	query := displayListQueryOptions{
		MediaType:     "series",
		Genres:        []string{"history"},
		SortBy:        "name",
		SortDirection: "asc",
	}

	filtered := query.Apply(items)
	if len(filtered) != 2 {
		t.Fatalf("expected 2 filtered items, got %d", len(filtered))
	}
	if filtered[0].Title.Name != "A Bravo" || filtered[1].Title.Name != "The Zebra" {
		t.Fatalf("unexpected order: %q, %q", filtered[0].Title.Name, filtered[1].Title.Name)
	}
	page := paginateTrendingItems(filtered, 1, 1)
	if len(page) != 1 || page[0].Title.Name != "The Zebra" {
		t.Fatalf("unexpected page: %#v", page)
	}
}

func TestQueryWatchlistItemsFiltersSortsAndReturnsGenres(t *testing.T) {
	items := []models.WatchlistItem{
		{Name: "Zulu", MediaType: "series", Genres: []string{"History"}, WatchState: "partial", AddedAt: time.Unix(1, 0)},
		{Name: "Alpha", MediaType: "series", Genres: []string{"History"}, WatchState: "partial", AddedAt: time.Unix(2, 0)},
		{Name: "Movie", MediaType: "movie", Genres: []string{"Drama"}, WatchState: "none", AddedAt: time.Unix(3, 0)},
	}
	result, genres, _ := queryWatchlistItems(items, displayListQueryOptions{
		MediaType: "series", WatchStatus: "partial", Genres: []string{"history"}, SortBy: "name", SortDirection: "asc",
	})
	if len(result) != 2 || result[0].Name != "Alpha" || result[1].Name != "Zulu" {
		t.Fatalf("unexpected query result: %#v", result)
	}
	if len(genres) != 2 || genres[0] != "Drama" || genres[1] != "History" {
		t.Fatalf("unexpected genres: %#v", genres)
	}
}

func TestDisplayListRatingSortKeepsMissingRatingsLast(t *testing.T) {
	items := []models.TrendingItem{
		{Title: models.Title{Name: "Missing"}},
		{Title: models.Title{Name: "Low", Ratings: []models.Rating{{Source: "imdb", Value: 6, Max: 10}}}},
		{Title: models.Title{Name: "High", Ratings: []models.Rating{{Source: "imdb", Value: 9, Max: 10}}}},
	}
	query := displayListQueryOptions{SortBy: "rating", SortDirection: "desc", RatingSource: "imdb"}
	result := query.Apply(items)
	if result[0].Title.Name != "High" || result[1].Title.Name != "Low" || result[2].Title.Name != "Missing" {
		t.Fatalf("unexpected rating order: %q, %q, %q", result[0].Title.Name, result[1].Title.Name, result[2].Title.Name)
	}
}

func TestDisplayListAlphabetUsesArticleNormalizedGlobalBucket(t *testing.T) {
	items := []models.TrendingItem{
		{Title: models.Title{Name: "The Zebra"}},
		{Title: models.Title{Name: "Alpha"}},
		{Title: models.Title{Name: "1917"}},
	}
	query := displayListQueryOptions{SortBy: "name", SortDirection: "asc", Alphabet: "Z"}
	result := query.Apply(items)
	if len(result) != 1 || result[0].Title.Name != "The Zebra" {
		t.Fatalf("unexpected alphabet result: %#v", result)
	}
	buckets := displayListAlphabetBuckets(items, query)
	if len(buckets) != 3 || buckets[0] != "#" || buckets[1] != "A" || buckets[2] != "Z" {
		t.Fatalf("unexpected alphabet buckets: %#v", buckets)
	}
}

func TestDisplayListWatchStatusAcceptsDescriptiveAliases(t *testing.T) {
	cases := map[string]string{
		"":            "",
		"all":         "all",
		"none":        "none",
		"Unwatched":   "none",
		"partial":     "partial",
		"in_progress": "partial",
		"watching":    "partial",
		"complete":    "complete",
		"watched":     "complete",
	}
	for input, want := range cases {
		req := httptest.NewRequest("GET", "/display-list?watchStatus="+input, nil)
		if got := parseDisplayListQuery(req).WatchStatus; got != want {
			t.Fatalf("watchStatus=%q: got %q want %q", input, got, want)
		}
	}
}
