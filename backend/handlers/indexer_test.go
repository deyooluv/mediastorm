package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"novastream/models"
	"novastream/services/indexer"
	"novastream/utils/filter"
)

func TestNormalizeDecoratedSeriesQuery(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		mediaType string
		want      string
	}{
		{
			name:      "continue watching display title",
			query:     "Legion • S02E01 – Chapter 9 S02E02",
			mediaType: "series",
			want:      "Legion S02E02",
		},
		{
			name:      "ordinary series query",
			query:     "Legion S02E02 2160p",
			mediaType: "series",
			want:      "Legion S02E02 2160p",
		},
		{
			name:      "movie title remains untouched",
			query:     "Movie • S02E01 S02E02",
			mediaType: "movie",
			want:      "Movie • S02E01 S02E02",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeDecoratedSeriesQuery(tt.query, tt.mediaType); got != tt.want {
				t.Fatalf("normalizeDecoratedSeriesQuery(%q, %q) = %q, want %q", tt.query, tt.mediaType, got, tt.want)
			}
		})
	}
}

type fakeIndexerService struct {
	results  []models.NZBResult
	err      error
	lastOpts indexer.SearchOptions
}

type fakeMovieMetadataService struct {
	title *models.Title
	err   error
}

type fakeSeriesMetadataService struct {
	details *models.SeriesDetails
	err     error
}

func (f *fakeSeriesMetadataService) SeriesDetails(_ context.Context, _ models.SeriesDetailsQuery) (*models.SeriesDetails, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.details, nil
}

func (f *fakeMovieMetadataService) MovieInfo(_ context.Context, _ models.MovieDetailsQuery) (*models.Title, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.title, nil
}

func (f *fakeIndexerService) Search(_ context.Context, opts indexer.SearchOptions) ([]models.NZBResult, error) {
	f.lastOpts = opts
	if f.err != nil {
		return nil, f.err
	}
	return f.results, nil
}

func (f *fakeIndexerService) SearchTest(_ context.Context, opts indexer.SearchOptions) ([]models.ScoredNZBResult, error) {
	f.lastOpts = opts
	if f.err != nil {
		return nil, f.err
	}
	scored := make([]models.ScoredNZBResult, len(f.results))
	for i, r := range f.results {
		scored[i] = models.ScoredNZBResult{NZBResult: r, FilterStatus: "passed"}
	}
	return scored, nil
}

func (f *fakeIndexerService) SearchWithScoring(_ context.Context, opts indexer.SearchOptions) ([]models.ScoredNZBResult, error) {
	f.lastOpts = opts
	if f.err != nil {
		return nil, f.err
	}
	scored := make([]models.ScoredNZBResult, len(f.results))
	for i, r := range f.results {
		scored[i] = models.ScoredNZBResult{NZBResult: r, FilterStatus: "passed"}
	}
	return scored, nil
}

func TestIndexerHandler_Search(t *testing.T) {
	fake := &fakeIndexerService{
		results: []models.NZBResult{{Title: "The Expanse", Indexer: "nzbPlanet", SizeBytes: 1234}},
	}
	handler := NewIndexerHandler(fake, false)

	req := httptest.NewRequest(http.MethodGet, "/api/indexers/search?q=The+Expanse&limit=2&cat=5000&cat=5040", nil)
	rec := httptest.NewRecorder()

	handler.Search(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, rec.Code)
	}
	if fake.lastOpts.Query != "The Expanse" {
		t.Fatalf("unexpected query captured: %q", fake.lastOpts.Query)
	}
	if fake.lastOpts.MaxResults != 2 {
		t.Fatalf("expected limit 2, got %d", fake.lastOpts.MaxResults)
	}
	if len(fake.lastOpts.Categories) != 2 {
		t.Fatalf("expected categories to pass through, got %+v", fake.lastOpts.Categories)
	}

	var payload []models.NZBResult
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(payload) != 1 || payload[0].Title != "The Expanse" {
		t.Fatalf("unexpected payload: %+v", payload)
	}
}

