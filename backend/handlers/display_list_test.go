package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"novastream/config"
	"novastream/handlers"
	"novastream/models"
	"novastream/services/customlists"
	"novastream/services/playback"
	"novastream/services/users"
	"novastream/services/watchlist"
	"path/filepath"

	"github.com/gorilla/mux"
)

type displayListPrequeueStore struct {
	entries []*playback.PrequeueEntry
}

func (s displayListPrequeueStore) ListAll() []*playback.PrequeueEntry { return s.entries }

func TestDisplayListWatchlist(t *testing.T) {
	dir := t.TempDir()
	wl, err := watchlist.NewService(dir)
	if err != nil {
		t.Fatalf("failed to create watchlist service: %v", err)
	}
	custom, err := customlists.NewService(dir)
	if err != nil {
		t.Fatalf("failed to create custom list service: %v", err)
	}
	userSvc, err := users.NewService(dir)
	if err != nil {
		t.Fatalf("failed to create users service: %v", err)
	}
	userID := userSvc.ListAll()[0].ID

	if _, err := wl.AddOrUpdate(userID, models.WatchlistUpsert{ID: "m1", MediaType: "movie", Name: "Sample"}); err != nil {
		t.Fatalf("failed to seed watchlist: %v", err)
	}

	h := handlers.NewDisplayListHandler(wl, custom, userSvc)
	req := httptest.NewRequest(http.MethodGet, "/api/users/"+userID+"/display-list?source=watchlist", nil)
	req = mux.SetURLVars(req, map[string]string{"userID": userID})
	rec := httptest.NewRecorder()

	h.Get(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Source string                 `json:"source"`
		Items  []models.WatchlistItem `json:"items"`
		Total  int                    `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Source != "watchlist" || resp.Total != 1 || len(resp.Items) != 1 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.Items[0].Name != "Sample" {
		t.Fatalf("unexpected item: %+v", resp.Items[0])
	}
}

func TestDisplayListPermanentPrequeueIsProfileScopedAndPersistentOnly(t *testing.T) {
	now := time.Now()
	h := handlers.NewDisplayListHandler(nil, nil, nil)
	h.SetPrequeueStore(displayListPrequeueStore{entries: []*playback.PrequeueEntry{
		{TitleID: "tmdb:movie:1", TitleName: "Older", MediaType: "movie", UserID: "profile-1", Persistent: true, CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)},
		{TitleID: "tvdb:series:2", TitleName: "Newer", MediaType: "series", UserID: "profile-1", Persistent: true, CreatedAt: now, ExpiresAt: now.Add(time.Hour)},
		{TitleID: "tmdb:movie:3", TitleName: "Temporary", MediaType: "movie", UserID: "profile-1", CreatedAt: now, ExpiresAt: now.Add(time.Hour)},
		{TitleID: "tmdb:movie:4", TitleName: "Other Profile", MediaType: "movie", UserID: "profile-2", Persistent: true, CreatedAt: now, ExpiresAt: now.Add(time.Hour)},
	}})
	req := httptest.NewRequest(http.MethodGet, "/api/users/profile-1/display-list?source=permanent-prequeue", nil)
	req = mux.SetURLVars(req, map[string]string{"userID": "profile-1"})
	rec := httptest.NewRecorder()

	h.Get(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Items []models.TrendingItem `json:"items"`
		Total int                   `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Total != 2 || len(resp.Items) != 2 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.Items[0].Title.Name != "Newer" || resp.Items[0].Title.TVDBID != 2 {
		t.Fatalf("newest persistent item was not first: %+v", resp.Items[0])
	}
	if resp.Items[1].Title.Name != "Older" || resp.Items[1].Title.TMDBID != 1 {
		t.Fatalf("unexpected older item: %+v", resp.Items[1])
	}
}

func TestDisplayListCustomListRequiresListID(t *testing.T) {
	dir := t.TempDir()
	wl, err := watchlist.NewService(dir)
	if err != nil {
		t.Fatalf("failed to create watchlist service: %v", err)
	}
	custom, err := customlists.NewService(dir)
	if err != nil {
		t.Fatalf("failed to create custom list service: %v", err)
	}
	userSvc, err := users.NewService(dir)
	if err != nil {
		t.Fatalf("failed to create users service: %v", err)
	}
	userID := userSvc.ListAll()[0].ID

	h := handlers.NewDisplayListHandler(wl, custom, userSvc)
	req := httptest.NewRequest(http.MethodGet, "/api/users/"+userID+"/display-list?source=custom-list", nil)
	req = mux.SetURLVars(req, map[string]string{"userID": userID})
	rec := httptest.NewRecorder()

	h.Get(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDisplayListHomeViewFiltersBeforePagination(t *testing.T) {
	dir := t.TempDir()
	wl, err := watchlist.NewService(dir)
	if err != nil {
		t.Fatal(err)
	}
	custom, err := customlists.NewService(dir)
	if err != nil {
		t.Fatal(err)
	}
	userSvc, err := users.NewService(dir)
	if err != nil {
		t.Fatal(err)
	}
	userID := userSvc.ListAll()[0].ID
	for _, item := range []models.WatchlistUpsert{
		{ID: "m1", MediaType: "movie", Name: "A"}, {ID: "s1", MediaType: "series", Name: "B"},
		{ID: "m2", MediaType: "movie", Name: "C"}, {ID: "s2", MediaType: "series", Name: "D"},
		{ID: "m3", MediaType: "movie", Name: "E"},
	} {
		if _, err := wl.AddOrUpdate(userID, item); err != nil {
			t.Fatal(err)
		}
	}
	h := handlers.NewDisplayListHandler(wl, custom, userSvc)
	manager := config.NewManager(filepath.Join(t.TempDir(), "settings.json"))
	if err := manager.Save(config.Settings{HomeShelves: config.HomeShelvesSettings{Views: map[string]config.HomeViewSettings{
		"page-films":  {Mode: "inherit", MediaFilter: "movies"},
		"page-series": {Mode: "custom", MediaFilter: "shows"},
	}}}); err != nil {
		t.Fatal(err)
	}
	h.SetMetadataHandler(handlers.NewMetadataHandler(nil, manager))
	for _, view := range []string{"movies", "shows", "page-films", "page-series"} {
		req := httptest.NewRequest(http.MethodGet, "/display-list?source=watchlist&homeView="+view+"&limit=1&offset=1", nil)
		req = mux.SetURLVars(req, map[string]string{"userID": userID})
		rec := httptest.NewRecorder()
		h.Get(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", view, rec.Code, rec.Body.String())
		}
		var got struct {
			Items []models.WatchlistItem `json:"items"`
			Total int                    `json:"total"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		total, kind := 3, "movie"
		if view == "shows" || view == "page-series" {
			total, kind = 2, "series"
		}
		if got.Total != total || len(got.Items) != 1 || got.Items[0].MediaType != kind {
			t.Fatalf("%s: %+v", view, got)
		}
	}
}

func TestDisplayListCustomListSortsAndFiltersBeforePagination(t *testing.T) {
	dir := t.TempDir()
	wl, err := watchlist.NewService(dir)
	if err != nil {
		t.Fatal(err)
	}
	custom, err := customlists.NewService(dir)
	if err != nil {
		t.Fatal(err)
	}
	userSvc, err := users.NewService(dir)
	if err != nil {
		t.Fatal(err)
	}
	userID := userSvc.ListAll()[0].ID
	list, err := custom.CreateList(userID, "Mixed")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []models.WatchlistUpsert{
		{ID: "m1", MediaType: "movie", Name: "Short", RuntimeMinutes: 80, Genres: []string{"Comedy"}},
		{ID: "m2", MediaType: "movie", Name: "Epic", RuntimeMinutes: 200, Genres: []string{"Drama"}},
		{ID: "m3", MediaType: "movie", Name: "Medium", RuntimeMinutes: 120, Genres: []string{"Drama", "Comedy"}},
		{ID: "m4", MediaType: "movie", Name: "Long", RuntimeMinutes: 150, Genres: []string{"Drama"}},
		{ID: "m5", MediaType: "movie", Name: "Unknown"},
	} {
		if _, err := custom.AddItem(userID, list.ID, item); err != nil {
			t.Fatal(err)
		}
	}
	h := handlers.NewDisplayListHandler(wl, custom, userSvc)

	get := func(query string) (names []string, total int) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/display-list?source=custom-list&listId="+list.ID+"&"+query, nil)
		req = mux.SetURLVars(req, map[string]string{"userID": userID})
		rec := httptest.NewRecorder()
		h.Get(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", query, rec.Code, rec.Body.String())
		}
		var got struct {
			Items  []models.WatchlistItem `json:"items"`
			Total  int                    `json:"total"`
			Genres []string               `json:"genres"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Genres) != 2 {
			t.Fatalf("%s: facets must cover the unfiltered list, got %v", query, got.Genres)
		}
		for _, item := range got.Items {
			names = append(names, item.Name)
		}
		return names, got.Total
	}

	// Duration sort is applied to the whole list before the page is cut.
	names, total := get("sortBy=duration&sortDirection=desc&limit=2&offset=1")
	if total != 5 || len(names) != 2 || names[0] != "Long" || names[1] != "Medium" {
		t.Fatalf("duration page: total=%d names=%v", total, names)
	}

	// Genre filtering narrows the total that drives pagination.
	names, total = get("genres=drama&sortBy=duration&sortDirection=asc&limit=2&offset=0")
	if total != 3 || len(names) != 2 || names[0] != "Medium" || names[1] != "Long" {
		t.Fatalf("genre page: total=%d names=%v", total, names)
	}

	// Omitting the query keeps the stored (newest-first) order and full total.
	names, total = get("")
	if total != 5 || len(names) != 5 || names[0] != "Unknown" {
		t.Fatalf("default order: total=%d names=%v", total, names)
	}
}
