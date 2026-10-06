package trakt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// DefaultListCacheTTL is how long Trakt list/watchlist fetch results are
// reused for Home shelves. Changes made directly on trakt.tv (outside this
// app) can take up to this long to appear; writes made by this app call
// InvalidateListCache so they show up immediately.
const DefaultListCacheTTL = 10 * time.Minute

// maxListCacheRetries bounds how many times a waiter re-issues a deduplicated
// fetch whose leader was cancelled while the waiter's own context is live.
const maxListCacheRetries = 2

type listCacheEntry struct {
	value      any
	expires    time.Time
	accountKey string
}

// listCache is a small TTL cache with singleflight deduplication for Trakt
// list/watchlist fetches. Keys are scoped by account identity so different
// accounts never share results.
type listCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[string]listCacheEntry
	// generation per account; bumped by invalidate so in-flight fetches that
	// started before a write do not repopulate the cache with stale data.
	generations map[string]uint64
	// inflight maps keys with a running fetch to their account key so
	// invalidate can detach new callers from a pre-write fetch.
	inflight map[string]string
	group    singleflight.Group
}

func newListCache(ttl time.Duration) *listCache {
	return &listCache{
		ttl:         ttl,
		now:         time.Now,
		entries:     make(map[string]listCacheEntry),
		generations: make(map[string]uint64),
		inflight:    make(map[string]string),
	}
}

func (lc *listCache) get(key string) (any, bool) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	e, ok := lc.entries[key]
	if !ok {
		return nil, false
	}
	if !lc.now().Before(e.expires) {
		delete(lc.entries, key)
		return nil, false
	}
	return e.value, true
}

func (lc *listCache) generation(accountKey string) uint64 {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	return lc.generations[accountKey]
}

func (lc *listCache) set(key, accountKey string, value any, gen uint64) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	if lc.generations[accountKey] != gen {
		return // invalidated while fetching
	}
	now := lc.now()
	for k, e := range lc.entries {
		if !now.Before(e.expires) {
			delete(lc.entries, k)
		}
	}
	lc.entries[key] = listCacheEntry{value: value, expires: now.Add(lc.ttl), accountKey: accountKey}
}

func (lc *listCache) invalidate(accountKey string) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	lc.generations[accountKey]++
	for k, e := range lc.entries {
		if e.accountKey == accountKey {
			delete(lc.entries, k)
		}
	}
	for k, acct := range lc.inflight {
		if acct == accountKey {
			lc.group.Forget(k)
		}
	}
}

func (lc *listCache) markInflight(key, accountKey string, running bool) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	if running {
		lc.inflight[key] = accountKey
	} else {
		delete(lc.inflight, key)
	}
}

func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// cachedListFetch returns a cached result for key or runs fetch, deduplicating
// concurrent identical fetches. The returned slice is always a fresh copy.
// The fetch runs with the context of the caller that started it; a waiter
// stops waiting when its own ctx is cancelled, and if the leader was cancelled
// a still-live waiter retries the fetch itself.
func cachedListFetch[T any](ctx context.Context, lc *listCache, accountKey, key string, fetch func(context.Context) ([]T, error), clone func([]T) []T) ([]T, error) {
	if lc == nil {
		items, err := fetch(ctx)
		if err != nil {
			return nil, err
		}
		return clone(items), nil
	}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if v, ok := lc.get(key); ok {
			return clone(v.([]T)), nil
		}
		gen := lc.generation(accountKey)
		ch := lc.group.DoChan(key, func() (any, error) {
			lc.markInflight(key, accountKey, true)
			defer lc.markInflight(key, accountKey, false)
			items, err := fetch(ctx)
			if err != nil {
				return nil, err
			}
			lc.set(key, accountKey, items, gen)
			return items, nil
		})
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case res := <-ch:
			if res.Err != nil {
				if isContextErr(res.Err) && ctx.Err() == nil && attempt < maxListCacheRetries {
					continue
				}
				return nil, res.Err
			}
			return clone(res.Val.([]T)), nil
		}
	}
}

// listCacheAccountKey scopes cache entries to a Trakt account. When no stable
// account ID is available the access token (hashed) identifies the account.
func listCacheAccountKey(accountID, accessToken string) string {
	if id := strings.TrimSpace(accountID); id != "" {
		return "acct:" + id
	}
	sum := sha256.Sum256([]byte(accessToken))
	return "tok:" + hex.EncodeToString(sum[:8])
}

func cloneMovie(m *Movie) *Movie {
	if m == nil {
		return nil
	}
	cp := *m
	return &cp
}

func cloneShow(s *Show) *Show {
	if s == nil {
		return nil
	}
	cp := *s
	return &cp
}

func cloneWatchlistItems(items []WatchlistItem) []WatchlistItem {
	if items == nil {
		return nil
	}
	out := make([]WatchlistItem, len(items))
	for i, it := range items {
		it.Movie = cloneMovie(it.Movie)
		it.Show = cloneShow(it.Show)
		out[i] = it
	}
	return out
}

func cloneListItems(items []ListItem) []ListItem {
	if items == nil {
		return nil
	}
	out := make([]ListItem, len(items))
	for i, it := range items {
		it.Movie = cloneMovie(it.Movie)
		it.Show = cloneShow(it.Show)
		out[i] = it
	}
	return out
}

func (c *Client) listCache() *listCache {
	if c == nil || c.shared == nil {
		return nil
	}
	return c.shared.lists
}

// GetAllWatchlistCached returns the account's full watchlist, served from a
// short TTL cache shared by all clients derived from the same base client.
// accountID scopes the cache entry (falls back to the access token when empty).
func (c *Client) GetAllWatchlistCached(ctx context.Context, accountID, accessToken string) ([]WatchlistItem, error) {
	acct := listCacheAccountKey(accountID, accessToken)
	return cachedListFetch(ctx, c.listCache(), acct, acct+"|watchlist", func(ctx context.Context) ([]WatchlistItem, error) {
		return c.GetAllWatchlistCtx(ctx, accessToken)
	}, cloneWatchlistItems)
}

// GetAllListItemsCached returns all items of a custom/smart list, served from
// a short TTL cache scoped to the account and list ID.
func (c *Client) GetAllListItemsCached(ctx context.Context, accountID, accessToken, listID string) ([]ListItem, error) {
	acct := listCacheAccountKey(accountID, accessToken)
	return cachedListFetch(ctx, c.listCache(), acct, acct+"|list|"+listID, func(ctx context.Context) ([]ListItem, error) {
		return c.GetAllListItemsCtx(ctx, accessToken, listID)
	}, cloneListItems)
}

// InvalidateListCache drops all cached list/watchlist results for the account
// (by account ID). Call after this app mutates the account's Trakt lists.
func (c *Client) InvalidateListCache(accountID string) {
	if lc := c.listCache(); lc != nil {
		lc.invalidate(listCacheAccountKey(accountID, ""))
	}
}
