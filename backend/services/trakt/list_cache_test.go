package trakt

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newWatchlistServer serves one-item-per-page watchlists keyed on the bearer
// token; totalPages controls pagination. It counts requests.
func newWatchlistServer(t *testing.T, totalPages int, delay time.Duration, hits *atomic.Int32, onRequest func()) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if onRequest != nil {
			onRequest()
		}
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		who := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		page := r.URL.Query().Get("page")
		w.Header().Set("X-Pagination-Item-Count", fmt.Sprint(totalPages))
		fmt.Fprintf(w, `[{"type":"movie","listed_at":"2024-01-01T00:00:00Z","movie":{"title":"%s-p%s","year":2020,"ids":{"trakt":1}}}]`, who, page)
	}))
	t.Cleanup(srv.Close)
	orig := traktAPIBaseURL
	setBaseURL(srv.URL)
	t.Cleanup(func() { setBaseURL(orig) })
	return srv
}

func TestListCache_HitAndTTLExpiry(t *testing.T) {
	var hits atomic.Int32
	newWatchlistServer(t, 1, 0, &hits, nil)

	c := NewClient("cid", "sec")
	now := time.Unix(1_700_000_000, 0)
	var clockMu sync.Mutex
	c.shared.lists.now = func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
	advance := func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() }

	ctx := context.Background()
	first, err := c.GetAllWatchlistCached(ctx, "acct-1", "tok")
	if err != nil || len(first) != 1 {
		t.Fatalf("first fetch: %v %+v", err, first)
	}
	// Mutating the returned copy must not affect the cache.
	first[0].Movie.Title = "mutated"

	second, err := c.GetAllWatchlistCached(ctx, "acct-1", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected cache hit, got %d requests", hits.Load())
	}
	if second[0].Movie.Title != "tok-p1" {
		t.Fatalf("cached value was mutated through returned copy: %q", second[0].Movie.Title)
	}

	// Derived per-account clients share the cache.
	if _, err := c.WithCredentials("other", "x").GetAllWatchlistCached(ctx, "acct-1", "tok"); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("derived client should share cache, got %d requests", hits.Load())
	}

	// A different account is never served another account's entry.
	other, err := c.GetAllWatchlistCached(ctx, "acct-2", "tok2")
	if err != nil || other[0].Movie.Title != "tok2-p1" || hits.Load() != 2 {
		t.Fatalf("acct-2 fetch: err=%v items=%+v hits=%d", err, other, hits.Load())
	}

	advance(DefaultListCacheTTL + time.Second)
	if _, err := c.GetAllWatchlistCached(ctx, "acct-1", "tok"); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 3 {
		t.Fatalf("expected refetch after TTL expiry, got %d requests", hits.Load())
	}
}

func TestListCache_InvalidateAccount(t *testing.T) {
	var hits atomic.Int32
	newWatchlistServer(t, 1, 0, &hits, nil)
	c := NewClient("cid", "sec")
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := c.GetAllWatchlistCached(ctx, "acct-1", "tok"); err != nil {
			t.Fatal(err)
		}
		if _, err := c.GetAllListItemsCached(ctx, "acct-2", "tok2", "list"); err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 2 {
		t.Fatalf("expected 2 requests, got %d", hits.Load())
	}
	c.InvalidateListCache("acct-1")
	if _, err := c.GetAllWatchlistCached(ctx, "acct-1", "tok"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetAllListItemsCached(ctx, "acct-2", "tok2", "list"); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 3 {
		t.Fatalf("expected only acct-1 to refetch, got %d requests", hits.Load())
	}
}

func TestListCache_DeduplicatesConcurrentFetches(t *testing.T) {
	var hits atomic.Int32
	newWatchlistServer(t, 3, 30*time.Millisecond, &hits, nil)
	c := NewClient("cid", "sec")

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			items, err := c.WithCredentials("cid", "sec").GetAllWatchlistCached(context.Background(), "acct-1", "tok")
			if err != nil {
				errs <- err
				return
			}
			if len(items) != 3 {
				errs <- fmt.Errorf("expected 3 items, got %d", len(items))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("expected one 3-page fetch (3 requests), got %d", got)
	}
}

func TestListCache_ContextCancellationStopsPagination(t *testing.T) {
	var hits atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel as soon as the second page is requested.
	newWatchlistServer(t, 50, 0, &hits, func() {
		if hits.Load() == 2 {
			cancel()
		}
	})
	c := NewClient("cid", "sec")

	_, err := c.GetAllWatchlistCached(ctx, "acct-1", "tok")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if got := hits.Load(); got > 3 {
		t.Fatalf("pagination continued after cancellation: %d requests", got)
	}

	// Errors (including cancellation) are not cached.
	hitsBefore := hits.Load()
	if _, err := c.GetAllWatchlistCached(context.Background(), "acct-1", "tok"); err != nil {
		t.Fatalf("fresh fetch: %v", err)
	}
	if hits.Load() <= hitsBefore {
		t.Fatalf("expected new requests after cancelled fetch")
	}
}

func TestListCache_WaiterSurvivesLeaderCancellation(t *testing.T) {
	var hits atomic.Int32
	newWatchlistServer(t, 1, 50*time.Millisecond, &hits, nil)
	c := NewClient("cid", "sec")

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := c.GetAllWatchlistCached(leaderCtx, "acct-1", "tok")
		leaderDone <- err
	}()
	// Let the leader start its fetch, then join as a waiter and cancel the leader.
	for hits.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	waiterDone := make(chan error, 1)
	go func() {
		items, err := c.GetAllWatchlistCached(context.Background(), "acct-1", "tok")
		if err == nil && len(items) != 1 {
			err = fmt.Errorf("unexpected items %+v", items)
		}
		waiterDone <- err
	}()
	time.Sleep(5 * time.Millisecond)
	cancelLeader()

	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader: expected cancellation, got %v", err)
	}
	if err := <-waiterDone; err != nil {
		t.Fatalf("waiter should retry after leader cancellation, got %v", err)
	}
}

func TestWithCredentials_DoesNotMutateParent(t *testing.T) {
	base := NewClient("base-id", "base-secret")
	derived := base.WithCredentials("acct-id", "acct-secret")
	if id, secret := base.credentials(); id != "base-id" || secret != "base-secret" {
		t.Fatalf("parent credentials changed: %s %s", id, secret)
	}
	if id, _ := derived.credentials(); id != "acct-id" {
		t.Fatalf("derived credentials wrong: %s", id)
	}
	if derived.shared != base.shared || derived.httpClient != base.httpClient {
		t.Fatal("derived client should share http client and shared state")
	}
	if base.getRefreshMu("x") != derived.getRefreshMu("x") {
		t.Fatal("derived client must share per-account refresh locks")
	}
}
