package filter

import (
	"testing"

	"novastream/models"
)

func TestSeasonPacksOnlyRejectsEpisodeReleases(t *testing.T) {
	resolver := NewSeriesEpisodeResolver(map[int]int{1: 10})
	for _, tc := range []struct {
		title    string
		resolver EpisodeCountResolver
		keep     bool
	}{
		{"Example.Show.S01.1080p.WEB-DL.DDP5.1.H.264-GRP", resolver, true},
		{"Example.Show.S01.COMPLETE.2160p.WEB-DL.DV.HDR-GRP", resolver, true},
		{"Example.Show.S01E01-E10.1080p.WEB-DL-GRP", resolver, true},
		{"Example.Show.S01E01.1080p.WEB-DL.DDP5.1.H.264-GRP", resolver, false},
		{"Example.Show.S01E01-E03.1080p.WEB-DL-GRP", resolver, false},
		{"Example.Show.S01E01E02.1080p.WEB-DL-GRP", nil, false},
		{"Example.Show.S01E01-E05.1080p.WEB-DL-GRP", nil, true},
	} {
		opts := Options{ExpectedTitle: "Example Show", TargetSeason: 1, EpisodeResolver: tc.resolver, SeasonPacksOnly: true}
		results := Results([]models.NZBResult{{Title: tc.title}}, opts)
		if kept := len(results) == 1; kept != tc.keep {
			t.Errorf("%s: kept=%v, want %v", tc.title, kept, tc.keep)
		}
	}
}

func TestSeasonPacksOffKeepsEpisodeReleases(t *testing.T) {
	results := Results([]models.NZBResult{{Title: "Example.Show.S01E01.1080p.WEB-DL-GRP"}}, Options{ExpectedTitle: "Example Show", TargetSeason: 1})
	if len(results) != 1 {
		t.Fatalf("season search without SeasonPacksOnly should keep episode releases")
	}
}

func TestSeasonPacksOnlyIgnoresDefaultFirstEpisodeTarget(t *testing.T) {
	// Season-only queries parse with a default E01 target; pack mode must
	// still reject the E01 single and keep the pack.
	opts := Options{ExpectedTitle: "Example Show", TargetSeason: 1, TargetEpisode: 1, SeasonPacksOnly: true}
	results := Results([]models.NZBResult{
		{Title: "Example.Show.S01E01.1080p.WEB-DL-GRP"},
		{Title: "Example.Show.S01.1080p.WEB-DL-GRP"},
	}, opts)
	if len(results) != 1 || results[0].Title != "Example.Show.S01.1080p.WEB-DL-GRP" {
		t.Fatalf("results = %v, want only the season pack", results)
	}
}
