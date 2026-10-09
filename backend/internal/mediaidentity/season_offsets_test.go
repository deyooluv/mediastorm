package mediaidentity

import (
	"testing"

	"novastream/models"
)

func TestBakeOffChannel4SeasonsAliasToContinuousNumbering(t *testing.T) {
	tmdbNumbering := &models.EpisodeNumbering{SeriesID: "tmdb:tv:87012", Ordering: "official"}
	for _, tc := range []struct{ season, episode, want int }{{1, 1, 8}, {10, 3, 17}, {11, 1, 18}} {
		got := ReleaseEpisodeAliases("tmdb:tv:87012", tc.season, tc.episode, tmdbNumbering)
		if len(got) != 1 || got[0].Season != tc.want || got[0].Episode != tc.episode || got[0].TVDBID != 184871 || got[0].IMDBID != "tt1877368" || got[0].Source != "season-offset" {
			t.Fatalf("S%02dE%02d aliases = %+v, want S%02dE%02d", tc.season, tc.episode, got, tc.want, tc.episode)
		}
	}
}

func TestSeasonOffsetsKeepCatalogIdentityAndProviderNumbering(t *testing.T) {
	// TVDB-numbered episodes for the same catalog title already use S8–S17.
	if got := ReleaseEpisodeAliases("tmdb:tv:87012", 8, 1, &models.EpisodeNumbering{SeriesID: "tvdb:series:184871", Ordering: "official"}); len(got) != 0 {
		t.Fatalf("TVDB numbering remapped: %+v", got)
	}
	// Scrobble/history identity is unchanged.
	if _, ok := KnownAnthologyEpisode("tmdb:tv:87012", 10, 3); ok {
		t.Fatal("season offset leaked into anthology identity")
	}
	for _, tc := range []struct {
		id      string
		season  int
		episode int
	}{{"tmdb:tv:34549", 7, 1}, {"tmdb:tv:87012", 0, 1}, {"tmdb:tv:93544", 4, 1}, {"tmdb:tv:203187", 2, 1}} {
		if got := ReleaseEpisodeAliases(tc.id, tc.season, tc.episode); len(got) != 0 {
			t.Fatalf("%s S%02dE%02d unexpectedly aliased: %+v", tc.id, tc.season, tc.episode, got)
		}
	}
}

func TestSeasonOffsetRanges(t *testing.T) {
	for _, tc := range []struct {
		id           string
		season, want int
	}{{"tmdb:tv:93544", 3, 5}, {"tmdb:tv:235884", 3, 9}, {"tmdb:tv:67208", 1, 8}, {"tmdb:tv:72095", 2, 9}, {"tmdb:tv:75907", 2, 12}, {"tmdb:tv:203187", 1, 13}} {
		got, ok := seasonOffsetAlias(seriesSeasonOffsets, tc.id, tc.season, 4)
		if !ok || got.Season != tc.want || got.Episode != 4 {
			t.Fatalf("%s S%d = %+v %v, want S%d", tc.id, tc.season, got, ok, tc.want)
		}
	}
}