func TestIndexerHandler_SearchDownloadRanking(t *testing.T) {
	fake := &fakeIndexerService{
		results: []models.NZBResult{{Title: "The Expanse", Indexer: "nzbPlanet", SizeBytes: 1234}},
	}
	handler := NewIndexerHandler(fake, false)

	req := httptest.NewRequest(http.MethodGet, "/api/indexers/search?q=The+Expanse&downloadRanking=true", nil)
	rec := httptest.NewRecorder()

	handler.Search(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, rec.Code)
	}
	if !fake.lastOpts.UseDownloadRanking {
		t.Fatal("expected UseDownloadRanking=true to be forwarded to search service")
	}
}

func TestIndexerHandler_SearchSeasonPacks(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  bool
	}{
		{"q=The+Expanse+S01&mediaType=series&seasonPacks=true", true},
		{"q=The+Expanse+S01&mediaType=series", false},
		{"q=The+Expanse&mediaType=movie&seasonPacks=true", false},
	} {
		fake := &fakeIndexerService{results: []models.NZBResult{}}
		handler := NewIndexerHandler(fake, false)
		rec := httptest.NewRecorder()
		handler.Search(rec, httptest.NewRequest(http.MethodGet, "/api/indexers/search?"+tc.query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: expected %d, got %d", tc.query, http.StatusOK, rec.Code)
		}
		if fake.lastOpts.SeasonPacksOnly != tc.want {
			t.Fatalf("%s: SeasonPacksOnly = %v, want %v", tc.query, fake.lastOpts.SeasonPacksOnly, tc.want)
		}
	}
}

func TestIndexerHandler_SearchDefaultLimit(t *testing.T) {
	fake := &fakeIndexerService{results: []models.NZBResult{}}
	handler := NewIndexerHandler(fake, false)

	req := httptest.NewRequest(http.MethodGet, "/api/indexers/search?q=expanse&limit=invalid", nil)
	rec := httptest.NewRecorder()

	handler.Search(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, rec.Code)
	}
	if fake.lastOpts.MaxResults != 5 {
		t.Fatalf("expected default limit 5, got %d", fake.lastOpts.MaxResults)
	}
}

func TestIndexerHandler_SearchError(t *testing.T) {
	fake := &fakeIndexerService{err: errors.New("indexer down")}
	handler := NewIndexerHandler(fake, false)

	req := httptest.NewRequest(http.MethodGet, "/api/indexers/search?q=expanse", nil)
	rec := httptest.NewRecorder()

	handler.Search(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected %d, got %d", http.StatusBadGateway, rec.Code)
	}

	var payload map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload["error"] == "" {
		t.Fatalf("expected error message, got %v", payload)
	}
}

func TestIndexerHandler_SearchMovieAnimeDetection(t *testing.T) {
	fake := &fakeIndexerService{results: []models.NZBResult{}}
	movieSvc := &fakeMovieMetadataService{
		title: &models.Title{
			Genres:       []string{"Animation", "Fantasy"},
			OriginalName: "千と千尋の神隠し",
		},
	}
	handler := NewIndexerHandler(fake, false)
	handler.SetMovieMetadataService(movieSvc)

	req := httptest.NewRequest(http.MethodGet, "/api/indexers/search?q=Spirited+Away&mediaType=movie&year=2001", nil)
	rec := httptest.NewRecorder()

	handler.Search(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, rec.Code)
	}
	if !fake.lastOpts.IsAnime {
		t.Fatal("expected IsAnime=true for anime movie, got false")
	}
}

