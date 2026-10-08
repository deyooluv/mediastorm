package handlers

import (
	"testing"

	"novastream/models"
)

func TestPrequeueCandidateAbsoluteEpisodeMismatch(t *testing.T) {
	target := &models.EpisodeReference{SeasonNumber: 7, EpisodeNumber: 9, AbsoluteEpisodeNumber: 81}

	tests := []struct {
		name         string
		title        string
		episodeCount int
		target       *models.EpisodeReference
		wantMismatch bool
	}{
		{
			name:   "Portuguese T07·E09 label for the target episode",
			title:  "🎬 Aeroporto Area Restrita\n🥏 T07·E09\n🌊 Azure\n🌎 Português",
			target: target,
		},
		{
			name:   "Portuguese label with year for the target episode",
			title:  "🎬 Aeroporto: Área Restrita 2017 (2017)\n🥏 T07·E09\n🌊 Carbon\n🌎 Português",
			target: target,
		},
		{
			name:         "Portuguese label for another episode",
			title:        "🎬 Aeroporto Area Restrita\n🥏 T07·E10\n🌊 Azure\n🌎 Português",
			target:       target,
			wantMismatch: true,
		},
		{
			name:   "standard SxxExx label",
			title:  "Airport.Brazil.Restricted.Areas.S07E09.1080p.WEB-DL",
			target: target,
		},
		{
			name:   "anime absolute label matching the absolute number",
			title:  "[SubsPlease] One Piece - 1162 (1080p) [ABCD1234].mkv",
			target: &models.EpisodeReference{SeasonNumber: 23, EpisodeNumber: 7, AbsoluteEpisodeNumber: 1162},
		},
		{
			name:         "anime absolute label for another episode",
			title:        "[SubsPlease] One Piece - 1163 (1080p) [ABCD1234].mkv",
			target:       &models.EpisodeReference{SeasonNumber: 23, EpisodeNumber: 7, AbsoluteEpisodeNumber: 1162},
			wantMismatch: true,
		},
		{
			name:         "season pack is not checked",
			title:        "🎬 Aeroporto Area Restrita\n🥏 T07·E10",
			episodeCount: 12,
			target:       target,
		},
		{
			name:   "no absolute target",
			title:  "🎬 Aeroporto Area Restrita\n🥏 T07·E10",
			target: &models.EpisodeReference{SeasonNumber: 7, EpisodeNumber: 9},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := models.NZBResult{Title: tt.title, EpisodeCount: tt.episodeCount}
			if _, got := prequeueCandidateAbsoluteEpisodeMismatch(result, tt.target); got != tt.wantMismatch {
				t.Fatalf("prequeueCandidateAbsoluteEpisodeMismatch() mismatch = %v, want %v", got, tt.wantMismatch)
			}
		})
	}
}
