package mediaidentity

import "strings"

// SeriesSeasonOffset maps a TMDB entry that restarts season numbering after a
// channel move or revival onto the continuous series that TVDB, IMDb and
// release groups number. Episode numbers within a season are unchanged.
//
// These are release aliases only. They never enter KnownAnthologyEpisode, so
// scrobbles and history keep catalog numbering, and they apply only while the
// episodes are numbered by the catalog's own TMDB entry: TVDB-numbered
// requests for the same title already use the continuous seasons.
type SeriesSeasonOffset struct {
	TitleID            string
	FirstCatalogSeason int
	LastCatalogSeason  int // 0 while the show is still running.
	ProviderSeason     int // Provider season of FirstCatalogSeason.
	IMDBID             string
	TVDBID             int64
}

// Verified 2026-10-09 against TheTVDB official order (allseasons pages),
// TMDB season episode lists and Cinemeta: episode counts, titles and air dates
// match season for season. IMDb IDs are set only where IMDb uses the same
// continuous numbering; elsewhere IMDb lists the revival as its own series.
var seriesSeasonOffsets = []SeriesSeasonOffset{
	// Great British Bake Off, Channel 4 era: TMDB S1 (2017) is TVDB/IMDb S8.
	{TitleID: "tmdb:tv:87012", FirstCatalogSeason: 1, ProviderSeason: 8, IMDBID: "tt1877368", TVDBID: 184871},
	// Top Boy, Netflix 2019: TMDB S1–3 are TVDB/IMDb S3–5.
	{TitleID: "tmdb:tv:93544", FirstCatalogSeason: 1, LastCatalogSeason: 3, ProviderSeason: 3, IMDBID: "tt1830379", TVDBID: 253138},
	// Kitchen Nightmares, 2023 revival: TMDB S1 is TVDB S7.
	{TitleID: "tmdb:tv:235884", FirstCatalogSeason: 1, ProviderSeason: 7, TVDBID: 80552},
	// Robot Wars, 2016 revival: TMDB S1–3 are TVDB S8–10.
	{TitleID: "tmdb:tv:67208", FirstCatalogSeason: 1, LastCatalogSeason: 3, ProviderSeason: 8, TVDBID: 71146},
	// Fear Factor, 2017 revival: TMDB S1–2 are TVDB S8–9.
	{TitleID: "tmdb:tv:72095", FirstCatalogSeason: 1, LastCatalogSeason: 2, ProviderSeason: 8, TVDBID: 76707},
	// Mystery Science Theater 3000, Netflix: TMDB S1–2 are TVDB S11–12.
	{TitleID: "tmdb:tv:75907", FirstCatalogSeason: 1, LastCatalogSeason: 2, ProviderSeason: 11, TVDBID: 74806},
	// Mystery Science Theater 3000, Gizmoplex 2022: TMDB S1 is TVDB S13.
	{TitleID: "tmdb:tv:203187", FirstCatalogSeason: 1, LastCatalogSeason: 1, ProviderSeason: 13, TVDBID: 74806},
}

func seasonOffsetAlias(offsets []SeriesSeasonOffset, titleID string, season, episode int) (AnthologyEpisode, bool) {
	titleID = strings.TrimSpace(titleID)
	for _, o := range offsets {
		if titleID != o.TitleID || season < o.FirstCatalogSeason || episode < 1 || (o.LastCatalogSeason > 0 && season > o.LastCatalogSeason) {
			continue
		}
		return AnthologyEpisode{
			Source:  "season-offset",
			IMDBID:  o.IMDBID,
			TVDBID:  o.TVDBID,
			Season:  o.ProviderSeason + season - o.FirstCatalogSeason,
			Episode: episode,
		}, true
	}
	return AnthologyEpisode{}, false
}
