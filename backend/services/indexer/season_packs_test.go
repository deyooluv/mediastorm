package indexer

import (
	"strings"
	"testing"

	"novastream/models"
	"novastream/services/debrid"
)

func TestSeasonPacksOnlyReachesFilterAndCacheKey(t *testing.T) {
	s := &Service{}
	opts := SearchOptions{Query: "Example Show S01", MediaType: "series", SeasonPacksOnly: true}
	filterOpts := s.buildFilterOptions(opts, models.FilterSettings{}, []string{"Example Show"})
	if !filterOpts.SeasonPacksOnly || filterOpts.TargetSeason != 1 {
		t.Fatalf("filter options = packsOnly:%v S%d", filterOpts.SeasonPacksOnly, filterOpts.TargetSeason)
	}
	plain := opts
	plain.SeasonPacksOnly = false
	if !buildSearchCacheOptions(opts).SeasonPacksOnly || buildSearchCacheOptions(plain).SeasonPacksOnly {
		t.Fatal("season-pack searches must not share a cache entry with plain season searches")
	}
}

func TestSeasonPackQueriesDoNotTargetFirstEpisode(t *testing.T) {
	opts := SearchOptions{Query: "Example Show S01", MediaType: "series"}
	parsed := debrid.ParseQuery(opts.Query)

	plain := buildSearchQueries(opts, parsed, nil)
	if !containsQuery(plain, "S01E01") {
		t.Fatalf("precondition: plain season search composes an E01 query, got %v", plain)
	}
	opts.SeasonPacksOnly = true
	packs := buildSearchQueries(opts, parsed, nil)
	if containsQuery(packs, "S01E01") {
		t.Fatalf("season-pack search queries = %v, want no E01 query", packs)
	}
	if !containsQuery(packs, "Example Show S01") {
		t.Fatalf("season-pack search queries = %v, want the season query", packs)
	}
}

func containsQuery(queries []string, needle string) bool {
	for _, q := range queries {
		if strings.Contains(q, needle) {
			return true
		}
	}
	return false
}