func TestIndexerHandler_SearchPassesHydratedMovieTitles(t *testing.T) {
	fake := &fakeIndexerService{results: []models.NZBResult{}}
	movieSvc := &fakeMovieMetadataService{
		title: &models.Title{
			Name:            "DC Showcase - Batman: Death in the Family",
			AlternateTitles: []string{"Batman: Death in the Family"},
		},
	}
	handler := NewIndexerHandler(fake, false)
	handler.SetMovieMetadataService(movieSvc)

	req := httptest.NewRequest(http.MethodGet, "/api/indexers/search?q=Batman%3A+Death+in+the+Family&mediaType=movie&year=2020", nil)
	rec := httptest.NewRecorder()
	handler.Search(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, rec.Code)
	}
	want := []string{"DC Showcase - Batman: Death in the Family", "Batman: Death in the Family"}
	if len(fake.lastOpts.AlternateTitles) != len(want) {
		t.Fatalf("alternate titles = %v, want %v", fake.lastOpts.AlternateTitles, want)
	}
	for i := range want {
		if fake.lastOpts.AlternateTitles[i] != want[i] {
			t.Fatalf("alternate titles = %v, want %v", fake.lastOpts.AlternateTitles, want)
		}
	}
}

func TestIndexerHandler_SearchMovieNonAnime(t *testing.T) {
	fake := &fakeIndexerService{results: []models.NZBResult{}}
	movieSvc := &fakeMovieMetadataService{
		title: &models.Title{Genres: []string{"Animation", "Family"}},
	}
	handler := NewIndexerHandler(fake, false)
	handler.SetMovieMetadataService(movieSvc)

	req := httptest.NewRequest(http.MethodGet, "/api/indexers/search?q=John+Wick&mediaType=movie&year=2014", nil)
	rec := httptest.NewRecorder()

	handler.Search(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, rec.Code)
	}
	if fake.lastOpts.IsAnime {
		t.Fatal("expected IsAnime=false for non-anime movie, got true")
	}
}

func TestIndexerHandler_SearchSeriesAbsoluteEpisode(t *testing.T) {
	fake := &fakeIndexerService{results: []models.NZBResult{}}
	seriesSvc := &fakeSeriesMetadataService{
		details: &models.SeriesDetails{
			Title: models.Title{
				Name:   "One Piece",
				Year:   1999,
				Genres: []string{"Anime"},
			},
			Seasons: []models.SeriesSeason{
				{
					Number: 23,
					Episodes: []models.SeriesEpisode{
						{SeasonNumber: 23, EpisodeNumber: 6, AbsoluteEpisodeNumber: 1161, AiredDate: "2026-05-10"},
					},
				},
			},
		},
	}
	handler := NewIndexerHandler(fake, false)
	handler.SetMetadataService(seriesSvc)

	req := httptest.NewRequest(http.MethodGet, "/api/indexers/search?q=One+Piece+S23E06&mediaType=series&year=1999", nil)
	rec := httptest.NewRecorder()

	handler.Search(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, rec.Code)
	}
	if !fake.lastOpts.IsAnime {
		t.Fatal("expected IsAnime=true for anime series")
	}
	if fake.lastOpts.AbsoluteEpisodeNumber != 1161 {
		t.Fatalf("expected absolute episode 1161, got %d", fake.lastOpts.AbsoluteEpisodeNumber)
	}
	if fake.lastOpts.EpisodeAirYear != 2026 {
		t.Fatalf("expected episode air year 2026, got %d", fake.lastOpts.EpisodeAirYear)
	}
	if !fake.lastOpts.EpisodeReleased {
		t.Fatal("expected episode to be marked released")
	}
}

