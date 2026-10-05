package remotemedia

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"novastream/config"
	"novastream/models"
	"novastream/services/plex"
)

type playbackUsers map[string]models.User

func (u playbackUsers) Get(id string) (models.User, bool) {
	user, ok := u[id]
	return user, ok
}

type playbackTransport func(*http.Request) (*http.Response, error)

func (f playbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func newPlexPlaybackTestReporter(t *testing.T, transport playbackTransport) *PlaybackReporter {
	t.Helper()
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	cfg := config.NewManager(filepath.Join(t.TempDir(), "settings.json"))
	settings, err := cfg.Load()
	if err != nil {
		t.Fatal(err)
	}
	settings.Plex.Accounts = []config.PlexAccount{
		{ID: "owner", AuthToken: "owner-auth"},
		{ID: "plex-a", AuthToken: "auth-a"},
		{ID: "plex-b", AuthToken: "auth-b"},
		{ID: "disconnected"},
	}
	if err := cfg.Save(settings); err != nil {
		t.Fatal(err)
	}
	library := models.RemoteMediaLibrary{
		ID: "library", Provider: models.MediaSourcePlex, AccountID: "owner",
		ServerID: "server", ServerURL: "http://plex.test:32400",
	}
	repo := &fakeRemoteMediaRepo{
		libraries: []models.RemoteMediaLibrary{library},
		items: map[string][]models.RemoteMediaItem{"library": {
			{ID: "item", LibraryID: "library", ExternalItemID: "42"},
		}},
	}
	reporter := NewPlaybackReporter(&Service{repo: repo, cfg: cfg, plex: plex.NewClient("test-client")})
	reporter.SetUserService(playbackUsers{
		"a":            {PlexAccountID: "plex-a"},
		"b":            {PlexAccountID: "plex-b"},
		"owner":        {PlexAccountID: "owner"},
		"unlinked":     {},
		"deleted":      {PlexAccountID: "missing-account"},
		"disconnected": {PlexAccountID: "disconnected"},
	})
	return reporter
}

func playbackResponse(req *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(body)), Request: req,
	}
}

func TestPlexPlaybackUsesProfileAccountForEntireSession(t *testing.T) {
	var timelines []*http.Request
	resourceLoads := map[string]int{}
	reporter := newPlexPlaybackTestReporter(t, func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/v2/resources":
			auth := req.Header.Get("X-Plex-Token")
			resourceLoads[auth]++
			body, err := json.Marshal([]plex.PlexResource{{
				ClientIdentifier: "server", Provides: "server", Presence: true,
				AccessToken: "server-" + auth,
				Connections: []plex.PlexConnection{{Protocol: "http", URI: "http://advertised.test:32400"}},
			}})
			if err != nil {
				return nil, err
			}
			return playbackResponse(req, string(body)), nil
		case "/:/timeline":
			timelines = append(timelines, req)
			return playbackResponse(req, ""), nil
		default:
			return nil, fmt.Errorf("unexpected request: %s", req.URL)
		}
	})
	update := models.PlaybackProgressUpdate{SourcePath: "plexmedia:item", Position: 12, Duration: 120}
	for _, userID := range []string{"a", "b", "owner"} {
		reporter.HandleProgressUpdate(userID, update, 10)
		reporter.HandleProgressUpdate(userID, update, 10) // unchanged state is throttled
		paused := update
		paused.IsPaused = true
		reporter.HandleProgressUpdate(userID, paused, 10)
		reporter.StopSession(userID, update, 10)
	}
	if len(timelines) != 9 {
		t.Fatalf("timeline reports = %d, want 9", len(timelines))
	}
	for i, auth := range []string{"auth-a", "auth-b", "owner-auth"} {
		if resourceLoads[auth] != 1 {
			t.Errorf("resource loads for %q = %d, want 1", auth, resourceLoads[auth])
		}
		sessionID := ""
		for j, state := range []string{"playing", "paused", "stopped"} {
			req := timelines[i*3+j]
			if got := req.Header.Get("X-Plex-Token"); got != "server-"+auth {
				t.Errorf("profile %d %s token = %q", i, state, got)
			}
			if req.Method != http.MethodPost || req.URL.Host != "plex.test:32400" {
				t.Errorf("timeline target = %s %s", req.Method, req.URL)
			}
			params := req.URL.Query()
			if params.Get("state") != state || params.Get("ratingKey") != "42" || params.Get("time") != "12000" || params.Get("duration") != "120000" {
				t.Errorf("unexpected timeline parameters: %s", params.Encode())
			}
			gotSessionID := req.Header.Get("X-Plex-Session-Identifier")
			if j == 0 {
				sessionID = gotSessionID
			}
			if gotSessionID == "" || gotSessionID != sessionID {
				t.Error("session identifier changed within profile playback")
			}
		}
		if i > 0 && sessionID == timelines[(i-1)*3].Header.Get("X-Plex-Session-Identifier") {
			t.Error("different profiles shared a playback session identifier")
		}
	}
	if got := reporter.service.repo.(*fakeRemoteMediaRepo).libraries[0].AccountID; got != "owner" {
		t.Errorf("source library account was mutated to %q", got)
	}
}

