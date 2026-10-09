package localmedia

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"novastream/models"
)

const gbboFileName = "The Great British Bake Off (2010) - S17E03 - Audience Choice Week [WEBDL-1080p][AAC 2.0][h264]-Kitsune.mkv"

func TestApplyFolderExternalIDsReadsSeriesFolderTag(t *testing.T) {
	path := "/media/tv_shows/The Great British Bake Off (2010) {tvdb-184871}/Season 17/" + gbboFileName
	detected := detectedTitle{title: "The Great British Bake Off", year: 2010, season: 17, episode: 3}
	applyFolderExternalIDs(&detected, path)
	if detected.tvdbID != 184871 || detected.tmdbID != 0 || detected.imdbID != "" {
		t.Fatalf("detected ids = %q/%d/%d, want tvdb 184871 only", detected.imdbID, detected.tmdbID, detected.tvdbID)
	}
	if detected.season != 17 || detected.episode != 3 {
		t.Fatalf("detected S%dE%d, want S17E03", detected.season, detected.episode)
	}
}

func TestApplyFolderExternalIDsKeepsFilenameIDsAndIgnoresLibraryRoot(t *testing.T) {
	detected := detectedTitle{tmdbID: 87012}
	applyFolderExternalIDs(&detected, "/media/Show {tvdb-184871}/Season 1/Show S01E01 {tmdb-87012}.mkv")
	if detected.tmdbID != 87012 || detected.tvdbID != 0 {
		t.Fatalf("filename ids were mixed with folder ids: %+v", detected)
	}

	detected = detectedTitle{}
	applyFolderExternalIDs(&detected, "/media/tvdb-99/Show/Season 1/Show S01E01.mkv")
	if detected.tvdbID != 0 {
		t.Fatalf("read tag above the series folder: %+v", detected)
	}
}

type gbboMetadata struct{ searchCalls, seriesCalls int }

func (m *gbboMetadata) Search(ctx context.Context, query, mediaType string) ([]models.SearchResult, error) {
	m.searchCalls++
	return []models.SearchResult{{Score: 100, Title: models.Title{ID: "tmdb:tv:34549", MediaType: "series", Name: "The Great British Bake Off", Year: 2010, TMDBID: 34549}}}, nil
}

func (m *gbboMetadata) MovieDetails(ctx context.Context, query models.MovieDetailsQuery) (*models.Title, error) {
	return nil, nil
}

func (m *gbboMetadata) SeriesDetails(ctx context.Context, query models.SeriesDetailsQuery) (*models.SeriesDetails, error) {
	m.seriesCalls++
	if query.TVDBID == 184871 {
		return &models.SeriesDetails{Title: models.Title{ID: "tvdb:series:184871", MediaType: "series", Name: "The Great British Bake Off", Year: 2010, TVDBID: 184871}}, nil
	}
	return &models.SeriesDetails{Title: models.Title{ID: "tmdb:tv:34549", MediaType: "series", Name: "The Great British Bake Off", Year: 2010, TMDBID: 34549}}, nil
}

func TestScanMatchesSeriesFolderTagAndRematchesReusedTitleSearchItems(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "The Great British Bake Off (2010) {tvdb-184871}", "Season 17")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := []string{gbboFileName, "The Great British Bake Off (2010) - S17E02 - Biscuit Week [WEBDL-1080p]-Kitsune.mkv"}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(filepath.Join(dir, files[0]))
	if err != nil {
		t.Fatal(err)
	}
	modifiedAt := info.ModTime().UTC()
	relativePath := filepath.Join("The Great British Bake Off (2010) {tvdb-184871}", "Season 17", files[0])

	repo := &fakeLocalMediaRepo{
		library: &models.LocalMediaLibrary{ID: "lib1", Name: "TV", Type: models.LocalMediaLibraryTypeShow, RootPath: root, CreatedAt: time.Now(), UpdatedAt: time.Now()},
		items: map[string]*models.LocalMediaItem{
			// Matched by title search before folder tags were read.
			relativePath: {
				ID: "existing", LibraryID: "lib1", RelativePath: relativePath, FilePath: filepath.Join(dir, files[0]),
				LibraryType: models.LocalMediaLibraryTypeShow, SeasonNumber: 17, EpisodeNumber: 3,
				MatchStatus: models.LocalMediaMatchStatusMatched, MatchedTitleID: "tmdb:tv:34549", MatchedMediaType: "series",
				SizeBytes: info.Size(), ModifiedAt: &modifiedAt, Probe: &models.LocalMediaProbe{},
			},
		},
	}
	metadata := &gbboMetadata{}
	service := &Service{repo: repo, metadata: metadata, ffprobePath: "ffprobe", scans: make(map[string]scanState)}

	if _, err := service.RunScan(context.Background(), "lib1"); err != nil {
		t.Fatalf("RunScan: %v", err)
	}
	for _, item := range repo.items {
		if item.MatchedTitleID != "tvdb:series:184871" || item.SeasonNumber != 17 {
			t.Fatalf("%s matched %q S%d, want tvdb:series:184871 S17", item.FileName, item.MatchedTitleID, item.SeasonNumber)
		}
	}
	if repo.items[relativePath].ID != "existing" {
		t.Fatal("reused item lost its identity")
	}
	if metadata.searchCalls != 0 {
		t.Fatalf("title search ran %d times despite folder id tag", metadata.searchCalls)
	}
}

func TestScanKeepsManualMatchOverFolderTag(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Show {tvdb-184871}", "Season 1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "Show - S01E01.mkv")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	modifiedAt := info.ModTime().UTC()
	relativePath := filepath.Join("Show {tvdb-184871}", "Season 1", "Show - S01E01.mkv")
	repo := &fakeLocalMediaRepo{
		library: &models.LocalMediaLibrary{ID: "lib1", Type: models.LocalMediaLibraryTypeShow, RootPath: root},
		items: map[string]*models.LocalMediaItem{relativePath: {
			ID: "manual", LibraryID: "lib1", RelativePath: relativePath, FilePath: path, LibraryType: models.LocalMediaLibraryTypeShow,
			MatchStatus: models.LocalMediaMatchStatusManual, MatchedTitleID: "tmdb:tv:1", SizeBytes: info.Size(), ModifiedAt: &modifiedAt,
		}},
	}
	service := &Service{repo: repo, metadata: &gbboMetadata{}, ffprobePath: "ffprobe", scans: make(map[string]scanState)}
	if _, err := service.RunScan(context.Background(), "lib1"); err != nil {
		t.Fatalf("RunScan: %v", err)
	}
	if got := repo.items[relativePath].MatchedTitleID; got != "tmdb:tv:1" {
		t.Fatalf("manual match replaced with %q", got)
	}
}