func TestIndexerHandler_SearchNonAnimeUsesReleaseAbsoluteEpisode(t *testing.T) {
	fake := &fakeIndexerService{results: []models.NZBResult{}}
	seriesSvc := &fakeSeriesMetadataService{
		details: &models.SeriesDetails{
			Title: models.Title{
				Name:   "Non-Anime Series",
				Year:   1997,
				Genres: []string{"Action", "Science Fiction"},
			},
			Seasons: []models.SeriesSeason{
				{
					Number:       0,
					EpisodeCount: 1,
					Episodes: []models.SeriesEpisode{
						{SeasonNumber: 0, EpisodeNumber: 1, AbsoluteEpisodeNumber: 13},
					},
				},
				{Number: 1, EpisodeCount: 12},
				{
					Number:       2,
					EpisodeCount: 11,
					Episodes: []models.SeriesEpisode{
						{SeasonNumber: 2, EpisodeNumber: 1, AbsoluteEpisodeNumber: 14},
					},
				},
			},
		},
	}
	handler := NewIndexerHandler(fake, false)
	handler.SetMetadataService(seriesSvc)

	req := httptest.NewRequest(http.MethodGet, "/api/indexers/search?q=Non-Anime+Series+S02E01&mediaType=series&year=1997", nil)
	rec := httptest.NewRecorder()
	handler.Search(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, rec.Code)
	}
	if fake.lastOpts.IsAnime {
		t.Fatal("expected series to remain non-anime")
	}
	if fake.lastOpts.AbsoluteEpisodeNumber != 13 {
		t.Fatalf("AbsoluteEpisodeNumber = %d, want release absolute 13", fake.lastOpts.AbsoluteEpisodeNumber)
	}
}

func TestIndexerHandler_SearchSoapUsesEpisodeAirDate(t *testing.T) {
	fake := &fakeIndexerService{results: []models.NZBResult{}}
	seriesSvc := &fakeSeriesMetadataService{
		details: &models.SeriesDetails{
			Title: models.Title{
				Name:    "Coronation Street",
				Year:    1960,
				Genres:  []string{"Soap", "Drama"},
				IsDaily: true,
			},
			Seasons: []models.SeriesSeason{{
				Number: 67,
				Episodes: []models.SeriesEpisode{{
					SeasonNumber:  67,
					EpisodeNumber: 151,
					AiredDate:     "2026-08-18",
				}},
			}},
		},
	}
	handler := NewIndexerHandler(fake, false)
	handler.SetMetadataService(seriesSvc)

	req := httptest.NewRequest(http.MethodGet, "/api/indexers/search?q=Coronation+Street+S67E151&mediaType=series&year=1960", nil)
	rec := httptest.NewRecorder()
	handler.Search(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, rec.Code)
	}
	if !fake.lastOpts.IsDaily {
		t.Fatal("expected soap to enable date-based matching")
	}
	if fake.lastOpts.TargetAirDate != "2026-08-18" {
		t.Fatalf("TargetAirDate = %q, want 2026-08-18", fake.lastOpts.TargetAirDate)
	}
}

func TestIndexerHandler_SearchNadesicoUsesPreservedAnimeMetadata(t *testing.T) {
	fake := &fakeIndexerService{results: []models.NZBResult{}}
	seriesSvc := &fakeSeriesMetadataService{
		details: &models.SeriesDetails{
			Title: models.Title{
				Name:         "Martian Successor Nadesico",
				OriginalName: "機動戦艦ナデシコ",
				Language:     "eng",
				Year:         1996,
				Genres:       []string{"Animation", "Comedy", "Anime"},
			},
			Seasons: []models.SeriesSeason{
				{
					Number: 1,
					Episodes: []models.SeriesEpisode{
						{SeasonNumber: 1, EpisodeNumber: 1, AbsoluteEpisodeNumber: 1, AiredDate: "1996-10-01"},
					},
				},
			},
		},
	}
	handler := NewIndexerHandler(fake, false)
	handler.SetMetadataService(seriesSvc)

	req := httptest.NewRequest(http.MethodGet, "/api/indexers/search?q=Martian+Successor+Nadesico+S01E01&mediaType=series&year=1996", nil)
	rec := httptest.NewRecorder()

	handler.Search(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, rec.Code)
	}
	if !fake.lastOpts.IsAnime {
		t.Fatal("expected preserved TVDB Anime genre to classify Nadesico as anime")
	}
	if fake.lastOpts.AbsoluteEpisodeNumber != 1 {
		t.Fatalf("expected absolute episode 1, got %d", fake.lastOpts.AbsoluteEpisodeNumber)
	}
}

