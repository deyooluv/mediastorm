package filter

import (
	"testing"

	"novastream/models"
)

func TestBakeOffChannel4AcceptsContinuousSeasonReleases(t *testing.T) {
	numbering := &models.EpisodeNumbering{SeriesID: "tmdb:tv:87012", Ordering: "official"}
	for _, tc := range []struct {
		name, release string
		keep          bool
	}{
		{"continuous numbering", "The.Great.British.Bake.Off.S17E03.1080p.WEB.h264-KITSUNE", true},
		{"catalog numbering", "The.Great.British.Bake.Off.S10E03.1080p.WEB.h264-KITSUNE", true},
		{"continuous season pack", "The.Great.British.Bake.Off.S17.1080p.WEB.h264", true},
		{"wrong episode", "The.Great.British.Bake.Off.S17E04.1080p.WEB.h264-KITSUNE", false},
		{"BBC era", "The.Great.British.Bake.Off.S01E03.1080p.WEB.h264", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := Options{TitleID: "tmdb:tv:87012", Numbering: numbering, ExpectedTitle: "The Great British Bake Off", ExpectedYear: 2017, TargetSeason: 10, TargetEpisode: 3}
			results := Results([]models.NZBResult{{Title: tc.release}}, opts)
			if (len(results) == 1) != tc.keep {
				t.Fatalf("kept %d results, want keep=%v", len(results), tc.keep)
			}
			if tc.keep && tc.name != "catalog numbering" && results[0].Attributes["targetSeason"] != "17" {
				t.Fatalf("file-selection hints must use season 17: %v", results[0].Attributes)
			}
		})
	}
}
