package history

import (
	"testing"

	"novastream/models"
)

// BBC-era Great British Bake Off metadata (series 1–7) with release absolute
// numbers, as returned when a later series file resolves to the wrong TMDB
// entry or metadata predates a new episode.
func bakeOffSeriesOneToSevenDetails() *models.SeriesDetails {
	details := &models.SeriesDetails{}
	absolute := 0
	for season := 1; season <= 7; season++ {
		s := models.SeriesSeason{Number: season}
		for episode := 1; episode <= 10; episode++ {
			absolute++
			s.Episodes = append(s.Episodes, models.SeriesEpisode{SeasonNumber: season, EpisodeNumber: episode, AbsoluteEpisodeNumber: absolute, Name: "BBC episode"})
		}
		details.Seasons = append(details.Seasons, s)
	}
	return details
}

func TestEnrichEpisodeFromMetadataDoesNotRemapMissingLaterSeasonToEarlierSeason(t *testing.T) {
	ref := &models.EpisodeReference{SeasonNumber: 17, EpisodeNumber: 3, Title: "Audience Choice Week"}
	enrichEpisodeFromMetadata(ref, bakeOffSeriesOneToSevenDetails())
	if ref.SeasonNumber != 17 || ref.EpisodeNumber != 3 || ref.Title != "Audience Choice Week" {
		t.Fatalf("got S%02dE%02d %q, want S17E03 Audience Choice Week", ref.SeasonNumber, ref.EpisodeNumber, ref.Title)
	}
}

func TestEnrichEpisodeFromMetadataStillRemapsLegacyAbsoluteForward(t *testing.T) {
	ref := &models.EpisodeReference{SeasonNumber: 1, EpisodeNumber: 25}
	enrichEpisodeFromMetadata(ref, bakeOffSeriesOneToSevenDetails())
	if ref.SeasonNumber != 3 || ref.EpisodeNumber != 5 {
		t.Fatalf("got S%02dE%02d, want S03E05", ref.SeasonNumber, ref.EpisodeNumber)
	}
}

func TestEpisodeNumberingCanonicalDoesNotRemapToEarlierSeason(t *testing.T) {
	idx := newEpisodeNumberingIndex(bakeOffSeriesOneToSevenDetails())
	if season, episode := idx.canonical(17, 3); season != 17 || episode != 3 {
		t.Fatalf("canonical(17,3) = S%02dE%02d, want unchanged", season, episode)
	}
	if season, episode := idx.canonical(3, 25); season != 3 || episode != 5 {
		t.Fatalf("canonical(3,25) = S%02dE%02d, want S03E05", season, episode)
	}
}