func TestIndexerHandler_SearchSeriesInfersMissingAbsoluteEpisode(t *testing.T) {
	fake := &fakeIndexerService{results: []models.NZBResult{}}
	seriesSvc := &fakeSeriesMetadataService{
		details: &models.SeriesDetails{
			Title: models.Title{
				Name:   "One Piece",
				Year:   1999,
				Genres: []string{"Anime"},
			},
			Seasons: []models.SeriesSeason{
				{
					Number: 23,
					Episodes: []models.SeriesEpisode{
						{SeasonNumber: 23, EpisodeNumber: 16, AbsoluteEpisodeNumber: 1171},
						{SeasonNumber: 23, EpisodeNumber: 17, AiredDate: "2026-08-02"},
					},
				},
			},
		},
	}
	handler := NewIndexerHandler(fake, false)
	handler.SetMetadataService(seriesSvc)

	req := httptest.NewRequest(http.MethodGet, "/api/indexers/search?q=One+Piece+S23E17&mediaType=series&year=1999", nil)
	rec := httptest.NewRecorder()

	handler.Search(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, rec.Code)
	}
	if fake.lastOpts.AbsoluteEpisodeNumber != 1172 {
		t.Fatalf("expected inferred absolute episode 1172, got %d", fake.lastOpts.AbsoluteEpisodeNumber)
	}

	filterOpts := filter.Options{
		ExpectedTitle:         "One Piece",
		TargetSeason:          23,
		TargetEpisode:         17,
		TargetAbsoluteEpisode: fake.lastOpts.AbsoluteEpisodeNumber,
		IsAnime:               fake.lastOpts.IsAnime,
	}
	results := filter.Results([]models.NZBResult{{Title: "One Piece S01E1172 1080p WEB-DL"}}, filterOpts)
	if len(results) != 1 {
		t.Fatal("expected S01E1172 result to pass filtering for inferred absolute episode 1172")
	}
	wrongResults := filter.Results([]models.NZBResult{{Title: "One Piece S01E1171 1080p WEB-DL"}}, filterOpts)
	if len(wrongResults) != 0 {
		t.Fatal("expected adjacent S01E1171 result to be rejected for absolute episode 1172")
	}
}

func TestIndexerHandler_SearchSeriesUsesReleaseAbsoluteEpisode(t *testing.T) {
	fake := &fakeIndexerService{results: []models.NZBResult{}}
	seriesSvc := &fakeSeriesMetadataService{
		details: &models.SeriesDetails{
			Title: models.Title{Name: "Kaiju No. 8", Year: 2024, Genres: []string{"Anime"}},
			Seasons: []models.SeriesSeason{
				{
					Number:       0,
					EpisodeCount: 1,
					Episodes: []models.SeriesEpisode{
						{Name: "Hoshina's Day Off", SeasonNumber: 0, EpisodeNumber: 1, AbsoluteEpisodeNumber: 13},
					},
				},
				{Number: 1, EpisodeCount: 12},
				{
					Number:       2,
					EpisodeCount: 11,
					Episodes: []models.SeriesEpisode{
						{Name: "Kaiju Weapon", SeasonNumber: 2, EpisodeNumber: 1, AbsoluteEpisodeNumber: 14},
					},
				},
			},
		},
	}
	handler := NewIndexerHandler(fake, false)
	handler.SetMetadataService(seriesSvc)

	req := httptest.NewRequest(http.MethodGet, "/api/indexers/search?q=Kaiju+No.+8+S02E01&mediaType=series&year=2024", nil)
	rec := httptest.NewRecorder()
	handler.Search(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, rec.Code)
	}
	if fake.lastOpts.AbsoluteEpisodeNumber != 13 {
		t.Fatalf("AbsoluteEpisodeNumber = %d, want release-style 13", fake.lastOpts.AbsoluteEpisodeNumber)
	}
}

