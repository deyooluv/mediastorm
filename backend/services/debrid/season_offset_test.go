package debrid

import (
	"testing"

	"novastream/models"
)

func TestBakeOffChannel4AddsContinuousIMDbRequest(t *testing.T) {
	req := SearchRequest{TitleID: "tmdb:tv:87012", Numbering: &models.EpisodeNumbering{SeriesID: "tmdb:tv:87012", Ordering: "official"}, Parsed: ParsedQuery{Title: "The Great British Bake Off", MediaType: MediaTypeSeries, Season: 10, Episode: 3}}
	requests := mappedSearchRequests(req, true)
	if len(requests) != 2 || requests[0].Parsed.Season != 10 {
		t.Fatalf("catalog request must be kept: %+v", requests)
	}
	if got := requests[1]; got.IMDBID != "tt1877368" || got.Parsed.Season != 17 || got.Parsed.Episode != 3 {
		t.Fatalf("mapped request = %+v", got)
	}
}
