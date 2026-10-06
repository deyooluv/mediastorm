package playback

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"novastream/config"
	"novastream/models"
)

const seasonPackNZB = `<?xml version="1.0"?><nzb><file subject="Show.S01.1080p.WEB-DL-GRP"><segments><segment bytes="123456">abc</segment></segments></file></nzb>`

// newSeasonPackEngine serves an external engine whose WebDAV already holds a
// completed season pack with E01 (largest), E02 and E03.
func newSeasonPackEngine(t *testing.T, engineType, apiPath, releaseDir string) (*Service, *httptest.Server, string) {
	t.Helper()
	files := map[string]string{
		"Show.S01E01.1080p.WEB-DL-GRP.mkv": "3000000000",
		"Show.S01E02.1080p.WEB-DL-GRP.mkv": "1000000000",
		"Show.S01E03.1080p.WEB-DL-GRP.mkv": "1000000000",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == apiPath:
			t.Errorf("engine API should not be called for an existing release (mode=%q)", r.URL.Query().Get("mode"))
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == "HEAD" && strings.HasPrefix(r.URL.Path, releaseDir) && files[strings.TrimPrefix(r.URL.Path, releaseDir)] != "":
			w.WriteHeader(http.StatusOK)
		case r.Method == "HEAD":
			w.WriteHeader(http.StatusNotFound)
		case r.Method == "PROPFIND" && r.URL.Path == releaseDir:
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusMultiStatus)
			var b strings.Builder
			b.WriteString(`<?xml version="1.0" encoding="utf-8"?><D:multistatus xmlns:D="DAV:">`)
			b.WriteString(`<D:response><D:href>` + releaseDir + `</D:href><D:propstat><D:status>HTTP/1.1 200 OK</D:status><D:prop><D:resourcetype><D:collection/></D:resourcetype></D:prop></D:propstat></D:response>`)
			for name, size := range files {
				b.WriteString(`<D:response><D:href>` + releaseDir + name + `</D:href><D:propstat><D:status>HTTP/1.1 200 OK</D:status><D:prop><D:displayname>` + name + `</D:displayname><D:getcontentlength>` + size + `</D:getcontentlength></D:prop></D:propstat></D:response>`)
			}
			b.WriteString(`</D:multistatus>`)
			_, _ = io.WriteString(w, b.String())
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	indexer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="Show.S01.1080p.WEB-DL-GRP.nzb"`)
		_, _ = io.WriteString(w, seasonPackNZB)
	}))
	t.Cleanup(indexer.Close)

	settings := config.DefaultSettings()
	settings.UsenetEngines = []config.UsenetEngineSettings{{
		Name:          engineType,
		Type:          engineType,
		Enabled:       true,
		BaseURL:       server.URL,
		APIPath:       apiPath,
		Category:      "mediastorm",
		WebDAVBaseURL: server.URL + "/webdav",
	}}
	cfg := config.NewManager(filepath.Join(t.TempDir(), "settings.json"))
	if err := cfg.Save(settings); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	return NewService(cfg, nil, nil), server, indexer.URL + "/Show.S01.1080p.WEB-DL-GRP.nzb"
}

func seasonPackCandidate(downloadURL string) models.NZBResult {
	return models.NZBResult{
		Title:       "Show.S01.1080p.WEB-DL-GRP",
		DownloadURL: downloadURL,
		ServiceType: models.ServiceTypeUsenet,
		Attributes:  map[string]string{"targetSeason": "1"},
	}
}

func TestResolveBatchUsenetSelectsEachEpisodeFromExternalSeasonPack(t *testing.T) {
	releaseDir := "/webdav/completed-symlinks/mediastorm/Show.S01.1080p.WEB-DL-GRP/"
	svc, server, downloadURL := newSeasonPackEngine(t, "nzbdav", "/api", releaseDir)

	resp, err := svc.ResolveBatch(context.Background(), seasonPackCandidate(downloadURL), []models.BatchEpisodeTarget{
		{SeasonNumber: 1, EpisodeNumber: 1},
		{SeasonNumber: 1, EpisodeNumber: 2},
		{SeasonNumber: 1, EpisodeNumber: 3},
		{SeasonNumber: 1, EpisodeNumber: 4},
	})
	if err != nil {
		t.Fatalf("ResolveBatch: %v", err)
	}
	if len(resp.Results) != 4 {
		t.Fatalf("results = %d, want 4", len(resp.Results))
	}
	for i, want := range []string{
		"Show.S01E01.1080p.WEB-DL-GRP.mkv",
		"Show.S01E02.1080p.WEB-DL-GRP.mkv",
		"Show.S01E03.1080p.WEB-DL-GRP.mkv",
	} {
		got := resp.Results[i]
		if got.Error != "" || got.Resolution == nil {
			t.Fatalf("episode %d: error=%q resolution=%v", i+1, got.Error, got.Resolution)
		}
		if wantURL := server.URL + releaseDir + want; got.Resolution.WebDAVPath != wantURL {
			t.Fatalf("episode %d: WebDAVPath = %q, want %q", i+1, got.Resolution.WebDAVPath, wantURL)
		}
	}
	missing := resp.Results[3]
	if missing.Resolution != nil || !strings.Contains(missing.Error, ErrEpisodeNotInRelease.Error()) {
		t.Fatalf("episode 4: resolution=%v error=%q, want episode-not-in-release error", missing.Resolution, missing.Error)
	}
}

func TestResolveExternalUsenetReselectsEpisodeFromCachedPackResolution(t *testing.T) {
	releaseDir := "/webdav/completed-symlinks/mediastorm/Show.S01.1080p.WEB-DL-GRP/"
	svc, server, downloadURL := newSeasonPackEngine(t, "nzbdav", "/api", releaseDir)

	settings, err := svc.cfg.Load()
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	reuseKey := externalUsenetReuseKey(settings.UsenetEngines[0], []byte(seasonPackNZB))
	e01 := server.URL + releaseDir + "Show.S01E01.1080p.WEB-DL-GRP.mkv"
	// Simulate a completed job whose file was chosen for another episode.
	svc.externalReady[reuseKey] = &externalResolvedResult{
		Resolution: models.PlaybackResolution{WebDAVPath: e01, HealthStatus: "healthy"},
		ResolvedAt: time.Now(),
	}

	candidate := withBatchEpisodeTarget(seasonPackCandidate(downloadURL), models.BatchEpisodeTarget{SeasonNumber: 1, EpisodeNumber: 2})
	res, err := svc.Resolve(context.Background(), candidate)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := server.URL + releaseDir + "Show.S01E02.1080p.WEB-DL-GRP.mkv"; res.WebDAVPath != want {
		t.Fatalf("WebDAVPath = %q, want %q", res.WebDAVPath, want)
	}
}

func TestResolveUsenetBatchStopsAfterReleaseWideFailure(t *testing.T) {
	calls := 0
	indexer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer indexer.Close()
	cfg := config.NewManager(filepath.Join(t.TempDir(), "settings.json"))
	if err := cfg.Save(config.DefaultSettings()); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	svc := NewService(cfg, nil, nil)

	resp, err := svc.ResolveBatch(context.Background(), seasonPackCandidate(indexer.URL+"/dead.nzb"), []models.BatchEpisodeTarget{
		{SeasonNumber: 1, EpisodeNumber: 1},
		{SeasonNumber: 1, EpisodeNumber: 2},
		{SeasonNumber: 1, EpisodeNumber: 3},
	})
	if err != nil {
		t.Fatalf("ResolveBatch: %v", err)
	}
	for i, r := range resp.Results {
		if r.Error == "" || r.Resolution != nil {
			t.Fatalf("episode %d: expected failure, got resolution=%v", i+1, r.Resolution)
		}
	}
	if calls == 0 || calls > firstEpisodeCalls(t, indexer.URL) {
		t.Fatalf("indexer fetched %d times; a dead release must not be refetched per episode", calls)
	}
}

func TestWithBatchEpisodeTargetDoesNotMutateCandidate(t *testing.T) {
	base := models.NZBResult{Attributes: map[string]string{"targetSeason": "1"}}
	got := withBatchEpisodeTarget(base, models.BatchEpisodeTarget{SeasonNumber: 1, EpisodeNumber: 5, AbsoluteEpisodeNumber: 30})
	if got.Attributes["targetEpisodeCode"] != "S01E05" || got.Attributes["absoluteEpisodeNumber"] != "30" {
		t.Fatalf("attributes = %v", got.Attributes)
	}
	if _, ok := base.Attributes["targetEpisode"]; ok {
		t.Fatal("base candidate attributes were mutated")
	}
}

// firstEpisodeCalls measures how many indexer fetches a single failed resolve
// makes (fetch retries), which is the most a whole failed batch may make.
func firstEpisodeCalls(t *testing.T, _ string) int {
	t.Helper()
	calls := 0
	indexer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer indexer.Close()
	cfg := config.NewManager(filepath.Join(t.TempDir(), "settings.json"))
	if err := cfg.Save(config.DefaultSettings()); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	if _, err := NewService(cfg, nil, nil).Resolve(context.Background(), seasonPackCandidate(indexer.URL+"/dead.nzb")); err == nil {
		t.Fatal("expected single resolve of dead release to fail")
	}
	return calls
}

func TestWaitForQueueItemReturnsWhenExternalJobCompletes(t *testing.T) {
	prev := usenetBatchPollInterval
	usenetBatchPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { usenetBatchPollInterval = prev })

	cfg := config.NewManager(filepath.Join(t.TempDir(), "settings.json"))
	if err := cfg.Save(config.DefaultSettings()); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	svc := NewService(cfg, nil, nil)
	queueID := externalQueueIDBase + 7
	go func() {
		time.Sleep(20 * time.Millisecond)
		svc.externalMu.Lock()
		svc.externalDone[queueID] = &externalResolvedResult{
			Resolution: models.PlaybackResolution{QueueID: queueID, WebDAVPath: "http://engine/webdav/x.mkv"},
			ResolvedAt: time.Now(),
		}
		svc.externalMu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Before completion the unknown external ID reports not-found; keep the
	// item registered as pending so the wait observes the transition.
	svc.externalMu.Lock()
	svc.externalDone[queueID] = &externalResolvedResult{Resolution: models.PlaybackResolution{QueueID: queueID}, ResolvedAt: time.Now()}
	svc.externalMu.Unlock()
	if err := svc.waitForQueueItem(ctx, queueID); err != nil {
		t.Fatalf("waitForQueueItem: %v", err)
	}
}
