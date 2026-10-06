package handlers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"novastream/config"
	"novastream/services/trakt"
)

// TestFetchTraktShelfItems_ConcurrentAccountsIsolated loads shelves for two
// different Trakt accounts concurrently on one shared client and verifies each
// request uses its own account's app key + token and gets its own data.
func TestFetchTraktShelfItems_ConcurrentAccountsIsolated(t *testing.T) {
	origURL := trakt.GetBaseURLForTest()
	defer trakt.SetBaseURLForTest(origURL)
	trakt.SetBaseURLForTest("https://trakt.test")

	accounts := map[string]config.TraktAccount{
		"A": {ID: "trakt-A", Name: "A", ClientID: "cid-A", ClientSecret: "sec-A", AccessToken: "tok-A", ExpiresAt: time.Now().Add(24 * time.Hour).Unix()},
		"B": {ID: "trakt-B", Name: "B", ClientID: "cid-B", ClientSecret: "sec-B", AccessToken: "tok-B", ExpiresAt: time.Now().Add(24 * time.Hour).Unix()},
	}
	imdbFor := map[string]string{"A": "tt0000001", "B": "tt0000002"}

	var mismatches atomic.Int32
	traktClient := trakt.NewClient("", "")
	traktClient.SetHTTPClientForTest(&http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			who := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer tok-")
			acc, ok := accounts[who]
			if !ok || r.Header.Get("trakt-api-key") != acc.ClientID {
				mismatches.Add(1)
				return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("mismatch"))}, nil
			}
			// Small delay widens the race window between shelves.
			time.Sleep(2 * time.Millisecond)
			body := fmt.Sprintf(`[{"type":"movie","listed_at":"2024-01-01T00:00:00Z","movie":{"title":"Movie %s","year":2020,"ids":{"imdb":"%s"}}}]`, who, imdbFor[who])
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"X-Pagination-Item-Count": []string{"1"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}),
	})

	handler := NewMetadataHandler(&fakeMetadataService{}, testConfigManager(t))
	handler.SetTraktClient(traktClient)

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 32; i++ {
		who := "A"
		if i%2 == 1 {
			who = "B"
		}
		listType, listID := "watchlist", ""
		if i%4 >= 2 {
			listType, listID = "custom", "my-list"
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			items, err := handler.fetchTraktShelfItems(context.Background(), config.Settings{}, []config.TraktAccount{accounts[who]}, listType, listID)
			if err != nil {
				errs <- fmt.Errorf("account %s: %w", who, err)
				return
			}
			if len(items) != 1 || items[0].IMDBID != imdbFor[who] {
				errs <- fmt.Errorf("account %s got wrong items: %+v", who, items)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := mismatches.Load(); n != 0 {
		t.Fatalf("%d requests mixed one account's app key with another's token", n)
	}

	// Multi-account ("__all__") fetch merges both accounts' items.
	items, err := handler.fetchTraktShelfItems(context.Background(), config.Settings{}, []config.TraktAccount{accounts["A"], accounts["B"]}, "watchlist", "")
	if err != nil {
		t.Fatalf("multi-account fetch: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 merged items, got %+v", items)
	}
}