func TestPlexPlaybackWithoutConnectedProfileNeverUsesLibraryOwner(t *testing.T) {
	requests := 0
	reporter := newPlexPlaybackTestReporter(t, func(req *http.Request) (*http.Response, error) {
		requests++
		return nil, fmt.Errorf("unexpected request: %s", req.URL)
	})
	update := models.PlaybackProgressUpdate{SourcePath: "plexmedia:item", Position: 12, Duration: 120}
	for _, id := range []string{"unlinked", "deleted", "disconnected", "missing-user"} {
		reporter.HandleProgressUpdate(id, update, 10)
		reporter.StopSession(id, update, 10)
	}
	reporter.SetUserService(nil)
	reporter.HandleProgressUpdate("a", update, 10)
	if requests != 0 {
		t.Fatalf("requests = %d, want no Plex requests", requests)
	}
}

func TestPlexPlaybackUnavailableToProfileNeverFallsBackToOwner(t *testing.T) {
	resourceLoads := 0
	reporter := newPlexPlaybackTestReporter(t, func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/api/v2/resources" || req.Header.Get("X-Plex-Token") != "auth-a" {
			t.Errorf("unexpected request: %s", req.URL)
		}
		resourceLoads++
		return playbackResponse(req, "[]"), nil
	})
	// Even if the library owner's server is cached, the viewing account must
	// have its own access to that server before a timeline can be sent.
	reporter.service.servers.entries = map[string]plexServerCacheEntry{
		"owner\x00server": {
			server:    plex.PlexResource{ClientIdentifier: "server", AccessToken: "owner-server"},
			authToken: "owner-auth", expiresAt: time.Now().Add(time.Minute),
		},
	}
	library, _ := reporter.service.repo.GetLibrary(context.Background(), "library")
	item, _ := reporter.service.repo.GetItem(context.Background(), "item")
	err := reporter.reportToProvider(context.Background(), "a", library, item, "session", "playing", "", true, models.PlaybackProgressUpdate{})
	if err == nil || resourceLoads != 1 {
		t.Fatalf("unavailable server: error = %v, resource loads = %d", err, resourceLoads)
	}
}

func TestPlexServerResolverDoesNotReuseRevokedServer(t *testing.T) {
	for _, changedToken := range []bool{false, true} {
		t.Run(fmt.Sprintf("changed-token-%t", changedToken), func(t *testing.T) {
			now := time.Now()
			resolver := &plexServerResolver{now: func() time.Time { return now }}
			load := func(string) ([]plex.PlexResource, error) {
				return []plex.PlexResource{{ClientIdentifier: "server", AccessToken: "old-server-token"}}, nil
			}
			if _, err := resolver.resolve("account", "auth", "server", load); err != nil {
				t.Fatal(err)
			}
			token := "auth"
			if changedToken {
				token = "new-auth"
			} else {
				now = now.Add(plexServerCacheTTL)
			}
			_, err := resolver.resolve("account", token, "server", func(string) ([]plex.PlexResource, error) { return nil, nil })
			if err == nil {
				t.Fatal("revoked server was returned from stale cache")
			}
		})
	}
}
