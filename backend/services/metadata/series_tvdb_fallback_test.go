package metadata

import (
	"context"
	"encoding/json"
	"testing"

	"novastream/models"
)

func seedTVDBSeriesSearch(t *testing.T, cache *fileCache, title, remoteID, payload string) {
	t.Helper()
	var results []tvdbSearchResult
	if err := json.Unmarshal([]byte(payload), &results); err != nil {
		t.Fatalf("decode search payload: %v", err)
	}
	if err := cache.set(cacheKey("tvdb", "search", "series", title, "", remoteID), results); err != nil {
		t.Fatalf("seed search cache: %v", err)
	}
}

// TMDB external_ids can point at a TVDB record that was since deleted. The 404
// fallback must not resolve back through that stale TMDB→TVDB mapping.
func TestTryFallbackSeriesTVDBIDBypassesStaleTMDBMapping(t *testing.T) {
	const name = "Smoking Behind the Supermarket with You"
	cache := newFileCache(t.TempDir(), 24)
	if err := cache.set(seriesTVDBResolutionCacheKey(296286), int64(465973)); err != nil {
		t.Fatalf("seed resolution cache: %v", err)
	}
	seedTVDBSeriesSearch(t, cache, name, "tt37614297", `[
		{"tvdb_id":"465973","name":"`+name+`","primary_language":"jpn"},
		{"tvdb_id":"483430","name":"`+name+`","primary_language":"jpn",
		 "remote_ids":[{"id":"296286","sourceName":"TheMovieDB.com"},{"id":"tt37614297","sourceName":"IMDB"}]}
	]`)
	svc := &Service{client: newTVDBClient("tvdb-key", "eng", nil, 24), cache: cache, inflightRequests: make(map[string]*inflightRequest)}

	got := svc.tryFallbackSeriesTVDBID(context.Background(), models.SeriesDetailsQuery{
		TitleID: "tmdb:tv:296286", Name: name, Year: 2025, TMDBID: 296286, IMDBID: "tt37614297",
	}, 465973)
	if got != 483430 {
		t.Fatalf("fallback tvdb id = %d, want 483430", got)
	}
	var mapped int64
	if ok, _ := cache.get(seriesTVDBResolutionCacheKey(296286), &mapped); !ok || mapped != 483430 {
		t.Fatalf("tmdb→tvdb mapping = %d (ok=%v), want repointed to 483430", mapped, ok)
	}
}

func TestTryFallbackSeriesTVDBIDNameMatchKeepsTMDBMapping(t *testing.T) {
	const name = "Some Show"
	cache := newFileCache(t.TempDir(), 24)
	if err := cache.set(seriesTVDBResolutionCacheKey(111), int64(222)); err != nil {
		t.Fatalf("seed resolution cache: %v", err)
	}
	seedTVDBSeriesSearch(t, cache, name, "", `[
		{"tvdb_id":"222","name":"Some Show","primary_language":"eng"},
		{"tvdb_id":"333","name":"Some Show (Parent)","primary_language":"eng"}
	]`)
	svc := &Service{client: newTVDBClient("tvdb-key", "eng", nil, 24), cache: cache, inflightRequests: make(map[string]*inflightRequest)}

	got := svc.tryFallbackSeriesTVDBID(context.Background(), models.SeriesDetailsQuery{Name: name, TMDBID: 111}, 222)
	if got != 333 {
		t.Fatalf("fallback tvdb id = %d, want 333 (dead id excluded)", got)
	}
	var mapped int64
	if ok, _ := cache.get(seriesTVDBResolutionCacheKey(111), &mapped); !ok || mapped != 222 {
		t.Fatalf("tmdb→tvdb mapping = %d, want untouched 222 for a name-only match", mapped)
	}
}