func TestIndexerHandler_SearchTest(t *testing.T) {
	fake := &fakeIndexerService{
		results: []models.NZBResult{
			{Title: "The Expanse S01E01", Indexer: "nzbPlanet", SizeBytes: 1234},
		},
	}
	handler := NewIndexerHandler(fake, false)

	req := httptest.NewRequest(http.MethodGet, "/api/indexers/search-test?q=The+Expanse+S01E01&mediaType=series&limit=50", nil)
	rec := httptest.NewRecorder()

	handler.SearchTest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, rec.Code)
	}

	var payload []models.ScoredNZBResult
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(payload) != 1 {
		t.Fatalf("expected 1 result, got %d", len(payload))
	}
	if payload[0].FilterStatus != "passed" {
		t.Fatalf("expected filterStatus=passed, got %q", payload[0].FilterStatus)
	}
	if payload[0].Title != "The Expanse S01E01" {
		t.Fatalf("expected title 'The Expanse S01E01', got %q", payload[0].Title)
	}
}

func TestIndexerHandler_SearchTestPassesHydratedMovieTitles(t *testing.T) {
	fake := &fakeIndexerService{results: []models.NZBResult{}}
	movieSvc := &fakeMovieMetadataService{
		title: &models.Title{
			Name:            "DC Showcase - Batman: Death in the Family",
			AlternateTitles: []string{"Batman: Death in the Family"},
		},
	}
	handler := NewIndexerHandler(fake, false)
	handler.SetMovieMetadataService(movieSvc)

	req := httptest.NewRequest(http.MethodGet, "/api/indexers/search-test?q=Batman%3A+Death+in+the+Family&mediaType=movie&year=2020", nil)
	rec := httptest.NewRecorder()
	handler.SearchTest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, rec.Code)
	}
	want := []string{"DC Showcase - Batman: Death in the Family", "Batman: Death in the Family"}
	if !reflect.DeepEqual(fake.lastOpts.AlternateTitles, want) {
		t.Fatalf("alternate titles = %v, want %v", fake.lastOpts.AlternateTitles, want)
	}
}

func TestIndexerHandler_SearchTestDownloadRanking(t *testing.T) {
	fake := &fakeIndexerService{
		results: []models.NZBResult{
			{Title: "The Expanse S01E01", Indexer: "nzbPlanet", SizeBytes: 1234},
		},
	}
	handler := NewIndexerHandler(fake, false)

	req := httptest.NewRequest(http.MethodGet, "/api/indexers/search-test?q=The+Expanse+S01E01&mediaType=series&downloadRanking=true", nil)
	rec := httptest.NewRecorder()

	handler.SearchTest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, rec.Code)
	}
	if !fake.lastOpts.UseDownloadRanking {
		t.Fatal("expected UseDownloadRanking=true to be forwarded to indexer service")
	}
}

func TestIndexerHandler_SearchTestError(t *testing.T) {
	fake := &fakeIndexerService{err: errors.New("indexer down")}
	handler := NewIndexerHandler(fake, false)

	req := httptest.NewRequest(http.MethodGet, "/api/indexers/search-test?q=test", nil)
	rec := httptest.NewRecorder()

	handler.SearchTest(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected %d, got %d", http.StatusBadGateway, rec.Code)
	}
}

func TestIndexerHandler_SearchIncludeFiltered(t *testing.T) {
	fake := &fakeIndexerService{
		results: []models.NZBResult{
			{Title: "Movie 2024", Indexer: "test"},
		},
	}
	handler := NewIndexerHandler(fake, false)

	req := httptest.NewRequest(http.MethodGet, "/api/indexers/search?q=Movie&includeFiltered=true", nil)
	rec := httptest.NewRecorder()

	handler.Search(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, rec.Code)
	}

	var payload []models.ScoredNZBResult
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(payload) != 1 {
		t.Fatalf("expected 1 result, got %d", len(payload))
	}
	if payload[0].FilterStatus != "passed" {
		t.Fatalf("expected filterStatus=passed, got %q", payload[0].FilterStatus)
	}
}
